// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"sync"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// Update types pushed to live subscribers.
const (
	UpdateTick  = "tick"
	UpdateEvent = "event"
)

// Update is one message on the live stream.
type Update struct {
	Type       string            `json:"type"`
	Run        *Summary          `json:"run,omitempty"`
	Agents     []AgentInfo       `json:"agents,omitempty"`
	Ticks      []TickDTO         `json:"ticks,omitempty"`
	Thresholds []ThresholdResult `json:"thresholds,omitempty"`
	Events     []Event           `json:"events,omitempty"`
}

// EndpointTick is one endpoint's slice of a time bucket.
//
// Average only, no percentiles: keeping a histogram per endpoint per second
// is what the store deliberately does not do. Whole-run percentiles per
// endpoint are in Snapshot.Endpoints.
type EndpointTick struct {
	Avg       float64 `json:"avg"`
	Requests  uint64  `json:"requests"`
	ErrorRate float64 `json:"errorRate"`
}

// ScenarioTick is one scenario's slice of a time bucket.
type ScenarioTick struct {
	VUs        float64 `json:"vus"`
	Iterations uint64  `json:"iterations"`
	Requests   uint64  `json:"requests"`
	ErrorRate  float64 `json:"errorRate"`
	P95        float64 `json:"p95"`
}

// TickDTO is one second of the run, shaped for charting.
//
// The wire format is deliberately flat and pre-computed rather than a generic
// metric dump. The dashboard redraws several series many times a second, and
// having the browser derive rates and dig percentiles out of nested maps on
// every frame is exactly the sort of work that makes a live chart stutter.
type TickDTO struct {
	T          int64   `json:"t"`
	VUs        uint32  `json:"vus"`
	Requests   uint64  `json:"requests"`
	Failures   uint64  `json:"failures"`
	Iterations uint64  `json:"iterations"`
	RPS        float64 `json:"rps"`
	ErrorRate  float64 `json:"errorRate"`
	Avg        float64 `json:"avg"`
	P50        float64 `json:"p50"`
	P90        float64 `json:"p90"`
	P95        float64 `json:"p95"`
	P99        float64 `json:"p99"`

	Status    map[string]uint64       `json:"status,omitempty"`
	Scenarios map[string]ScenarioTick `json:"scenarios,omitempty"`

	// Endpoints is the per-request-name breakdown, which is what the response
	// time chart plots one line from.
	Endpoints map[string]EndpointTick `json:"endpoints,omitempty"`
}

// bucketToTick flattens a store bucket into the chart payload.
func bucketToTick(bucket metrics.Bucket, resolution time.Duration) TickDTO {
	seconds := resolution.Seconds()
	if seconds <= 0 {
		seconds = 1
	}

	tick := TickDTO{
		T:      bucket.Start.UnixMilli(),
		VUs:    bucket.ActiveVUs,
		Status: bucket.Status,
	}

	if reqs := bucket.Points[metrics.PointKey{Metric: loadwave.MetricHTTPReqs}]; reqs != nil {
		tick.Requests = reqs.Count
		tick.RPS = float64(reqs.Count) / seconds
	}
	if failed := bucket.Points[metrics.PointKey{Metric: loadwave.MetricHTTPReqFailed}]; failed != nil {
		tick.Failures = failed.NonZero
		tick.ErrorRate = failed.Ratio()
	}
	if iters := bucket.Points[metrics.PointKey{Metric: loadwave.MetricIterations}]; iters != nil {
		tick.Iterations = iters.Count
	}
	if dur := bucket.Points[metrics.PointKey{Metric: loadwave.MetricHTTPReqDuration}]; dur != nil {
		tick.Avg = dur.Avg()
		tick.P50 = dur.Percentiles["p50"]
		tick.P90 = dur.Percentiles["p90"]
		tick.P95 = dur.Percentiles["p95"]
		tick.P99 = dur.Percentiles["p99"]
	}

	tick.Scenarios = scenarioTicks(bucket)
	tick.Endpoints = endpointTicks(bucket)
	return tick
}

// endpointTicks flattens the per-endpoint breakdown for one bucket.
func endpointTicks(bucket metrics.Bucket) map[string]EndpointTick {
	if len(bucket.Endpoints) == 0 {
		return nil
	}

	out := make(map[string]EndpointTick, len(bucket.Endpoints))
	for name, point := range bucket.Endpoints {
		out[name] = EndpointTick{
			Avg:       point.Avg(),
			Requests:  point.Requests,
			ErrorRate: point.ErrorRate(),
		}
	}
	return out
}

// scenarioTicks extracts the per-scenario breakdown from a bucket.
func scenarioTicks(bucket metrics.Bucket) map[string]ScenarioTick {
	var out map[string]ScenarioTick

	ensure := func(name string) ScenarioTick {
		if out == nil {
			out = make(map[string]ScenarioTick, 4)
		}
		return out[name]
	}

	for key, point := range bucket.Points {
		if key.Scenario == "" {
			continue
		}
		entry := ensure(key.Scenario)

		switch key.Metric {
		case loadwave.MetricVUs:
			entry.VUs = point.Sum
		case loadwave.MetricIterations:
			entry.Iterations = point.Count
		case loadwave.MetricHTTPReqs:
			entry.Requests = point.Count
		case loadwave.MetricHTTPReqFailed:
			entry.ErrorRate = point.Ratio()
		case loadwave.MetricHTTPReqDuration:
			entry.P95 = point.Percentiles["p95"]
		default:
			continue
		}
		out[key.Scenario] = entry
	}
	return out
}

