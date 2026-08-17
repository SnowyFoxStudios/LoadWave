// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agent runs one host's share of a distributed load test.
//
// An agent is a supervisor, not a load generator. It joins the coordinator,
// spawns worker processes on its host, subdivides the quota it was given
// across them, and relays their telemetry upward. Keeping supervision out of
// the processes that generate load is what allows a worker to be killed,
// crash, or be starved of CPU without taking the host's participation in the
// run down with it.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
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
)

// Config describes an agent.
type Config struct {
	// NodeID identifies this agent to the coordinator. Reconnecting with the
	// same id is what lets the coordinator recognise a returning agent rather
	// than counting it twice.
	NodeID string

	// CoordinatorTarget is the coordinator's gRPC address.
	CoordinatorTarget string

	// Workers is how many worker processes to spawn per run. Zero derives it
	// from the core count.
	Workers int

	// MaxVUs is the ceiling this agent advertises. Zero derives one from the
	// core count.
	MaxVUs int

	// Labels are advertised to the coordinator, for operator-facing grouping.
	Labels map[string]string

	// SocketPath is where the worker control socket lives. Zero picks a path
	// under the system temporary directory.
	SocketPath string

	// WorkerArgs are appended to every spawned worker's command line.
	WorkerArgs []string

	// Executable is the binary to spawn as a worker. Empty uses this process's
	// own path, which is almost always right: a Go SDK test compiles its
	// scenarios into the same binary that runs the agent.
	Executable string

	Logger *slog.Logger
}

// Defaults for a zero Config.
const (
	// DefaultVUsPerCore is the advertised capacity per core. Virtual users
	// are goroutines blocked on network I/O for most of their lives, so a
	// core carries far more of them than it could carry busy threads.
	DefaultVUsPerCore = 1000

	// workerJoinTimeout bounds how long a run waits for spawned workers to
	// connect before proceeding with whichever ones did.
	workerJoinTimeout = 20 * time.Second

	// workerStopTimeout is how long a worker gets to exit on its own before
	// it is killed.
	workerStopTimeout = 30 * time.Second

	// workerReportGrace is added to the plan's graceful-stop budget to cover
	// a worker's own shutdown and the trip its final status takes upstream.
	workerReportGrace = 5 * time.Second
)

// workerProc is a spawned worker process.
type workerProc struct {
	id      string
	index   int
	cmd     *exec.Cmd
	started time.Time
	exited  chan struct{}
}

// runState is the agent's view of the run it is participating in.
type runState struct {
	id      string
	plan    *loadwavev1.TestPlan
	startAt time.Time

	vuQuota        int
	rateQuota      int
	iterationQuota uint64

	// ramp is how long the workers should take to reach a newly-set quota.
	// It applies to the next dispatch only; a rebalance caused by the fleet
	// changing shape is immediate.
	ramp       time.Duration
	vuIDBase   int64
	shardIndex uint32
	shardCount uint32

	// dispatched records which workers have already been told to start, so a
	// rebalance sends them a quota change rather than a duplicate start.
	dispatched map[string]bool
}

// Agent supervises one host's worker processes.
type Agent struct {
	cfg Config
	log *slog.Logger

	upstream *control.Client
	workers  *control.SessionRegistry

	grpcServer *grpc.Server
	socketPath string

	mu    sync.Mutex
	procs map[string]*workerProc
	run   *runState

	// vuByWorker holds each worker's most recent reported VU count, kept
	// under its own lock so heartbeat handling never waits on supervision.
	vuMu       sync.Mutex
	vuByWorker map[string]uint32

	// phaseMu guards the per-worker run phases the agent folds into the
	// single status it reports upward.
	phaseMu     sync.Mutex
	workerPhase map[string]loadwavev1.RunPhase
	workerIters map[string]uint64

	// stopMu serialises stopRun, which is now asynchronous and can be
	// triggered by a coordinator command and by shutdown at the same time.
	stopMu sync.Mutex

	nextWorkerSeq atomic.Int64
	activeVUs     atomic.Int64
}

