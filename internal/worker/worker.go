// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package worker is the process that actually generates load.
//
// A worker joins its local agent over a Unix socket, waits to be told about a
// run, and then drives a pool of virtual users through the engine. It is a
// separate OS process on purpose: virtual users are goroutines, and past a few
// thousand of them one Go runtime's scheduler and garbage collector become the
// bottleneck rather than the system under test. Splitting the pool across
// processes buys back that headroom, and means one panicking scenario cannot
// take the whole generator down with it.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/internal/control"
	"github.com/SnowyFoxStudios/LoadWave/internal/engine"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/internal/scenario"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Config describes a worker process.
type Config struct {
	// NodeID is unique across the whole run. The agent assigns it.
	NodeID string

	// AgentTarget is the agent's control socket, as a gRPC target.
	AgentTarget string

	// Registry holds the scenarios compiled into this binary.
	Registry *loadwave.Registry

	// MaxVUs caps how many virtual users this worker will accept.
	MaxVUs int

	Logger *slog.Logger
}

// DefaultMaxVUs is the ceiling a worker advertises when the operator has not
// set one. Well above what a single process is likely to be given, since the
// coordinator's apportionment is the real control.
const DefaultMaxVUs = 20000

// activeRun is the state of the run a worker is currently executing.
type activeRun struct {
	id       string
	engine   *engine.Engine
	recorder *metrics.Recorder
	http     *loadwave.HTTPClientFactory
	cancel   context.CancelFunc
	done     chan struct{}
}

// Worker executes one process's share of a run.
type Worker struct {
	cfg    Config
	log    *slog.Logger
	client *control.Client

	// metricsInterval is set by the agent when the worker joins.
	metricsInterval atomic.Int64

	// base is the worker process's lifetime context, captured when Run
	// starts. Runs derive from it so that shutting the worker down cancels
	// whatever it is executing, rather than leaving an orphaned engine
	// attached to context.Background().
	base context.Context

	mu  sync.Mutex
	run *activeRun
}

// New prepares a worker. It does not connect; Run does that.
func New(cfg Config) (*Worker, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("worker needs a node id")
	}
	if cfg.AgentTarget == "" {
		return nil, errors.New("worker needs an agent target")
	}
	if cfg.Registry == nil {
		cfg.Registry = loadwave.Default
	}
	if cfg.MaxVUs <= 0 {
		cfg.MaxVUs = DefaultMaxVUs
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	w := &Worker{cfg: cfg, log: cfg.Logger.With("worker", cfg.NodeID)}
	w.metricsInterval.Store(int64(control.DefaultMetricsInterval))

	hostname, _ := os.Hostname()
	client, err := control.NewClient(control.ClientConfig{
		Target:  cfg.AgentTarget,
		Handler: w,
		Logger:  w.log,
		Hello: &loadwavev1.NodeHello{
			NodeId:     cfg.NodeID,
			Hostname:   hostname,
			Version:    buildinfo.Version(),
			CpuCores:   uint32(runtime.NumCPU()),
			MaxWorkers: 0, // a worker supervises nothing
			MaxVus:     uint32(cfg.MaxVUs),
		},
		Heartbeat: w.heartbeat,
	})
	if err != nil {
		return nil, err
	}
	w.client = client
	return w, nil
}

// Run joins the agent and serves until the context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.base = ctx
	defer w.stopRun(true, "worker shutting down")
	return w.client.Run(ctx)
}

// heartbeat reports this worker's current load.
func (w *Worker) heartbeat() *loadwavev1.NodeHeartbeat {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	beat := &loadwavev1.NodeHeartbeat{MemBytes: memStats.HeapAlloc}
	if run := w.currentRun(); run != nil {
		beat.ActiveVus = uint32(run.engine.ActiveVUs())
	}
	return beat
}

// currentRun returns the active run, or nil.
func (w *Worker) currentRun() *activeRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.run
}

// OnAccepted implements control.Handler.
func (w *Worker) OnAccepted(_ context.Context, msg *loadwavev1.Accepted) error {
	if interval := msg.GetMetricsInterval().AsDuration(); interval > 0 {
		w.metricsInterval.Store(int64(interval))
	}
	return nil
}

