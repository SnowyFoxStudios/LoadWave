// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine executes a load profile inside one worker process.
//
// It owns the virtual user pool: starting goroutines as the profile ramps up,
// stopping them as it ramps down, and driving each one through its scenario in
// a loop. Everything above it — agents, the coordinator, the dashboard — deals
// in quotas and aggregates; this is the only layer that runs user code.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/apportion"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// tickInterval is how often the engine re-evaluates the profile and resizes
// the pool. Fine enough that a ramp looks smooth, coarse enough that the
// bookkeeping stays invisible next to the load itself.
const tickInterval = 100 * time.Millisecond

// DefaultGracefulStop is how long in-flight iterations get to finish when a
// run ends, if the plan does not say.
const DefaultGracefulStop = 30 * time.Second

// Config is everything the engine needs to execute one node's share of a run.
type Config struct {
	RunID  string
	NodeID string

	Plan     *loadwavev1.TestPlan
	Registry *loadwave.Registry
	Recorder *metrics.Recorder
	HTTP     *loadwave.HTTPClientFactory
	Logger   *slog.Logger

	// StartAt is the instant, agreed across the whole fleet, that elapsed
	// time is measured from. The engine idles until it arrives, so that nodes
	// which received their orders at different moments still ramp together.
	StartAt time.Time

	// VUQuota is this node's virtual user count at the profile's peak.
	VUQuota int

	// IterationRateQuota caps iterations started per second on this node.
	// Zero means unlimited.
	IterationRateQuota int

	// IterationQuota stops this node after this many completed iterations.
	// Zero means unbounded.
	IterationQuota uint64

	// VUIDBase is the first virtual user id this node may allocate.
	VUIDBase int64

	// Shard identifies this node's slice of shared fixtures.
	Shard loadwave.Shard
}

// group is the set of virtual users assigned to one scenario.
//
// A virtual user belongs to a single scenario for its whole life rather than
// choosing afresh each iteration. That is what makes per-user state coherent:
// a user who logged in during OnVUStart stays logged in, and the session it
// holds belongs to the scenario that opened it.
type group struct {
	scenario loadwave.Scenario
	weight   int
	vus      []*vuHandle
}

// vuHandle is the engine's grip on one running virtual user.
type vuHandle struct {
	id int64

	// soft asks the user to stop once its current iteration finishes.
	soft context.CancelFunc
	// hard aborts it mid-iteration, cancelling any in-flight request.
	hard context.CancelFunc
}

// stopRequest carries an operator's or supervisor's decision to end the run.
type stopRequest struct {
	graceful bool
	reason   string
}