// New prepares an agent. It neither listens nor connects; Run does both.
func New(cfg Config) (*Agent, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("agent needs a node id")
	}
	if cfg.CoordinatorTarget == "" {
		return nil, errors.New("agent needs a coordinator target")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkerCount()
	}
	if cfg.MaxVUs <= 0 {
		cfg.MaxVUs = runtime.NumCPU() * DefaultVUsPerCore
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate this executable to spawn workers: %w", err)
		}
		cfg.Executable = exe
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = defaultSocketPath(cfg.NodeID)
	}

	a := &Agent{
		cfg:         cfg,
		log:         cfg.Logger.With("agent", cfg.NodeID),
		workers:     control.NewSessionRegistry(),
		socketPath:  cfg.SocketPath,
		procs:       make(map[string]*workerProc),
		vuByWorker:  make(map[string]uint32),
		workerPhase: make(map[string]loadwavev1.RunPhase),
		workerIters: make(map[string]uint64),
	}

	hostname, _ := os.Hostname()
	upstream, err := control.NewClient(control.ClientConfig{
		Target:  cfg.CoordinatorTarget,
		Handler: a,
		Logger:  a.log,
		Hello: &loadwavev1.NodeHello{
			NodeId:     cfg.NodeID,
			Hostname:   hostname,
			Version:    buildinfo.Version(),
			CpuCores:   uint32(runtime.NumCPU()),
			MaxWorkers: uint32(cfg.Workers),
			MaxVus:     uint32(cfg.MaxVUs),
			Labels:     cfg.Labels,
		},
		Heartbeat: a.heartbeat,
	})
	if err != nil {
		return nil, err
	}
	a.upstream = upstream
	return a, nil
}

// defaultWorkerCount leaves a core for the agent itself and for the operating
// system, so that supervision and metric reporting do not compete with load
// generation on a small host.
func defaultWorkerCount() int {
	return max(1, runtime.NumCPU()-1)
}

// defaultSocketPath builds a short socket path.
//
// Unix socket paths are limited to about a hundred bytes, and macOS temporary
// directories are already long, so the name is kept terse and the pid keeps
// two agents on one host from colliding.
func defaultSocketPath(nodeID string) string {
	short := nodeID
	if len(short) > 8 {
		short = short[:8]
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("lw-%s-%d.sock", short, os.Getpid()))
}

// SocketPath returns the worker control socket's filesystem path.
func (a *Agent) SocketPath() string { return a.socketPath }

// Run serves workers and stays joined to the coordinator until ctx is
// cancelled.
func (a *Agent) Run(ctx context.Context) error {
	listener, err := a.listen(ctx)
	if err != nil {
		return err
	}

	server, err := control.NewServer(control.ServerConfig{
		Handler: a,
		Logger:  a.log,
		Version: buildinfo.Version(),
	})
	if err != nil {
		return err
	}

	a.grpcServer = grpc.NewServer()
	loadwavev1.RegisterControlServiceServer(a.grpcServer, server)

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.grpcServer.Serve(listener) }()

	a.log.Info("agent ready",
		"socket", a.socketPath,
		"coordinator", a.cfg.CoordinatorTarget,
		"workers", a.cfg.Workers,
		"maxVUs", a.cfg.MaxVUs)

	upstreamErr := make(chan error, 1)
	go func() { upstreamErr <- a.upstream.Run(ctx) }()

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		a.log.Error("worker control server stopped", "error", err)
	case err = <-upstreamErr:
	}

	a.shutdown()
	return err
}

// listen opens the worker control socket, clearing any stale one left behind
// by a previous process that did not exit cleanly.
func (a *Agent) listen(ctx context.Context) (net.Listener, error) {
	if err := os.Remove(a.socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("clear stale socket %s: %w", a.socketPath, err)
	}

	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", a.socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", a.socketPath, err)
	}
	// The socket is a local control channel with no authentication of its
	// own, so the filesystem is the access control: owner only.
	if err := os.Chmod(a.socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}
	return listener, nil
}

