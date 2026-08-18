// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package coordinator is LoadWave's control plane.
//
// It holds the fleet together: agents join it, it decides how a run's load is
// divided between them, it merges everything they report into one coherent
// picture, and it serves that picture to the CLI and the dashboard. It is the
// only component that sees the whole run, which is why threshold evaluation
// and the pass/fail verdict live here rather than anywhere further down.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/apportion"
	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/internal/control"
	"github.com/SnowyFoxStudios/LoadWave/internal/engine"
	"github.com/SnowyFoxStudios/LoadWave/internal/idspace"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
)

// Config describes a coordinator.
type Config struct {
	// ListenAddr is where agents connect. ":0" picks a free port, which is
	// what the single-process `loadwave run` path uses.
	ListenAddr string

	Logger *slog.Logger

	// HeartbeatInterval is how often agents should report liveness.
	HeartbeatInterval time.Duration

	// MetricsInterval is the reporting period, and therefore the width of a
	// chart bucket.
	MetricsInterval time.Duration

	// AgentTimeout is how long an agent may go unheard from before it is
	// treated as gone. Generous relative to the heartbeat, so that one
	// dropped packet does not evict a healthy agent mid-run.
	AgentTimeout time.Duration

	// StartDelay is the lead time built into a run's agreed start instant, so
	// that every agent has received its orders before the clock starts.
	StartDelay time.Duration

	// Store configures each run's metric store.
	Store metrics.StoreConfig

	// MaxRunHistory is how many finished runs are retained in memory.
	MaxRunHistory int
}

// Defaults for a zero Config.
const (
	DefaultAgentTimeout  = 15 * time.Second
	DefaultStartDelay    = 2 * time.Second
	DefaultMaxRunHistory = 50
	maxGlobalEvents      = 500

	// stopTimeoutMargin is added to a plan's grace budget before the
	// coordinator gives up waiting for agents to confirm a stop.
	stopTimeoutMargin = 20 * time.Second
)

func (c *Config) applyDefaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = control.DefaultServerHeartbeatInterval
	}
	if c.MetricsInterval <= 0 {
		c.MetricsInterval = control.DefaultMetricsInterval
	}
	if c.AgentTimeout <= 0 {
		c.AgentTimeout = DefaultAgentTimeout
	}
	if c.StartDelay <= 0 {
		c.StartDelay = DefaultStartDelay
	}
	if c.MaxRunHistory <= 0 {
		c.MaxRunHistory = DefaultMaxRunHistory
	}
	if c.Store.Resolution <= 0 {
		c.Store.Resolution = c.MetricsInterval
	}
}

// AgentInfo is the operator-facing view of a connected agent.
type AgentInfo struct {
	ID             string            `json:"id"`
	Hostname       string            `json:"hostname"`
	Version        string            `json:"version"`
	Cores          uint32            `json:"cores"`
	MaxWorkers     uint32            `json:"maxWorkers"`
	MaxVUs         uint32            `json:"maxVUs"`
	Labels         map[string]string `json:"labels,omitempty"`
	RemoteAddr     string            `json:"remoteAddr"`
	JoinedAt       time.Time         `json:"joinedAt"`
	LastSeen       time.Time         `json:"lastSeen"`
	ActiveVUs      uint32            `json:"activeVUs"`
	HealthyWorkers uint32            `json:"healthyWorkers"`
	Healthy        bool              `json:"healthy"`
	VUQuota        int               `json:"vuQuota"`

	// CPUPercent and MemBytes describe the agent process itself — its
	// supervisory footprint, not the workers it spawns. Zero until its
	// first heartbeat arrives.
	CPUPercent float64 `json:"cpuPercent"`
	MemBytes   uint64  `json:"memBytes"`

	// Workers is the per-process breakdown within this agent. Never nil:
	// this is JSON-encoded straight to the dashboard, which maps over it
	// unconditionally.
	Workers []WorkerInfo `json:"workers"`
}

// WorkerInfo is one worker process's resource usage, as its agent reported
// it — the detail an agent-level aggregate would hide, such as one process
// starved for CPU while its siblings on the same host are not.
type WorkerInfo struct {
	ID         string  `json:"id"`
	Index      uint32  `json:"index"`
	ActiveVUs  uint32  `json:"activeVUs"`
	CPUPercent float64 `json:"cpuPercent"`
	MemBytes   uint64  `json:"memBytes"`
}