// Engine runs one node's share of a load profile.
type Engine struct {
	cfg Config
	log *slog.Logger

	// executor is never replaced after construction; its quota is mutated in
	// place, which is what lets a mid-ramp change ease from the current value.
	executor *scaled
	groups   []*group
	weights  []int
	graceful time.Duration
	planTags loadwave.Labels

	// limiter throttles iteration starts across every VU on this node. Nil
	// when the plan sets no arrival rate.
	limiter *rate.Limiter

	activeVUs  atomic.Int64
	iterations atomic.Uint64
	nextVUID   atomic.Int64

	// mu guards the groups' virtual user slices, which the control loop
	// resizes and nothing else touches.
	mu sync.Mutex

	stop     chan stopRequest
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New validates the configuration and prepares the engine. It starts nothing;
// Run does that.
func New(cfg Config) (*Engine, error) {
	if cfg.Plan == nil {
		return nil, errors.New("engine needs a test plan")
	}
	if cfg.Registry == nil {
		return nil, errors.New("engine needs a scenario registry")
	}
	if cfg.Recorder == nil {
		return nil, errors.New("engine needs a metric recorder")
	}
	if cfg.HTTP == nil {
		return nil, errors.New("engine needs an HTTP client factory")
	}

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("run", cfg.RunID, "node", cfg.NodeID)

	base, err := NewExecutor(cfg.Plan.GetLoad())
	if err != nil {
		return nil, err
	}

	groups, weights, err := resolveScenarios(cfg.Registry, cfg.Plan.GetScenarios())
	if err != nil {
		return nil, err
	}

	graceful := cfg.Plan.GetLoad().GetGracefulStop().AsDuration()
	if graceful <= 0 {
		graceful = DefaultGracefulStop
	}

	e := &Engine{
		cfg:      cfg,
		log:      log,
		executor: newScaled(base, cfg.VUQuota),
		groups:   groups,
		weights:  weights,
		graceful: graceful,
		planTags: loadwave.LabelsFromMap(cfg.Plan.GetTags()),
		stop:     make(chan stopRequest, 1),
	}
	e.nextVUID.Store(cfg.VUIDBase)

	if cfg.IterationRateQuota > 0 {
		limit := rate.Limit(cfg.IterationRateQuota)
		// A burst equal to one second of budget lets the pool absorb the
		// jitter of thousands of goroutines waking at once without letting
		// the average drift above the requested rate.
		e.limiter = rate.NewLimiter(limit, max(1, cfg.IterationRateQuota))
	}

	return e, nil
}

// resolveScenarios looks the plan's scenario references up in the registry.
func resolveScenarios(
	registry *loadwave.Registry, refs []*loadwavev1.ScenarioRef,
) ([]*group, []int, error) {
	// An empty reference list means "everything this binary knows about",
	// which is what a single-scenario test wants and saves it a config line.
	if len(refs) == 0 {
		names := registry.Names()
		if len(names) == 0 {
			return nil, nil, errors.New("no scenarios are registered")
		}
		refs = make([]*loadwavev1.ScenarioRef, 0, len(names))
		for _, name := range names {
			refs = append(refs, &loadwavev1.ScenarioRef{Name: name, Weight: 1})
		}
	}

	groups := make([]*group, 0, len(refs))
	weights := make([]int, 0, len(refs))

	for _, ref := range refs {
		scenario, ok := registry.Lookup(ref.GetName())
		if !ok {
			return nil, nil, fmt.Errorf(
				"scenario %q is not registered in this binary (known: %v)",
				ref.GetName(), registry.Names())
		}
		weight := int(ref.GetWeight())
		if weight <= 0 {
			weight = scenario.EffectiveWeight()
		}
		groups = append(groups, &group{scenario: scenario, weight: weight})
		weights = append(weights, weight)
	}
	return groups, weights, nil
}

// ActiveVUs reports how many virtual users are running right now.
func (e *Engine) ActiveVUs() int { return int(e.activeVUs.Load()) }

// Iterations reports how many iterations have completed on this node.
func (e *Engine) Iterations() uint64 { return e.iterations.Load() }

// Describe renders this node's share of the profile.
func (e *Engine) Describe() string { return e.executor.Describe() }

// Stop ends the run. A graceful stop lets in-flight iterations finish within
// the plan's grace window; otherwise they are cancelled at once. Calling Stop
// more than once has no additional effect.
func (e *Engine) Stop(graceful bool, reason string) {
	e.stopOnce.Do(func() {
		e.stop <- stopRequest{graceful: graceful, reason: reason}
	})
}

// SetQuota rescales this node's share while the run is in progress, either
// because an operator changed the target or because the fleet changed size.
//
// A non-zero ramp introduces the change gradually rather than in one tick. The
// executor is mutated rather than replaced, so a change arriving mid-ramp
// eases from wherever the quota actually is instead of snapping back.
func (e *Engine) SetQuota(vuQuota, iterationRateQuota int, ramp time.Duration) {
	if vuQuota >= 0 {
		e.executor.Rescale(vuQuota, e.elapsed(), ramp)
	}
	if iterationRateQuota > 0 && e.limiter != nil {
		e.limiter.SetLimit(rate.Limit(iterationRateQuota))
		e.limiter.SetBurst(max(1, iterationRateQuota))
	}
	e.log.Info("quota updated", "vus", vuQuota, "iterationRate", iterationRateQuota, "ramp", ramp)
}

// elapsed is how long the run has been going, which is the clock every part of
// the profile is expressed against.
func (e *Engine) elapsed() time.Duration {
	if e.cfg.StartAt.IsZero() {
		return 0
	}
	return time.Since(e.cfg.StartAt)
}

// Run executes the profile and returns when the run has finished and every
// virtual user has stopped.
//
// It returns nil for a run that completed or was stopped on request, and an
// error only when the run could not be carried out — a scenario's Setup
// failing, say. A load test full of HTTP 500s is a successful run with a bad
// result, and the distinction matters to anything scripting this.
func (e *Engine) Run(ctx context.Context) error {
	hardCtx, hardCancel := context.WithCancel(ctx)
	defer hardCancel()

	if err := e.setup(hardCtx); err != nil {
		return err
	}
	defer e.teardown(ctx)

	if err := e.awaitStart(hardCtx); err != nil {
		return nil // cancelled before the run began; not a failure
	}

	e.log.Info("run started",
		"profile", e.executor.Describe(),
		"scenarios", len(e.groups),
		"gracefulStop", e.graceful)

	reason := e.control(hardCtx)
	e.shutdown(hardCtx, hardCancel, reason)

	e.log.Info("run finished",
		"reason", reason.reason,
		"iterations", e.iterations.Load(),
		"droppedSamples", e.cfg.Recorder.Dropped())
	return nil
}

// setup runs every scenario's Setup hook.
func (e *Engine) setup(ctx context.Context) error {
	for _, g := range e.groups {
		if g.scenario.Setup == nil {
			continue
		}
		if err := g.scenario.Setup(ctx); err != nil {
			return fmt.Errorf("scenario %q setup: %w", g.scenario.Name, err)
		}
	}
	return nil
}

// teardown runs every scenario's Teardown hook.
//
// It deliberately does not use the run's context: by the time teardown runs
// that context is usually already cancelled, and a teardown that cannot make
// any calls is not much of a teardown. It gets its own budget instead.
func (e *Engine) teardown(ctx context.Context) {
	tdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.graceful)
	defer cancel()

	for _, g := range e.groups {
		if g.scenario.Teardown == nil {
			continue
		}
		if err := g.scenario.Teardown(tdCtx); err != nil {
			e.log.Error("scenario teardown failed", "scenario", g.scenario.Name, "error", err)
		}
	}
}