// shutdown stops workers and releases the socket.
func (a *Agent) shutdown() {
	a.stopRun(true, "agent shutting down")
	a.terminateWorkers()

	if a.grpcServer != nil {
		a.grpcServer.GracefulStop()
	}
	if err := os.Remove(a.socketPath); err != nil && !os.IsNotExist(err) {
		a.log.Warn("could not remove control socket", "path", a.socketPath, "error", err)
	}
}

// heartbeat reports this agent's aggregate load to the coordinator.
func (a *Agent) heartbeat() *loadwavev1.NodeHeartbeat {
	return &loadwavev1.NodeHeartbeat{
		ActiveVus:      uint32(a.activeVUs.Load()),
		HealthyWorkers: uint32(a.workers.Len()),
	}
}

// ---------------------------------------------------------------------------
// Coordinator commands (control.Handler)
// ---------------------------------------------------------------------------

// OnAccepted implements control.Handler.
func (a *Agent) OnAccepted(_ context.Context, msg *loadwavev1.Accepted) error {
	a.log.Info("joined coordinator", "coordinatorVersion", msg.GetSupervisorVersion())
	return nil
}

// OnStartRun implements control.Handler.
func (a *Agent) OnStartRun(_ context.Context, msg *loadwavev1.StartRun) error {
	a.stopRun(false, "superseded by a new run")

	workerCount := int(msg.GetPlan().GetWorkersPerAgent())
	if workerCount <= 0 {
		workerCount = a.cfg.Workers
	}
	workerCount = max(1, min(workerCount, a.cfg.Workers))

	state := &runState{
		id:             msg.GetRunId(),
		plan:           msg.GetPlan(),
		startAt:        msg.GetStartAt().AsTime(),
		vuQuota:        int(msg.GetVuQuota()),
		rateQuota:      int(msg.GetIterationRateQuota()),
		iterationQuota: msg.GetIterationQuota(),
		vuIDBase:       msg.GetVuIdBase(),
		shardIndex:     msg.GetShardIndex(),
		shardCount:     msg.GetShardCount(),
		dispatched:     make(map[string]bool),
	}

	a.mu.Lock()
	a.run = state
	a.mu.Unlock()

	a.phaseMu.Lock()
	clear(a.workerPhase)
	clear(a.workerIters)
	a.phaseMu.Unlock()

	a.log.Info("starting run",
		"run", state.id, "workers", workerCount, "vuQuota", state.vuQuota)

	if err := a.spawnWorkers(state.id, workerCount); err != nil {
		a.reportStatus(state.id, loadwavev1.RunPhase_RUN_PHASE_FAILED, err.Error())
		return err
	}

	// Workers connect back asynchronously. Dispatch happens once they are
	// all present, or once waiting for the stragglers has cost more than
	// their contribution is worth.
	go a.dispatchWhenReady(state.id, workerCount)
	return nil
}

// OnSetQuota implements control.Handler.
func (a *Agent) OnSetQuota(_ context.Context, msg *loadwavev1.SetQuota) error {
	a.mu.Lock()
	run := a.run
	if run == nil || run.id != msg.GetRunId() {
		a.mu.Unlock()
		return fmt.Errorf("no run %q is active on this agent", msg.GetRunId())
	}
	run.vuQuota = int(msg.GetVuQuota())
	run.rateQuota = int(msg.GetIterationRateQuota())
	// Passed through unchanged rather than divided: every worker should take
	// the same time to reach its share, so the host's total follows the ramp
	// the operator asked for.
	run.ramp = msg.GetRamp().AsDuration()
	a.mu.Unlock()

	a.rebalance()
	return nil
}

// OnStopRun implements control.Handler.
//
// Stopping waits for workers to wind down, which takes as long as the plan's
// grace period allows. That cannot happen on the control stream's receive
// goroutine: blocking there would stop the agent noticing anything else the
// coordinator says, including a follow-up hard stop.
func (a *Agent) OnStopRun(_ context.Context, msg *loadwavev1.StopRun) error {
	go a.stopRun(msg.GetGraceful(), msg.GetReason())
	return nil
}

// ---------------------------------------------------------------------------
// Worker supervision
// ---------------------------------------------------------------------------