// OnStartRun implements control.Handler.
//
// It builds the engine synchronously so that a misconfigured plan is reported
// as a failure straight away, then hands execution to a goroutine — the
// control stream's receive loop must stay responsive, not least so that a
// stop command can still get through.
func (w *Worker) OnStartRun(_ context.Context, msg *loadwavev1.StartRun) error {
	w.stopRun(false, "superseded by a new run")

	run, err := w.build(msg)
	if err != nil {
		w.log.Error("cannot start run", "run", msg.GetRunId(), "error", err)
		w.reportStatus(msg.GetRunId(), loadwavev1.RunPhase_RUN_PHASE_FAILED, err.Error(), 0)
		return err
	}

	w.mu.Lock()
	w.run = run
	w.mu.Unlock()

	w.reportStatus(run.id, loadwavev1.RunPhase_RUN_PHASE_STARTING, "engine built", 0)

	// The handler's context covers delivery of this one message, not the run
	// it starts; execute derives from the worker's own lifetime instead.
	//nolint:contextcheck // see above
	go w.execute(run)
	return nil
}

// build assembles everything the run needs.
func (w *Worker) build(msg *loadwavev1.StartRun) (*activeRun, error) {
	plan := msg.GetPlan()
	if plan == nil {
		return nil, errors.New("start-run carried no plan")
	}

	cfg, err := scenario.FromPlan(plan)
	if err != nil {
		return nil, err
	}

	// A fresh registry per run: declarative scenarios defined in this run's
	// configuration must not linger and collide with the next one.
	registry := w.cfg.Registry.Clone()
	if err := cfg.BuildScenarios(registry); err != nil {
		return nil, err
	}

	httpOptions := cfg.HTTPOptions()
	if httpOptions.BaseURL == "" {
		httpOptions.BaseURL = plan.GetBaseUrl()
	}
	httpFactory, err := loadwave.NewHTTPClientFactory(httpOptions)
	if err != nil {
		return nil, err
	}

	recorder := metrics.NewRecorder(metrics.RecorderConfig{
		Histogram: metrics.DefaultHistogramConfig(),
	})

	eng, err := engine.New(engine.Config{
		RunID:              msg.GetRunId(),
		NodeID:             w.cfg.NodeID,
		Plan:               plan,
		Registry:           registry,
		Recorder:           recorder,
		HTTP:               httpFactory,
		Logger:             w.log,
		StartAt:            msg.GetStartAt().AsTime(),
		VUQuota:            int(msg.GetVuQuota()),
		IterationRateQuota: int(msg.GetIterationRateQuota()),
		IterationQuota:     msg.GetIterationQuota(),
		VUIDBase:           msg.GetVuIdBase(),
		Shard: loadwave.Shard{
			Index: msg.GetShardIndex(),
			Count: msg.GetShardCount(),
		},
	})
	if err != nil {
		httpFactory.Close()
		return nil, err
	}

	return &activeRun{
		id:       msg.GetRunId(),
		engine:   eng,
		recorder: recorder,
		http:     httpFactory,
		done:     make(chan struct{}),
	}, nil
}

// execute drives the engine and reports throughout.
func (w *Worker) execute(run *activeRun) {
	defer close(run.done)
	defer run.http.Close()

	// Derived from the worker's lifetime rather than the background context,
	// so that killing the worker cancels the run it is executing. It is not
	// derived from the OnStartRun call's context, which ends the moment that
	// message has been handled.
	parent := w.base
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	run.cancel = cancel
	defer cancel()

	// A cancelled worker asks its engine to stop gracefully rather than
	// having the engine's context yanked mid-request.
	stopOnShutdown := context.AfterFunc(parent, func() {
		run.engine.Stop(true, "worker shutting down")
	})
	defer stopOnShutdown()

	reporting := make(chan struct{})
	go func() {
		defer close(reporting)
		w.report(ctx, run)
	}()

	w.reportStatus(run.id, loadwavev1.RunPhase_RUN_PHASE_RUNNING, run.engine.Describe(), 0)

	err := run.engine.Run(ctx)

	// Stop the reporting loop and let it emit one final batch, so the last
	// few hundred milliseconds of a run are not silently discarded.
	cancel()
	<-reporting

	phase := loadwavev1.RunPhase_RUN_PHASE_COMPLETED
	message := "completed"
	if err != nil {
		phase = loadwavev1.RunPhase_RUN_PHASE_FAILED
		message = err.Error()
		w.log.Error("run failed", "run", run.id, "error", err)
	}
	w.reportStatus(run.id, phase, message, run.engine.Iterations())

	w.mu.Lock()
	if w.run == run {
		w.run = nil
	}
	w.mu.Unlock()
}