// awaitStart blocks until the agreed start instant.
func (e *Engine) awaitStart(ctx context.Context) error {
	wait := time.Until(e.cfg.StartAt)
	if e.cfg.StartAt.IsZero() || wait <= 0 {
		return nil
	}

	e.log.Info("waiting for the agreed start instant", "in", wait.Round(time.Millisecond))
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case req := <-e.stop:
		return fmt.Errorf("stopped before start: %s", req.reason)
	}
}

// control is the main loop: it resizes the pool to follow the profile and
// watches for the conditions that end the run.
func (e *Engine) control(ctx context.Context) stopRequest {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return stopRequest{graceful: false, reason: "cancelled"}

		case req := <-e.stop:
			return req

		case now := <-ticker.C:
			elapsed := now.Sub(e.cfg.StartAt)
			executor := e.executor

			if d := executor.Duration(); d > 0 && elapsed >= d {
				return stopRequest{graceful: true, reason: "profile completed"}
			}
			if q := e.cfg.IterationQuota; q > 0 && e.iterations.Load() >= q {
				return stopRequest{graceful: true, reason: "iteration budget spent"}
			}

			e.resize(ctx, executor.TargetAt(elapsed))
			e.reportGauges()
		}
	}
}

// resize brings the pool to the requested total, apportioned across scenarios
// by weight.
func (e *Engine) resize(ctx context.Context, total int) {
	if total < 0 {
		total = 0
	}
	shares := apportion.Largest(total, e.weights)

	e.mu.Lock()
	defer e.mu.Unlock()

	for i, g := range e.groups {
		want := shares[i]
		for len(g.vus) < want {
			e.startVU(ctx, g)
		}
		for len(g.vus) > want {
			last := len(g.vus) - 1
			e.retireVU(g.vus[last])
			g.vus = g.vus[:last]
		}
	}
}

// startVU launches one virtual user. Called with e.mu held.
func (e *Engine) startVU(ctx context.Context, g *group) {
	id := e.nextVUID.Add(1) - 1

	hardCtx, hardCancel := context.WithCancel(ctx)
	softCtx, softCancel := context.WithCancel(hardCtx)

	vu := loadwave.NewVU(loadwave.VUConfig{
		ID:       id,
		Index:    len(g.vus),
		Shard:    e.cfg.Shard,
		Scenario: g.scenario.Name,
		Recorder: e.cfg.Recorder.ForVU(id),
		HTTP:     e.cfg.HTTP.New(),
		Logger:   e.log,
		Tags:     e.planTags,
	})

	handle := &vuHandle{id: id, soft: softCancel, hard: hardCancel}
	g.vus = append(g.vus, handle)

	e.wg.Add(1)
	e.activeVUs.Add(1)

	go func() {
		defer e.wg.Done()
		defer e.activeVUs.Add(-1)
		defer hardCancel()
		defer func() { _ = vu.Close() }()

		e.driveVU(hardCtx, softCtx, vu, g.scenario)
	}()
}