// spawnWorkers launches worker processes for a run.
func (a *Agent) spawnWorkers(runID string, count int) error {
	for i := range count {
		if err := a.spawnWorker(runID, i); err != nil {
			return fmt.Errorf("spawn worker %d: %w", i, err)
		}
	}
	return nil
}

// spawnWorker launches one worker process.
func (a *Agent) spawnWorker(runID string, index int) error {
	id := fmt.Sprintf("%s-w%d-%d", a.cfg.NodeID, index, a.nextWorkerSeq.Add(1))

	args := append([]string{
		"worker",
		"--agent-socket", a.socketPath,
		"--node-id", id,
	}, a.cfg.WorkerArgs...)

	// Deliberately not CommandContext: worker lifetime is managed explicitly
	// through the control protocol and terminateWorkers, so that a stopping
	// run gets a graceful drain instead of having its processes killed the
	// instant the agent's context ends.
	//nolint:noctx // see above
	cmd := exec.Command(a.cfg.Executable, args...)
	cmd.Env = os.Environ()
	// Workers inherit the agent's streams so their logs land wherever the
	// operator is already looking, rather than in a file nobody finds.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	proc := &workerProc{
		id:      id,
		index:   index,
		cmd:     cmd,
		started: time.Now(),
		exited:  make(chan struct{}),
	}

	a.mu.Lock()
	a.procs[id] = proc
	a.mu.Unlock()

	a.log.Info("spawned worker", "worker", id, "pid", cmd.Process.Pid, "index", index)
	go a.superviseWorker(runID, proc)
	return nil
}

// superviseWorker waits for a worker to exit and reacts to it going away
// unexpectedly.
func (a *Agent) superviseWorker(runID string, proc *workerProc) {
	err := proc.cmd.Wait()
	close(proc.exited)

	a.mu.Lock()
	delete(a.procs, proc.id)
	run := a.run
	a.mu.Unlock()

	expected := run == nil || run.id != runID
	switch {
	case expected:
		a.log.Info("worker exited", "worker", proc.id)
		return
	case err != nil:
		a.log.Error("worker died during a run", "worker", proc.id, "error", err,
			"ranFor", time.Since(proc.started).Round(time.Second))
	default:
		a.log.Error("worker exited unexpectedly during a run", "worker", proc.id,
			"ranFor", time.Since(proc.started).Round(time.Second))
	}

	a.reportLog(loadwavev1.LogLevel_LOG_LEVEL_ERROR, runID,
		fmt.Sprintf("worker %s stopped unexpectedly", proc.id),
		map[string]string{"worker": proc.id})

	// The surviving workers take over the dead one's share. The run is worth
	// more than perfect fidelity here: an operator who asked for a thousand
	// VUs is better served by a thousand on three processes than by seven
	// hundred and fifty and no warning.
	a.rebalance()
}

// terminateWorkers stops every worker process, escalating to a kill.
func (a *Agent) terminateWorkers() {
	a.mu.Lock()
	procs := make([]*workerProc, 0, len(a.procs))
	for _, proc := range a.procs {
		procs = append(procs, proc)
	}
	a.mu.Unlock()

	for _, proc := range procs {
		if proc.cmd.Process == nil {
			continue
		}
		if err := proc.cmd.Process.Signal(os.Interrupt); err != nil {
			a.log.Warn("could not interrupt worker", "worker", proc.id, "error", err)
		}
	}

	for _, proc := range procs {
		select {
		case <-proc.exited:
		case <-time.After(workerStopTimeout):
			a.log.Warn("worker did not exit in time; killing", "worker", proc.id)
			if proc.cmd.Process != nil {
				_ = proc.cmd.Process.Kill()
			}
			<-proc.exited
		}
	}
}

// dispatchWhenReady waits for the spawned workers to join, then hands out
// their shares.
func (a *Agent) dispatchWhenReady(runID string, expected int) {
	deadline := time.After(workerJoinTimeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		a.mu.Lock()
		stale := a.run == nil || a.run.id != runID
		a.mu.Unlock()
		if stale {
			return
		}

		if a.workers.Len() >= expected {
			a.rebalance()
			return
		}

		select {
		case <-ticker.C:
		case <-deadline:
			joined := a.workers.Len()
			if joined == 0 {
				err := fmt.Errorf("no worker joined within %s", workerJoinTimeout)
				a.log.Error("cannot run", "run", runID, "error", err)
				a.reportStatus(runID, loadwavev1.RunPhase_RUN_PHASE_FAILED, err.Error())
				return
			}
			a.log.Warn("proceeding without every worker",
				"run", runID, "joined", joined, "expected", expected)
			a.rebalance()
			return
		}
	}
}