// Coordinator is the control plane.
type Coordinator struct {
	cfg Config
	log *slog.Logger

	sessions   *control.SessionRegistry
	grpcServer *grpc.Server
	addr       string
	addrReady  chan struct{}

	mu     sync.RWMutex
	agents map[string]*AgentInfo
	runs   map[string]*Run
	order  []string
	active *Run
	events []Event

	subs subscriberSet
}

// New prepares a coordinator. It does not listen; Run does that.
func New(cfg Config) (*Coordinator, error) {
	cfg.applyDefaults()
	return &Coordinator{
		cfg:       cfg,
		log:       cfg.Logger.With("component", "coordinator"),
		sessions:  control.NewSessionRegistry(),
		addrReady: make(chan struct{}),
		agents:    make(map[string]*AgentInfo),
		runs:      make(map[string]*Run),
	}, nil
}

// Run listens for agents and serves until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", c.cfg.ListenAddr, err)
	}
	c.addr = listener.Addr().String()
	close(c.addrReady)

	server, err := control.NewServer(control.ServerConfig{
		Handler:           c,
		Logger:            c.log,
		Version:           buildinfo.Version(),
		HeartbeatInterval: c.cfg.HeartbeatInterval,
		MetricsInterval:   c.cfg.MetricsInterval,
	})
	if err != nil {
		return err
	}

	c.grpcServer = grpc.NewServer()
	loadwavev1.RegisterControlServiceServer(c.grpcServer, server)

	c.log.Info("coordinator listening for agents", "addr", c.addr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- c.grpcServer.Serve(listener) }()

	ticker := time.NewTicker(c.cfg.MetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.grpcServer.GracefulStop()
			c.subs.closeAll()
			return nil
		case err := <-serveErr:
			c.subs.closeAll()
			return err
		case now := <-ticker.C:
			c.tick(now)
		}
	}
}

// Addr returns the address agents should dial, once the listener is up.
func (c *Coordinator) Addr() string {
	<-c.addrReady
	return c.addr
}

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// StartRun distributes a test plan across the connected agents.
//
// Only one run is permitted at a time. Concurrent runs would have to share the
// same worker processes and the same network capacity, and the resulting
// numbers would measure the interference rather than the system under test.
// sourcePath is the configuration file cfg was loaded from, or empty when
// there isn't one — a Go-defined scenario, a quick-check built from flags, or
// a configuration submitted to the dashboard directly.
func (c *Coordinator) StartRun(cfg *scenario.Config, sourcePath string) (*Run, error) {
	plan, err := cfg.Plan()
	if err != nil {
		return nil, err
	}

	executor, err := engine.NewExecutor(plan.GetLoad())
	if err != nil {
		return nil, err
	}

	sessions := c.healthySessions()
	if len(sessions) == 0 {
		return nil, errors.New("no agents are connected; start one with `loadwave agent`")
	}

	c.mu.Lock()
	if c.active != nil && c.active.Active() {
		id := c.active.ID()
		c.mu.Unlock()
		return nil, fmt.Errorf("run %s is already in progress; stop it first", id)
	}

	now := time.Now()
	run := newRun(
		NewRunID(now), sanitiseName(cfg.Name), plan, metrics.NewStore(c.cfg.Store), executor.Peak(), sourcePath,
	)
	run.startAt = now.Add(c.cfg.StartDelay)

	c.runs[run.id] = run
	c.order = append(c.order, run.id)
	c.active = run
	c.trimHistoryLocked()
	c.mu.Unlock()

	parts := c.dispatch(run, sessions, executor.Peak(), true)
	run.setParticipants(parts)
	run.setPhase(loadwavev1.RunPhase_RUN_PHASE_STARTING, "")

	c.record(run, Event{
		Level:   "info",
		Source:  "coordinator",
		Message: fmt.Sprintf("run %s starting across %d agent(s): %s", run.id, len(parts), executor.Describe()),
	})
	c.log.Info("run starting",
		"run", run.id, "agents", len(parts), "peakVUs", executor.Peak(), "profile", executor.Describe())

	return run, nil
}