// report flushes metrics on wall-clock-aligned boundaries.
//
// Alignment is what lets the coordinator merge batches from many hosts without
// interpolating: every node cuts its buckets at the same instants, so a p99
// for 12:00:03 is genuinely everyone's traffic in that second. Clocks still
// have to be roughly in step, which NTP handles; the coordinator's grace
// window absorbs the rest.
func (w *Worker) report(ctx context.Context, run *activeRun) {
	interval := time.Duration(w.metricsInterval.Load())
	if interval <= 0 {
		interval = control.DefaultMetricsInterval
	}

	next := time.Now().Truncate(interval).Add(interval)
	for {
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			w.flush(run, next.Add(-interval), interval)
			return
		case <-timer.C:
		}

		w.flush(run, next.Add(-interval), interval)
		next = next.Add(interval)

		// Under a long stall — a stop-the-world pause, or a badly
		// oversubscribed host — emitting a burst of stale buckets would be
		// worse than skipping them, since they would arrive past the
		// coordinator's grace window anyway.
		if now := time.Now(); next.Before(now) {
			next = now.Truncate(interval).Add(interval)
		}
	}
}

// flush drains the recorder and sends one batch upstream.
func (w *Worker) flush(run *activeRun, bucketStart time.Time, width time.Duration) {
	batch := run.recorder.Flush(
		run.id, w.cfg.NodeID, bucketStart, width, uint32(run.engine.ActiveVUs()))

	if !w.client.SendMetrics(batch) {
		w.log.Warn("dropped a metric batch: the control queue is full",
			"run", run.id, "bucket", bucketStart)
	}
}

// OnSetQuota implements control.Handler.
func (w *Worker) OnSetQuota(_ context.Context, msg *loadwavev1.SetQuota) error {
	run := w.currentRun()
	if run == nil || run.id != msg.GetRunId() {
		return fmt.Errorf("no run %q is active on this worker", msg.GetRunId())
	}
	run.engine.SetQuota(
		int(msg.GetVuQuota()),
		int(msg.GetIterationRateQuota()),
		msg.GetRamp().AsDuration(),
	)
	return nil
}

// OnStopRun implements control.Handler.
func (w *Worker) OnStopRun(_ context.Context, msg *loadwavev1.StopRun) error {
	run := w.currentRun()
	if run == nil {
		return nil
	}
	if msg.GetRunId() != "" && run.id != msg.GetRunId() {
		return fmt.Errorf("no run %q is active on this worker", msg.GetRunId())
	}

	reason := msg.GetReason()
	if reason == "" {
		reason = "stop requested"
	}
	run.engine.Stop(msg.GetGraceful(), reason)
	return nil
}

// stopRun ends the active run and waits for it to unwind.
func (w *Worker) stopRun(graceful bool, reason string) {
	run := w.currentRun()
	if run == nil {
		return
	}

	run.engine.Stop(graceful, reason)
	if !graceful && run.cancel != nil {
		run.cancel()
	}
	<-run.done
}

// reportStatus sends a run status update upstream.
func (w *Worker) reportStatus(
	runID string, phase loadwavev1.RunPhase, message string, iterations uint64,
) {
	var vus uint32
	if run := w.currentRun(); run != nil {
		vus = uint32(run.engine.ActiveVUs())
	}

	w.client.SendRunStatus(&loadwavev1.RunStatusUpdate{
		RunId:               runID,
		Phase:               phase,
		Message:             message,
		ActiveVus:           vus,
		CompletedIterations: iterations,
	})
}

// LogEvent forwards a notable event to the agent, and on to the dashboard.
func (w *Worker) LogEvent(level loadwavev1.LogLevel, runID, message string, fields map[string]string) {
	w.client.SendLog(&loadwavev1.LogEvent{
		Time:    timestamppb.Now(),
		Level:   level,
		NodeId:  w.cfg.NodeID,
		RunId:   runID,
		Message: message,
		Fields:  fields,
	})
}