// rebalance recomputes each worker's share and pushes it out.
//
// It is called whenever the set of workers changes or the coordinator adjusts
// this agent's quota. Workers that have not been started yet receive a full
// start command; the rest receive a quota update, so a rebalance never
// restarts a virtual user that is already doing useful work.
func (a *Agent) rebalance() {
	a.mu.Lock()
	run := a.run
	if run == nil {
		a.mu.Unlock()
		return
	}

	sessions := a.workers.All()
	// Sorting makes apportionment reproducible: the same fleet always gets
	// the same split, which keeps ids and data shards stable across a
	// rebalance instead of reshuffling every worker's fixtures.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })

	if len(sessions) == 0 {
		a.mu.Unlock()
		a.log.Warn("no workers are connected; nothing to rebalance", "run", run.id)
		return
	}

	weights := make([]int, len(sessions))
	for i := range weights {
		weights[i] = 1
	}

	vuShares := apportion.Largest(run.vuQuota, weights)
	rateShares := apportion.Largest(run.rateQuota, weights)
	iterShares := apportion.Largest(int(min(run.iterationQuota, 1<<31)), weights)

	type dispatch struct {
		session *control.Session
		msg     *loadwavev1.NodeDown
	}
	messages := make([]dispatch, 0, len(sessions))

	for i, session := range sessions {
		if run.dispatched[session.ID] {
			messages = append(messages, dispatch{session, &loadwavev1.NodeDown{
				Payload: &loadwavev1.NodeDown_SetQuota{SetQuota: &loadwavev1.SetQuota{
					RunId:              run.id,
					VuQuota:            uint32(vuShares[i]),
					IterationRateQuota: uint32(rateShares[i]),
					Ramp:               durationpb.New(run.ramp),
				}},
			}})
			continue
		}

		run.dispatched[session.ID] = true
		start := &loadwavev1.StartRun{
			RunId:              run.id,
			Plan:               run.plan,
			VuQuota:            uint32(vuShares[i]),
			IterationRateQuota: uint32(rateShares[i]),
			StartAt:            timestampOf(run.startAt),
			ShardIndex:         run.shardIndex,
			ShardCount:         run.shardCount,
			VuIdBase:           idspace.WorkerBase(run.vuIDBase, i),
		}
		if run.iterationQuota > 0 {
			start.IterationQuota = uint64(iterShares[i])
		}
		messages = append(messages, dispatch{session, &loadwavev1.NodeDown{
			Payload: &loadwavev1.NodeDown_StartRun{StartRun: start},
		}})
	}
	// The ramp applies to this dispatch only. A later rebalance — an agent
	// lost, a worker died — must take effect at once rather than easing while
	// the run runs short.
	run.ramp = 0
	a.mu.Unlock()

	// Sent outside the lock: Send can block briefly on a slow worker, and
	// holding the agent's lock through that would stall heartbeats and
	// metric relaying for every other worker.
	for _, d := range messages {
		if err := d.session.Send(d.msg); err != nil {
			a.log.Error("could not reach worker", "worker", d.session.ID, "error", err)
		}
	}
}