// dispatch apportions the run across agents and sends their orders.
//
// Agents are weighted by the virtual user capacity they advertised, so a
// sixteen-core host carries proportionally more than a two-core one instead of
// both being handed the same share and the small one falling behind.
func (c *Coordinator) dispatch(
	run *Run, sessions []*control.Session, peak int, initial bool,
) []*participant {
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })

	weights := make([]int, len(sessions))
	for i, session := range sessions {
		weights[i] = max(1, int(session.Hello.GetMaxVus()))
	}

	load := run.plan.GetLoad()
	vuShares := apportion.Largest(peak, weights)
	rateShares := apportion.Largest(int(load.GetMaxIterationsPerSecond()), weights)
	iterShares := apportion.Largest(int(min(load.GetIterations(), 1<<31)), weights)

	parts := make([]*participant, 0, len(sessions))
	for i, session := range sessions {
		part := &participant{
			AgentID:    session.ID,
			VUQuota:    vuShares[i],
			RateQuota:  rateShares[i],
			ShardIndex: uint32(i),
			Phase:      PhasePending,
			Dispatched: true,
		}
		parts = append(parts, part)

		msg := &loadwavev1.StartRun{
			RunId:              run.id,
			Plan:               run.plan,
			VuQuota:            uint32(vuShares[i]),
			IterationRateQuota: uint32(rateShares[i]),
			StartAt:            timestamppb.New(run.startAt),
			ShardIndex:         uint32(i),
			ShardCount:         uint32(len(sessions)),
			VuIdBase:           idspace.AgentBase(i),
			WorkerCount:        run.plan.GetWorkersPerAgent(),
		}
		if load.GetIterations() > 0 {
			msg.IterationQuota = uint64(iterShares[i])
		}

		if err := session.Send(&loadwavev1.NodeDown{
			Payload: &loadwavev1.NodeDown_StartRun{StartRun: msg},
		}); err != nil {
			c.log.Error("could not reach agent to start the run", "agent", session.ID, "error", err)
			part.Phase = PhaseFailed
			part.Message = err.Error()
			continue
		}

		c.mu.Lock()
		if info, ok := c.agents[session.ID]; ok {
			info.VUQuota = vuShares[i]
		}
		c.mu.Unlock()
	}

	if !initial {
		c.log.Info("rebalanced run", "run", run.id, "agents", len(parts))
	}
	return parts
}

// StopRun ends a run.
func (c *Coordinator) StopRun(runID string, graceful bool, reason string) error {
	run, ok := c.Lookup(runID)
	if !ok {
		return fmt.Errorf("no run %q", runID)
	}
	if !run.Active() {
		return fmt.Errorf("run %s has already finished", runID)
	}
	if reason == "" {
		reason = "stopped by operator"
	}

	run.setPhase(loadwavev1.RunPhase_RUN_PHASE_STOPPING, reason)
	c.record(run, Event{Level: "info", Source: "coordinator", Message: "stopping: " + reason})

	err := c.sessions.Broadcast(&loadwavev1.NodeDown{
		Payload: &loadwavev1.NodeDown_StopRun{StopRun: &loadwavev1.StopRun{
			RunId:    runID,
			Graceful: graceful,
			Reason:   reason,
		}},
	})
	if err != nil {
		c.log.Warn("could not reach every agent to stop the run", "run", runID, "error", err)
	}
	return nil
}

// ScaleRun changes a running test's peak virtual user count.
//
// A non-zero ramp introduces the change gradually. Spawning several hundred
// virtual users in a single tick measures how the service copes with a
// thundering herd, which is a different question from how it copes with the
// load level being asked for; the ramp is how an operator asks the second
// question.
func (c *Coordinator) ScaleRun(runID string, peakVUs int, ramp time.Duration) error {
	if peakVUs < 0 {
		return errors.New("virtual user count cannot be negative")
	}
	if ramp < 0 {
		return errors.New("ramp cannot be negative")
	}

	run, ok := c.Lookup(runID)
	if !ok {
		return fmt.Errorf("no run %q", runID)
	}
	if !run.Active() {
		return fmt.Errorf("run %s has already finished", runID)
	}

	run.mu.Lock()
	previous := run.peakVUs
	run.peakVUs = peakVUs
	run.scaleRamp = ramp
	run.mu.Unlock()

	c.rebalance(run)

	message := fmt.Sprintf("peak virtual users changed from %d to %d", previous, peakVUs)
	if ramp > 0 {
		message += fmt.Sprintf(", ramping over %s", ramp)
	}
	c.record(run, Event{Level: "info", Source: "coordinator", Message: message})
	return nil
}

