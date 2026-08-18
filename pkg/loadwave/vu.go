// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Shard tells a virtual user which slice of shared test data belongs to it.
//
// A distributed run must not have every node hammering the same fixture row,
// and coordinating that at runtime would mean chatter on the hot path.
// Instead the coordinator hands each node a static (Index, Count) pair at
// start, and nodes partition data arithmetically with no further
// communication.
type Shard struct {
	Index uint32
	Count uint32
}

// Owns reports whether this shard is responsible for item i.
func (s Shard) Owns(i int) bool {
	if s.Count <= 1 {
		return true
	}
	return uint32(i)%s.Count == s.Index
}

// Slice returns the elements of items belonging to this shard, preserving
// order. The result aliases nothing: callers may mutate it freely.
func Slice[T any](s Shard, items []T) []T {
	if s.Count <= 1 {
		return items
	}
	out := make([]T, 0, len(items)/int(s.Count)+1)
	for i, item := range items {
		if s.Owns(i) {
			out = append(out, item)
		}
	}
	return out
}

// VU is one virtual user: a single simulated client, executing its scenario
// in a loop for the lifetime of the run.
//
// Exactly one goroutine ever touches a given VU, so nothing on it is
// synchronised and scenarios may store whatever they like on it without
// locking. Do not hand a VU to a goroutine you spawn yourself; if a scenario
// needs concurrency within one iteration, share only immutable values.
type VU struct {
	id       int64
	index    int
	shard    Shard
	scenario string

	rec     Recorder
	http    *HTTPClient
	rnd     *rand.Rand
	log     *slog.Logger
	baseTag Labels

	// state carries per-VU scenario data across iterations, populated by
	// OnVUStart. Lazily allocated: most scenarios never use it.
	state map[string]any

	// Mutable per-iteration bookkeeping.
	iteration int
	iterTag   Labels
	thinkTime time.Duration
	failed    bool
}

// VUConfig is the set of dependencies needed to construct a VU.
//
// The engine fills this in for real runs. It is exported so scenario authors
// can build a VU in their own unit tests and exercise scenario logic without
// standing up a coordinator.
type VUConfig struct {
	// ID is unique across the entire run, not just this process. The
	// coordinator allocates a distinct range to every worker.
	ID int64
	// Index is this VU's position within its worker process, from 0.
	Index int
	// Shard identifies which slice of shared fixtures this VU should use.
	Shard Shard
	// Scenario is the name of the scenario this VU executes.
	Scenario string
	// Recorder receives all metric observations. Defaults to discarding.
	Recorder Recorder
	// HTTP is the client scenarios reach through VU.HTTP.
	HTTP *HTTPClient
	// Logger receives scenario log output. Defaults to slog.Default.
	Logger *slog.Logger
	// Rand seeds this VU's generator. Defaults to a per-VU deterministic
	// source derived from ID, which keeps runs reproducible.
	Rand *rand.Rand
	// Tags are attached to every metric this VU emits, on top of the
	// automatic scenario tag.
	Tags Labels
}