// stopRun ends the active run and stops the workers that were serving it.
//
// The order matters. Workers are asked to stop, then given time to finish
// their in-flight iterations and report a terminal phase, and only then are
// their processes torn down. Killing them straight after the request would
// discard the last seconds of results and — because the coordinator decides a
// run is over when every agent reports terminal — leave the run stuck in
// "stopping" until something else timed it out.
func (a *Agent) stopRun(graceful bool, reason string) {
	a.stopMu.Lock()
	defer a.stopMu.Unlock()

	a.mu.Lock()
	run := a.run
	a.mu.Unlock()

	if run == nil {
		return
	}
	a.log.Info("stopping run", "run", run.id, "graceful", graceful, "reason", reason)

	stop := &loadwavev1.NodeDown{
		Payload: &loadwavev1.NodeDown_StopRun{StopRun: &loadwavev1.StopRun{
			RunId:    run.id,
			Graceful: graceful,
			Reason:   reason,
		}},
	}
	if err := a.workers.Broadcast(stop); err != nil {
		a.log.Warn("could not reach every worker to stop it", "error", err)
	}

	if graceful {
		budget := run.plan.GetLoad().GetGracefulStop().AsDuration()
		if budget <= 0 {
			budget = engine.DefaultGracefulStop
		}
		// The extra allowance covers the worker's own shutdown and the trip
		// its final status takes back up the socket.
		if !a.awaitWorkersFinished(budget + workerReportGrace) {
			a.log.Warn("workers did not all report finishing in time", "run", run.id)
		}
	}

	a.mu.Lock()
	if a.run == run {
		a.run = nil
	}
	a.mu.Unlock()

	a.terminateWorkers()
}

// awaitWorkersFinished blocks until every connected worker has reported a
// terminal phase, reporting whether they all did before the deadline.
func (a *Agent) awaitWorkersFinished(limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		a.phaseMu.Lock()
		phases := make([]loadwavev1.RunPhase, 0, len(a.workerPhase))
		for _, phase := range a.workerPhase {
			phases = append(phases, phase)
		}
		a.phaseMu.Unlock()

		// No worker ever reported: there is nothing left to wait for, and
		// waiting out the full grace period would only delay the run's end.
		if len(phases) == 0 {
			return true
		}

		done := true
		for _, phase := range phases {
			switch phase {
			case loadwavev1.RunPhase_RUN_PHASE_COMPLETED,
				loadwavev1.RunPhase_RUN_PHASE_FAILED,
				loadwavev1.RunPhase_RUN_PHASE_ABORTED:
			default:
				done = false
			}
		}
		if done {
			return true
		}

		if time.Now().After(deadline) {
			return false
		}
		<-ticker.C
	}
}

// ---------------------------------------------------------------------------
// Worker telemetry (control.SessionHandler)
// ---------------------------------------------------------------------------

// OnJoin implements control.SessionHandler.
func (a *Agent) OnJoin(_ context.Context, session *control.Session) error {
	a.workers.Add(session)

	// A worker that joins while a run is under way — a late starter, or one
	// reconnecting after a blip — needs its share immediately rather than at
	// the next quota change.
	a.mu.Lock()
	pending := a.run != nil
	a.mu.Unlock()

	if pending {
		go a.rebalance()
	}
	return nil
}

// OnLeave implements control.SessionHandler.
func (a *Agent) OnLeave(session *control.Session) {
	if !a.workers.Remove(session) {
		// The session was already displaced by a reconnection, which owns the
		// registry entry now. Rebalancing on its behalf would be wrong.
		return
	}

	a.mu.Lock()
	if a.run != nil {
		delete(a.run.dispatched, session.ID)
	}
	a.mu.Unlock()

	a.vuMu.Lock()
	delete(a.vuByWorker, session.ID)
	a.vuMu.Unlock()

	a.phaseMu.Lock()
	delete(a.workerPhase, session.ID)
	a.phaseMu.Unlock()
}

// OnHeartbeat implements control.SessionHandler.
//
// Each worker's VU count is kept separately and summed, rather than
// accumulated into a running total. A worker that dies between heartbeats
// simply stops contributing at the next recount, where an incremental total
// would keep its VUs on the books forever.
func (a *Agent) OnHeartbeat(session *control.Session, beat *loadwavev1.NodeHeartbeat) {
	a.vuMu.Lock()
	a.vuByWorker[session.ID] = beat.GetActiveVus()
	total := int64(0)
	for _, n := range a.vuByWorker {
		total += int64(n)
	}
	a.vuMu.Unlock()

	a.activeVUs.Store(total)
}