// rebalance recomputes quotas across the currently healthy agents.
//
// It runs whenever the fleet changes shape. An agent that joins mid-run gets a
// full start command and picks up the profile from wherever it currently is;
// the survivors of an agent that died absorb its share, so a run does not
// quietly lose a third of its load when one host falls over.
func (c *Coordinator) rebalance(run *Run) {
	if run == nil || !run.Active() {
		return
	}

	sessions := c.healthySessions()
	if len(sessions) == 0 {
		c.log.Warn("no agents remain for the run", "run", run.id)
		return
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })

	run.mu.Lock()
	peak := run.peakVUs
	// The ramp belongs to this dispatch alone. A rebalance triggered by an
	// agent dying has to take effect immediately, or the run silently runs
	// under target for the length of somebody else's ramp.
	ramp := run.scaleRamp
	run.scaleRamp = 0
	known := make(map[string]bool, len(run.participants))
	for id, p := range run.participants {
		known[id] = p.Dispatched
	}
	run.mu.Unlock()

	weights := make([]int, len(sessions))
	for i, session := range sessions {
		weights[i] = max(1, int(session.Hello.GetMaxVus()))
	}
	load := run.plan.GetLoad()
	vuShares := apportion.Largest(peak, weights)
	rateShares := apportion.Largest(int(load.GetMaxIterationsPerSecond()), weights)

	parts := make([]*participant, 0, len(sessions))
	for i, session := range sessions {
		part := &participant{
			AgentID:    session.ID,
			VUQuota:    vuShares[i],
			RateQuota:  rateShares[i],
			ShardIndex: uint32(i),
			Phase:      PhaseRunning,
			Dispatched: true,
		}
		parts = append(parts, part)

		var msg *loadwavev1.NodeDown
		if known[session.ID] {
			msg = &loadwavev1.NodeDown{
				Payload: &loadwavev1.NodeDown_SetQuota{SetQuota: &loadwavev1.SetQuota{
					RunId:              run.id,
					VuQuota:            uint32(vuShares[i]),
					IterationRateQuota: uint32(rateShares[i]),
					Ramp:               durationpb.New(ramp),
				}},
			}
		} else {
			msg = &loadwavev1.NodeDown{
				Payload: &loadwavev1.NodeDown_StartRun{StartRun: &loadwavev1.StartRun{
					RunId:              run.id,
					Plan:               run.plan,
					VuQuota:            uint32(vuShares[i]),
					IterationRateQuota: uint32(rateShares[i]),
					StartAt:            timestamppb.New(run.startAt),
					ShardIndex:         uint32(i),
					ShardCount:         uint32(len(sessions)),
					VuIdBase:           idspace.AgentBase(i),
					WorkerCount:        run.plan.GetWorkersPerAgent(),
				}},
			}
		}

		if err := session.Send(msg); err != nil {
			c.log.Error("could not reach agent while rebalancing", "agent", session.ID, "error", err)
			part.Message = err.Error()
		}
	}
	run.setParticipants(parts)
}

// finish moves a run into a terminal phase and records the verdict.
func (c *Coordinator) finish(run *Run, phase loadwavev1.RunPhase, reason string) {
	run.setThresholds(EvaluateThresholds(run.store, run.plan.GetThresholds()))
	if !run.setPhase(phase, reason) {
		return
	}

	level := "info"
	if phase != loadwavev1.RunPhase_RUN_PHASE_COMPLETED || run.Breached() {
		level = "warn"
	}
	c.record(run, Event{
		Level:   level,
		Source:  "coordinator",
		Message: fmt.Sprintf("run %s: %s", PhaseName(phase), reason),
	})
	c.log.Info("run finished",
		"run", run.id, "phase", PhaseName(phase), "reason", reason, "breached", run.Breached())
}