// ticksSince returns the buckets finalised since the run last published, and
// advances the run's watermark.
//
// Publishing only the new buckets keeps the live stream small and lets the
// dashboard append rather than redraw. Clients recover the history they missed
// through the REST snapshot, which is also what a page reload does.
func (c *Coordinator) ticksSince(run *Run) []TickDTO {
	run.mu.Lock()
	since := run.published
	run.mu.Unlock()

	buckets := run.store.Timeline(since)
	if len(buckets) == 0 {
		return nil
	}

	ticks := make([]TickDTO, 0, len(buckets))
	newest := since
	for _, bucket := range buckets {
		ticks = append(ticks, bucketToTick(bucket, c.cfg.Store.Resolution))
		if bucket.Start.After(newest) {
			newest = bucket.Start
		}
	}

	run.mu.Lock()
	if newest.After(run.published) {
		run.published = newest
	}
	run.mu.Unlock()

	return ticks
}

// Snapshot is the complete current state, served to a client on connect.
type Snapshot struct {
	Build      buildinfo.Info          `json:"build"`
	Run        *Summary                `json:"run,omitempty"`
	Runs       []Summary               `json:"runs"`
	Agents     []AgentInfo             `json:"agents"`
	Ticks      []TickDTO               `json:"ticks"`
	Series     []metrics.SeriesSummary `json:"series"`
	Events     []Event                 `json:"events"`
	Resolution float64                 `json:"resolutionSeconds"`

	// Totals holds one correctly merged aggregate per metric. Clients must
	// use these for whole-run figures rather than folding Series, which
	// cannot be done correctly outside the store.
	Totals map[string]metrics.SeriesSummary `json:"totals,omitempty"`

	// Endpoints is the per-request-name breakdown, with percentiles
	// recomputed from each endpoint's merged distribution.
	Endpoints []metrics.EndpointSummary `json:"endpoints,omitempty"`

	// Failures explains what went wrong, which the metrics only count.
	Failures []metrics.FailureSummary `json:"failures,omitempty"`
}

// Snapshot renders the coordinator's whole state.
func (c *Coordinator) Snapshot() Snapshot {
	snapshot := Snapshot{
		Build:      buildinfo.Get(),
		Runs:       c.Runs(),
		Agents:     c.Agents(),
		Events:     c.GlobalEvents(),
		Resolution: c.cfg.Store.Resolution.Seconds(),
		Ticks:      []TickDTO{},
		Series:     []metrics.SeriesSummary{},
	}

	run := c.ActiveRun()
	if run == nil {
		return snapshot
	}
	snapshot.Run = ptr(run.Summary(c.profileOf(run)))
	snapshot.Series = run.store.Summary()
	snapshot.Totals = run.store.Totals()
	snapshot.Endpoints = run.store.Endpoints()
	snapshot.Failures = run.store.Failures()
	snapshot.Events = append(snapshot.Events, run.Events()...)

	for _, bucket := range run.store.Timeline(time.Time{}) {
		snapshot.Ticks = append(snapshot.Ticks, bucketToTick(bucket, c.cfg.Store.Resolution))
	}
	return snapshot
}

// RunSnapshot renders one run's full state, live or finished.
func (c *Coordinator) RunSnapshot(runID string) (Snapshot, bool) {
	run, ok := c.Lookup(runID)
	if !ok {
		return Snapshot{}, false
	}

	snapshot := Snapshot{
		Build:      buildinfo.Get(),
		Run:        ptr(run.Summary(c.profileOf(run))),
		Runs:       c.Runs(),
		Agents:     c.Agents(),
		Series:     run.store.Summary(),
		Totals:     run.store.Totals(),
		Endpoints:  run.store.Endpoints(),
		Failures:   run.store.Failures(),
		Events:     run.Events(),
		Resolution: c.cfg.Store.Resolution.Seconds(),
		Ticks:      []TickDTO{},
	}
	for _, bucket := range run.store.Timeline(time.Time{}) {
		snapshot.Ticks = append(snapshot.Ticks, bucketToTick(bucket, c.cfg.Store.Resolution))
	}
	return snapshot, true
}

// subscriberBuffer is how many updates a slow client may fall behind before
// its updates start being dropped. Two seconds of ticks is enough to ride out
// a garbage collection pause without letting a stalled browser tab hold
// coordinator memory.
const subscriberBuffer = 8

// Subscription is one client's live feed.
type Subscription struct {
	ch   chan Update
	done chan struct{}
	set  *subscriberSet
	once sync.Once
}

// Updates returns the channel of live updates. It is never closed; select on
// Done alongside it.
func (s *Subscription) Updates() <-chan Update { return s.ch }

// Done is closed when the subscription ends.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Close ends the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		close(s.done)
		s.set.remove(s)
	})
}

// subscriberSet fans updates out to live clients.
type subscriberSet struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// Subscribe opens a live feed.
func (c *Coordinator) Subscribe() *Subscription { return c.subs.add() }

func (s *subscriberSet) add() *Subscription {
	sub := &Subscription{
		ch:   make(chan Update, subscriberBuffer),
		done: make(chan struct{}),
		set:  s,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = make(map[*Subscription]struct{})
	}
	s.subs[sub] = struct{}{}
	return sub
}

func (s *subscriberSet) remove(sub *Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, sub)
}

// publish delivers an update to every subscriber, skipping any that has
// fallen behind.
//
// A wedged browser tab must not be able to slow the coordinator down, and a
// dropped tick is recoverable: the next one carries the current state, and a
// reload fetches the full history.
func (s *subscriberSet) publish(update Update) {
	s.mu.Lock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- update:
		case <-sub.done:
		default:
		}
	}
}

// closeAll ends every subscription, on coordinator shutdown.
func (s *subscriberSet) closeAll() {
	s.mu.Lock()
	subs := make([]*Subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		sub.Close()
	}
}