// OnMetrics implements control.SessionHandler.
//
// Batches are relayed to the coordinator unchanged, still carrying the
// worker's own node id. Merging them here would save a little bandwidth but
// would cost the operator the ability to see which process in a fleet is
// misbehaving, which is exactly the question a distributed run raises.
func (a *Agent) OnMetrics(_ *control.Session, batch *loadwavev1.MetricBatch) {
	if !a.upstream.SendMetrics(batch) {
		a.log.Warn("dropped a metric batch relaying to the coordinator", "run", batch.GetRunId())
	}
}

// OnRunStatus implements control.SessionHandler.
//
// Worker phases are folded into a single agent-level phase rather than relayed
// one by one. The coordinator decides a run is over when every participating
// agent reports a terminal phase, and forwarding each worker's status verbatim
// would have the agent's phase flapping between its workers' — so a run with
// one worker still going could look finished.
func (a *Agent) OnRunStatus(session *control.Session, update *loadwavev1.RunStatusUpdate) {
	a.log.Info("worker run status",
		"worker", session.ID, "run", update.GetRunId(),
		"phase", update.GetPhase(), "message", update.GetMessage())

	a.phaseMu.Lock()
	a.workerPhase[session.ID] = update.GetPhase()
	if n := update.GetCompletedIterations(); n > 0 {
		a.workerIters[session.ID] = n
	}
	phase, iterations := foldPhases(a.workerPhase, a.workerIters)
	a.phaseMu.Unlock()

	a.upstream.SendRunStatus(&loadwavev1.RunStatusUpdate{
		RunId:               update.GetRunId(),
		Phase:               phase,
		Message:             update.GetMessage(),
		ActiveVus:           uint32(a.activeVUs.Load()),
		CompletedIterations: iterations,
	})
}

// foldPhases reduces the worker phases to the one the agent reports.
//
// Any failure makes the agent's participation a failure, since a host that
// could not run its share did not run its share. Otherwise the agent is
// finished only when all of its workers are, and running while any still is.
func foldPhases(
	phases map[string]loadwavev1.RunPhase, iters map[string]uint64,
) (loadwavev1.RunPhase, uint64) {
	total := uint64(0)
	for _, n := range iters {
		total += n
	}
	if len(phases) == 0 {
		return loadwavev1.RunPhase_RUN_PHASE_PENDING, total
	}

	failed, completed := 0, 0
	for _, phase := range phases {
		switch phase {
		case loadwavev1.RunPhase_RUN_PHASE_FAILED:
			failed++
		case loadwavev1.RunPhase_RUN_PHASE_COMPLETED, loadwavev1.RunPhase_RUN_PHASE_ABORTED:
			completed++
		default:
			// Still working: pending, starting, running or stopping.
		}
	}

	switch {
	case failed > 0:
		return loadwavev1.RunPhase_RUN_PHASE_FAILED, total
	case completed == len(phases):
		return loadwavev1.RunPhase_RUN_PHASE_COMPLETED, total
	default:
		return loadwavev1.RunPhase_RUN_PHASE_RUNNING, total
	}
}

// OnLog implements control.SessionHandler.
func (a *Agent) OnLog(_ *control.Session, event *loadwavev1.LogEvent) {
	a.upstream.SendLog(event)
}

// timestampOf converts a wall-clock instant to its protobuf form, mapping the
// zero time to a nil message so that "unset" survives the round trip.
func timestampOf(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// timestampNow is timestampOf(time.Now()).
func timestampNow() *timestamppb.Timestamp { return timestamppb.Now() }

// reportStatus sends the agent's own run status upstream.
func (a *Agent) reportStatus(runID string, phase loadwavev1.RunPhase, message string) {
	a.upstream.SendRunStatus(&loadwavev1.RunStatusUpdate{
		RunId:   runID,
		Phase:   phase,
		Message: message,
	})
}

// reportLog forwards an agent-level event to the dashboard.
func (a *Agent) reportLog(level loadwavev1.LogLevel, runID, message string, fields map[string]string) {
	a.upstream.SendLog(&loadwavev1.LogEvent{
		Time:    timestampNow(),
		Level:   level,
		NodeId:  a.cfg.NodeID,
		RunId:   runID,
		Message: message,
		Fields:  fields,
	})
}