// NewVU constructs a virtual user.
func NewVU(cfg VUConfig) *VU {
	rec := cfg.Recorder
	if rec == nil {
		rec = DiscardRecorder()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	rnd := cfg.Rand
	if rnd == nil {
		// Deriving the seed from the VU id rather than the wall clock means
		// two runs with the same shape make the same random choices, which
		// makes a regression reproducible instead of a coin flip.
		rnd = rand.New(rand.NewPCG(uint64(cfg.ID), 0x9E3779B97F4A7C15))
	}

	base := cfg.Tags
	if cfg.Scenario != "" {
		base = base.With(LabelScenario, cfg.Scenario)
	}

	vu := &VU{
		id:       cfg.ID,
		index:    cfg.Index,
		shard:    cfg.Shard,
		scenario: cfg.Scenario,
		rec:      rec,
		http:     cfg.HTTP,
		rnd:      rnd,
		log:      log.With("vu", cfg.ID, "scenario", cfg.Scenario),
		baseTag:  base,
		iterTag:  base,
	}
	if vu.http != nil {
		vu.http.attach(vu)
	}
	return vu
}

// ID returns the run-wide unique identifier of this virtual user.
func (vu *VU) ID() int64 { return vu.id }

// Index returns the VU's position within its worker process.
func (vu *VU) Index() int { return vu.index }

// Shard returns the data partition assigned to this VU.
func (vu *VU) Shard() Shard { return vu.shard }

// Scenario returns the name of the scenario being executed.
func (vu *VU) Scenario() string { return vu.scenario }

// Iteration returns the zero-based index of the current iteration.
func (vu *VU) Iteration() int { return vu.iteration }

// HTTP returns the VU's HTTP client.
//
// It panics if the run was configured without one, which only happens in
// hand-built test VUs; that is a clearer failure than a nil dereference deep
// inside a scenario.
func (vu *VU) HTTP() *HTTPClient {
	if vu.http == nil {
		panic("loadwave: VU has no HTTP client configured")
	}
	return vu.http
}

// Metrics returns the recorder, for scenarios emitting custom metrics.
func (vu *VU) Metrics() Recorder { return vu.rec }

// Rand returns this VU's random source. It is not shared with any other VU,
// so it needs no locking and contributes no contention.
func (vu *VU) Rand() *rand.Rand { return vu.rnd }

// Log returns a logger already tagged with the VU id and scenario.
func (vu *VU) Log() *slog.Logger { return vu.log }

// Labels returns the tags currently applied to this VU's observations.
func (vu *VU) Labels() Labels { return vu.iterTag }

// Tag adds a label to every metric this VU emits for the rest of the current
// iteration. It is reset when the iteration ends.
//
// Keep the value space small. A tag with unbounded values — a user id, a
// timestamp — creates a new time series per value and will exhaust memory on
// the coordinator.
func (vu *VU) Tag(key, value string) {
	vu.iterTag = vu.iterTag.With(key, value)
}

// SetState stores a value that survives across iterations of this VU.
func (vu *VU) SetState(key string, value any) {
	if vu.state == nil {
		vu.state = make(map[string]any, 4)
	}
	vu.state[key] = value
}

// State retrieves a value stored by SetState.
func (vu *VU) State(key string) (any, bool) {
	v, ok := vu.state[key]
	return v, ok
}

// StateOf is the generic form of State, returning the zero value of T when the
// key is absent or holds a different type.
func StateOf[T any](vu *VU, key string) (T, bool) {
	var zero T
	v, ok := vu.state[key]
	if !ok {
		return zero, false
	}
	typed, ok := v.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}

// Check records a named assertion and reports the result back to the caller,
// so it composes into control flow:
//
//	if !vu.Check("logged in", resp.StatusCode == 200) {
//	    return fmt.Errorf("login failed: %d", resp.StatusCode)
//	}
//
// A failing check does not by itself fail the iteration. Return an error from
// the scenario to do that.
func (vu *VU) Check(name string, ok bool) bool {
	vu.rec.Rate(MetricChecks, vu.iterTag.With(LabelCheck, name), ok)
	if !ok {
		vu.log.Debug("check failed", "check", name, "iteration", vu.iteration)
	}
	return ok
}

// Checkf is Check with a message logged when the assertion fails. The message
// is not used as a metric label, so it may safely include specific values.
func (vu *VU) Checkf(name string, ok bool, format string, args ...any) bool {
	if !ok {
		vu.log.Debug("check failed",
			"check", name,
			"iteration", vu.iteration,
			"detail", fmt.Sprintf(format, args...))
	}
	vu.rec.Rate(MetricChecks, vu.iterTag.With(LabelCheck, name), ok)
	return ok
}

// Fail records an error against this iteration. Returning an error from the
// scenario's Run does the same thing; Fail is for recording an additional
// error without abandoning the iteration.
func (vu *VU) Fail(err error) {
	if err == nil {
		return
	}
	vu.failed = true
	vu.rec.Count(MetricErrors, vu.iterTag, 1)
	vu.log.Debug("iteration error", "error", err, "iteration", vu.iteration)
}

// Think pauses the virtual user, simulating a real person reading the page.
//
// The pause is interruptible: when the run is stopping, Think returns early
// rather than holding the shutdown open. Time spent here is excluded from
// iteration_duration, so think time does not distort the metric.
func (vu *VU) Think(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	start := time.Now()
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	vu.thinkTime += time.Since(start)
}

// ThinkBetween pauses for a duration drawn uniformly from [minDur, maxDur].
// Constant think times make virtual users march in lockstep and produce
// artificial traffic spikes; jitter is almost always what you want.
func (vu *VU) ThinkBetween(ctx context.Context, minDur, maxDur time.Duration) {
	if maxDur <= minDur {
		vu.Think(ctx, minDur)
		return
	}
	spread := int64(maxDur - minDur)
	vu.Think(ctx, minDur+time.Duration(vu.rnd.Int64N(spread)))
}

// BeginIteration resets per-iteration state. The engine calls this; scenarios
// do not.
func (vu *VU) BeginIteration(n int) {
	vu.iteration = n
	vu.iterTag = vu.baseTag
	vu.thinkTime = 0
	vu.failed = false
}

// EndIteration reports the accounting for the iteration just finished: the
// time it took excluding think time, and whether it failed. The engine calls
// this; scenarios do not.
func (vu *VU) EndIteration(elapsed time.Duration, err error) {
	active := elapsed - vu.thinkTime
	if active < 0 {
		active = 0
	}
	failed := vu.failed || err != nil

	vu.rec.Count(MetricIterations, vu.baseTag, 1)
	vu.rec.Trend(MetricIterationDuration, vu.baseTag, float64(active.Nanoseconds())/1e6)
	vu.rec.Rate(MetricIterationFailed, vu.baseTag, failed)

	if err != nil {
		vu.rec.Count(MetricErrors, vu.baseTag, 1)
	}
}

// Close releases resources held by the VU, such as idle HTTP connections.
func (vu *VU) Close() error {
	if vu.http != nil {
		return vu.http.close()
	}
	return nil
}

// assert that VU does not accidentally satisfy io.Closer's error contract in a
// way that hides failures.
var _ io.Closer = (*VU)(nil)