// ---------------------------------------------------------------------------
// Periodic maintenance
// ---------------------------------------------------------------------------

// tick performs the coordinator's once-per-interval housekeeping.
func (c *Coordinator) tick(now time.Time) {
	c.expireAgents(now)

	run := c.ActiveRun()
	if run == nil {
		c.subs.publish(Update{Type: UpdateTick, Agents: c.Agents()})
		return
	}

	run.store.CloseStale(now)

	results := EvaluateThresholds(run.store, run.plan.GetThresholds())
	run.setThresholds(results)

	if breach, abort := AbortRequested(results); abort && run.Active() {
		reason := fmt.Sprintf("threshold breached: %s (actual %.4g)", breach.Description, breach.Actual)
		c.record(run, Event{Level: "error", Source: "threshold", Message: reason})
		_ = c.StopRun(run.id, true, reason)
	}

	if run.Active() {
		c.checkCompletion(run)
		c.checkStopTimeout(run)
	}

	c.subs.publish(Update{
		Type:       UpdateTick,
		Run:        ptr(run.Summary(c.profileOf(run))),
		Agents:     c.Agents(),
		Ticks:      c.ticksSince(run),
		Thresholds: results,
	})
}

// checkCompletion finishes a run once every participating agent has reported a
// terminal phase.
func (c *Coordinator) checkCompletion(run *Run) {
	parts := run.Participants()
	if len(parts) == 0 {
		return
	}

	failures := 0
	for _, p := range parts {
		switch p.Phase {
		case PhaseCompleted:
		case PhaseFailed:
			failures++
		default:
			return // at least one agent is still working
		}
	}

	if failures == len(parts) {
		c.finish(run, loadwavev1.RunPhase_RUN_PHASE_FAILED, "every agent reported a failure")
		return
	}
	reason := "all agents finished"
	if failures > 0 {
		reason = fmt.Sprintf("all agents finished, %d of %d reported a failure", failures, len(parts))
	}
	c.finish(run, loadwavev1.RunPhase_RUN_PHASE_COMPLETED, reason)
}

// checkStopTimeout force-finishes a run whose agents never confirmed stopping.
//
// A run that has been asked to stop depends on every agent reporting back to
// reach a terminal phase. An agent that dies at exactly the wrong moment would
// otherwise leave the run — and any CLI or CI job waiting on it — hanging
// indefinitely. Past the grace period plus a margin, the coordinator stops
// waiting and records what it has.
func (c *Coordinator) checkStopTimeout(run *Run) {
	stopping := run.StoppingFor()
	if stopping == 0 {
		return
	}

	limit := run.GraceBudget() + stopTimeoutMargin
	if stopping < limit {
		return
	}

	c.log.Warn("agents did not confirm the run stopped; finishing it anyway",
		"run", run.id, "waited", stopping.Round(time.Second))
	c.finish(run, loadwavev1.RunPhase_RUN_PHASE_ABORTED,
		fmt.Sprintf("stopped, but %s passed without every agent confirming", limit))
}

// expireAgents drops agents that have gone quiet.
func (c *Coordinator) expireAgents(now time.Time) {
	sessions := c.sessions.All()
	lost := make([]string, 0, len(sessions))

	for _, session := range sessions {
		if now.Sub(session.LastSeen()) <= c.cfg.AgentTimeout {
			continue
		}
		c.log.Warn("agent timed out",
			"agent", session.ID, "lastSeen", session.LastSeen(), "timeout", c.cfg.AgentTimeout)
		session.Close()
		lost = append(lost, session.ID)
	}

	if len(lost) == 0 {
		return
	}
	if run := c.ActiveRun(); run != nil {
		c.record(run, Event{
			Level:   "error",
			Source:  "coordinator",
			Message: fmt.Sprintf("lost contact with agent(s) %v; redistributing their load", lost),
		})
		c.rebalance(run)
	}
}