// retireVU asks a virtual user to stop, then guarantees it does.
//
// The soft cancel lets the current iteration finish so that a scale-down does
// not manufacture failures out of requests that were about to succeed. The
// timer is the backstop: a scenario that ignores its context, or an endpoint
// that never answers, must not be able to hold the pool open indefinitely.
func (e *Engine) retireVU(h *vuHandle) {
	h.soft()
	time.AfterFunc(e.graceful, h.hard)
}

// driveVU runs one virtual user's whole life: start hook, iteration loop,
// stop hook.
func (e *Engine) driveVU(hardCtx, softCtx context.Context, vu *loadwave.VU, sc loadwave.Scenario) {
	if sc.OnVUStart != nil {
		if err := e.guard(sc.Name, vu, "OnVUStart", func() error {
			return sc.OnVUStart(hardCtx, vu)
		}); err != nil {
			e.log.Error("virtual user failed to start", "scenario", sc.Name, "vu", vu.ID(), "error", err)
			return
		}
	}

	if sc.OnVUStop != nil {
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(hardCtx), e.graceful)
			defer cancel()
			_ = e.guard(sc.Name, vu, "OnVUStop", func() error {
				return sc.OnVUStop(stopCtx, vu)
			})
		}()
	}

	e.iterate(hardCtx, softCtx, vu, sc)
}

// iterate loops the scenario until the virtual user is told to stop.
func (e *Engine) iterate(hardCtx, softCtx context.Context, vu *loadwave.VU, sc loadwave.Scenario) {
	for n := 0; ; n++ {
		if softCtx.Err() != nil {
			return
		}
		if q := e.cfg.IterationQuota; q > 0 && e.iterations.Load() >= q {
			return
		}
		if e.limiter != nil {
			// Waiting on the soft context means a stopping run does not have
			// to sit through the remaining rate-limit delay before its VUs
			// notice they should exit.
			if err := e.limiter.Wait(softCtx); err != nil {
				return
			}
		}

		vu.BeginIteration(n)
		started := time.Now()
		err := e.guard(sc.Name, vu, "Run", func() error { return sc.Run(hardCtx, vu) })
		vu.EndIteration(time.Since(started), err)
		e.iterations.Add(1)
	}
}

// guard runs a scenario callback, converting a panic into an error.
//
// Scenarios are contributor-written code doing unfamiliar things to
// unfamiliar responses, and a nil map access in one of ten thousand virtual
// users must not take down a load test that has been running for an hour. The
// panic is logged with its stack and charged to the iteration as a failure.
func (e *Engine) guard(scenario string, vu *loadwave.VU, hook string, fn func() error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err = fmt.Errorf("scenario %q panicked in %s: %v", scenario, hook, r)
		e.log.Error("scenario panicked",
			"scenario", scenario,
			"hook", hook,
			"vu", vu.ID(),
			"panic", r,
			"stack", string(debug.Stack()))
	}()
	return fn()
}

// reportGauges publishes the current VU count per scenario.
func (e *Engine) reportGauges() {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, g := range e.groups {
		labels := e.planTags.With(loadwave.LabelScenario, g.scenario.Name)
		e.cfg.Recorder.Gauge(loadwave.MetricVUs, labels, float64(len(g.vus)))
	}
}

// shutdown drains the pool and waits for every virtual user to exit.
func (e *Engine) shutdown(ctx context.Context, hardCancel context.CancelFunc, req stopRequest) {
	e.log.Info("stopping", "graceful", req.graceful, "reason", req.reason)

	e.mu.Lock()
	for _, g := range e.groups {
		for _, h := range g.vus {
			if req.graceful {
				h.soft()
			} else {
				h.hard()
			}
		}
		g.vus = nil
	}
	e.mu.Unlock()

	if !req.graceful {
		hardCancel()
		e.wg.Wait()
		return
	}

	drained := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(drained)
	}()

	timer := time.NewTimer(e.graceful)
	defer timer.Stop()

	select {
	case <-drained:
	case <-timer.C:
		e.log.Warn("virtual users did not finish within the grace period; cancelling",
			"grace", e.graceful, "remaining", e.activeVUs.Load())
		hardCancel()
		e.wg.Wait()
	case <-ctx.Done():
		hardCancel()
		e.wg.Wait()
	}
}