// healthySessions returns the agents currently fit to receive work.
func (c *Coordinator) healthySessions() []*control.Session {
	all := c.sessions.All()
	out := make([]*control.Session, 0, len(all))
	for _, session := range all {
		if !session.Closed() {
			out = append(out, session)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// control.SessionHandler
// ---------------------------------------------------------------------------

// OnJoin implements control.SessionHandler.
func (c *Coordinator) OnJoin(_ context.Context, session *control.Session) error {
	c.sessions.Add(session)

	info := &AgentInfo{
		ID:         session.ID,
		Hostname:   session.Hello.GetHostname(),
		Version:    session.Hello.GetVersion(),
		Cores:      session.Hello.GetCpuCores(),
		MaxWorkers: session.Hello.GetMaxWorkers(),
		MaxVUs:     session.Hello.GetMaxVus(),
		Labels:     session.Hello.GetLabels(),
		RemoteAddr: session.RemoteAddr,
		JoinedAt:   session.JoinedAt,
		LastSeen:   session.LastSeen(),
		Healthy:    true,
		Workers:    []WorkerInfo{},
	}

	c.mu.Lock()
	c.agents[session.ID] = info
	c.mu.Unlock()

	c.recordGlobal(Event{
		Level:   "info",
		Source:  "cluster",
		Message: fmt.Sprintf("agent %s joined from %s", session.ID, session.RemoteAddr),
	})

	// An agent arriving mid-run is put to work rather than left idle, which
	// is what makes scaling a running test out simply a matter of starting
	// another agent.
	if run := c.ActiveRun(); run != nil {
		go c.rebalance(run)
	}
	return nil
}

// OnLeave implements control.SessionHandler.
func (c *Coordinator) OnLeave(session *control.Session) {
	if !c.sessions.Remove(session) {
		// Displaced by a reconnection that already owns this node id.
		return
	}

	c.mu.Lock()
	delete(c.agents, session.ID)
	c.mu.Unlock()

	c.recordGlobal(Event{
		Level:   "warn",
		Source:  "cluster",
		Message: fmt.Sprintf("agent %s disconnected", session.ID),
	})

	if run := c.ActiveRun(); run != nil {
		go c.rebalance(run)
	}
}

// OnHeartbeat implements control.SessionHandler.
func (c *Coordinator) OnHeartbeat(session *control.Session, beat *loadwavev1.NodeHeartbeat) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if info, ok := c.agents[session.ID]; ok {
		info.LastSeen = session.LastSeen()
		info.ActiveVUs = beat.GetActiveVus()
		info.HealthyWorkers = beat.GetHealthyWorkers()
		info.Healthy = true
		info.CPUPercent = beat.GetCpuPercent()
		info.MemBytes = beat.GetMemBytes()

		workers := make([]WorkerInfo, 0, len(beat.GetWorkers()))
		for _, w := range beat.GetWorkers() {
			workers = append(workers, WorkerInfo{
				ID:         w.GetWorkerId(),
				Index:      w.GetIndex(),
				ActiveVUs:  w.GetActiveVus(),
				CPUPercent: w.GetCpuPercent(),
				MemBytes:   w.GetMemBytes(),
			})
		}
		sort.Slice(workers, func(i, j int) bool { return workers[i].Index < workers[j].Index })
		info.Workers = workers
	}
}

// OnMetrics implements control.SessionHandler.
func (c *Coordinator) OnMetrics(_ *control.Session, batch *loadwavev1.MetricBatch) {
	run, ok := c.Lookup(batch.GetRunId())
	if !ok {
		// Telemetry for a run this coordinator has never heard of: an agent
		// still finishing a run started by a previous coordinator process.
		// Dropping it is right; the run it belongs to no longer exists.
		return
	}
	if err := run.store.Ingest(batch); err != nil {
		c.log.Debug("rejected a metric batch", "run", batch.GetRunId(),
			"node", batch.GetNodeId(), "error", err)
	}
}

// OnRunStatus implements control.SessionHandler.
func (c *Coordinator) OnRunStatus(session *control.Session, update *loadwavev1.RunStatusUpdate) {
	run, ok := c.Lookup(update.GetRunId())
	if !ok {
		return
	}

	phase := PhaseName(update.GetPhase())
	run.updateParticipant(session.ID, phase, update.GetMessage())

	if update.GetPhase() == loadwavev1.RunPhase_RUN_PHASE_RUNNING {
		run.setPhase(loadwavev1.RunPhase_RUN_PHASE_RUNNING, "")
	}
	if update.GetPhase() == loadwavev1.RunPhase_RUN_PHASE_FAILED {
		c.record(run, Event{
			Level:   "error",
			Source:  session.ID,
			Message: update.GetMessage(),
		})
	}
}

// OnLog implements control.SessionHandler.
func (c *Coordinator) OnLog(session *control.Session, event *loadwavev1.LogEvent) {
	level := "info"
	switch event.GetLevel() {
	case loadwavev1.LogLevel_LOG_LEVEL_DEBUG:
		level = "debug"
	case loadwavev1.LogLevel_LOG_LEVEL_WARN:
		level = "warn"
	case loadwavev1.LogLevel_LOG_LEVEL_ERROR:
		level = "error"
	default:
		// Unspecified and info both render as info.
	}

	entry := Event{
		Time:    event.GetTime().AsTime(),
		Level:   level,
		Source:  event.GetNodeId(),
		Message: event.GetMessage(),
		Fields:  event.GetFields(),
	}
	if entry.Source == "" {
		entry.Source = session.ID
	}

	if run, ok := c.Lookup(event.GetRunId()); ok {
		c.record(run, entry)
		return
	}
	c.recordGlobal(entry)
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

// Lookup returns a run by id.
func (c *Coordinator) Lookup(runID string) (*Run, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	run, ok := c.runs[runID]
	return run, ok
}

// ActiveRun returns the run in progress, or nil.
func (c *Coordinator) ActiveRun() *Run {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active
}

// Agents lists connected agents, newest first.
func (c *Coordinator) Agents() []AgentInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]AgentInfo, 0, len(c.agents))
	for _, info := range c.agents {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Runs lists known runs, newest first.
func (c *Coordinator) Runs() []Summary {
	c.mu.RLock()
	ids := append([]string(nil), c.order...)
	runs := make([]*Run, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if run, ok := c.runs[ids[i]]; ok {
			runs = append(runs, run)
		}
	}
	c.mu.RUnlock()

	out := make([]Summary, 0, len(runs))
	for _, run := range runs {
		out = append(out, run.Summary(c.profileOf(run)))
	}
	return out
}

// profileOf renders a run's load profile for display.
func (c *Coordinator) profileOf(run *Run) string {
	executor, err := engine.NewExecutor(run.plan.GetLoad())
	if err != nil {
		return ""
	}
	return executor.Describe()
}

// trimHistoryLocked evicts the oldest finished runs. Callers hold c.mu.
func (c *Coordinator) trimHistoryLocked() {
	for len(c.order) > c.cfg.MaxRunHistory {
		oldest := c.order[0]
		if run, ok := c.runs[oldest]; ok && run.Active() {
			// Never evict a live run, however long the history is.
			return
		}
		delete(c.runs, oldest)
		c.order = c.order[1:]
	}
}

// record appends an event to a run's log and publishes it.
func (c *Coordinator) record(run *Run, event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	run.addEvent(event)
	c.subs.publish(Update{Type: UpdateEvent, Events: []Event{event}})
}

// recordGlobal appends an event not tied to any run.
func (c *Coordinator) recordGlobal(event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}

	c.mu.Lock()
	c.events = append(c.events, event)
	if len(c.events) > maxGlobalEvents {
		keep := len(c.events) - maxGlobalEvents
		c.events = append(c.events[:0], c.events[keep:]...)
	}
	c.mu.Unlock()

	c.subs.publish(Update{Type: UpdateEvent, Events: []Event{event}})
}

// GlobalEvents returns cluster-level events not attached to a run.
//
// Never nil, even when empty: this feeds Snapshot.Events, which is
// JSON-encoded straight into the API, and a nil slice there marshals to
// `null` rather than `[]`.
func (c *Coordinator) GlobalEvents() []Event {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Event{}, c.events...)
}

// ptr returns a pointer to v, for building optional JSON fields inline.
func ptr[T any](v T) *T { return &v }

// assert the coordinator satisfies the handler interface it is registered as.
var _ control.SessionHandler = (*Coordinator)(nil)
