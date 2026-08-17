// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"fmt"
	"sort"
	"sync"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// StoreConfig tunes the coordinator's view of a run.
type StoreConfig struct {
	// Resolution is the width of one time bucket. Zero applies one second.
	Resolution time.Duration

	// Window is how much history is retained for charting. Older buckets are
	// evicted. Zero applies one hour.
	Window time.Duration

	// LateGrace is how long a bucket accepts further contributions before it
	// is finalised. It has to cover the worst-case skew between an agent
	// flushing and the coordinator receiving; too short and slow nodes get
	// silently dropped from the tail of the chart.
	LateGrace time.Duration

	// Histogram must match the resolution every node is recording at.
	Histogram HistogramConfig

	// MaxSeries caps distinct cumulative series held. Nodes enforce their own
	// cap, but a large fleet can still sum to more than the coordinator
	// should hold, so it is enforced again here.
	MaxSeries int

	// MaxChartedEndpoints caps how many request names get their own line in
	// the time buckets. Zero applies DefaultMaxChartedEndpoints.
	MaxChartedEndpoints int

	// MaxFailureKinds caps distinct kinds of failure retained for the run.
	MaxFailureKinds int
}

// Defaults applied to a zero StoreConfig.
const (
	DefaultResolution = time.Second
	DefaultWindow     = time.Hour
	DefaultLateGrace  = 3 * time.Second

	// DefaultMaxChartedEndpoints bounds the per-endpoint timeline.
	//
	// Endpoint names are already collapsed to low cardinality, so this is
	// generous for any real service; it exists so that a run which defeats
	// that collapsing cannot turn a one-hour window into a memory leak.
	DefaultMaxChartedEndpoints = 40
)

func (c *StoreConfig) applyDefaults() {
	if c.Resolution <= 0 {
		c.Resolution = DefaultResolution
	}
	if c.Window <= 0 {
		c.Window = DefaultWindow
	}
	if c.LateGrace <= 0 {
		c.LateGrace = DefaultLateGrace
	}
	if c.MaxSeries <= 0 {
		c.MaxSeries = DefaultMaxSeries
	}
	if c.MaxChartedEndpoints <= 0 {
		c.MaxChartedEndpoints = DefaultMaxChartedEndpoints
	}
	if c.MaxFailureKinds <= 0 {
		c.MaxFailureKinds = DefaultMaxFailureKinds
	}
	if c.Histogram.Validate() != nil {
		c.Histogram = DefaultHistogramConfig()
	}
}

// Percentiles reported for every trend metric.
var reportedPercentiles = []float64{50, 90, 95, 99}

// PercentileKeys names the reported percentiles, in the same order.
var PercentileKeys = []string{"p50", "p90", "p95", "p99"}

// Point is one metric's activity inside one time bucket.
//
// It holds scalars only. The histogram a bucket needs to derive percentiles is
// discarded once the bucket is finalised: keeping one per series per second
// for an hour-long run would cost tens of gigabytes, whereas the four
// percentiles the dashboard actually plots cost thirty-two bytes.
type Point struct {
	Count   uint64  `json:"count"`
	Sum     float64 `json:"sum"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	NonZero uint64  `json:"nonZero"`

	// Percentiles is populated for trend metrics, keyed by PercentileKeys.
	Percentiles map[string]float64 `json:"percentiles,omitempty"`
}

// Avg returns the mean observation, or zero when there were none.
func (p Point) Avg() float64 {
	if p.Count == 0 {
		return 0
	}
	return p.Sum / float64(p.Count)
}

// Ratio returns the share of truthy observations, for rate metrics.
func (p Point) Ratio() float64 {
	if p.Count == 0 {
		return 0
	}
	return float64(p.NonZero) / float64(p.Count)
}

// PointKey identifies a bucket series.
//
// Buckets are indexed by metric and scenario only, not by the full label set.
// Full-cardinality history is what makes a metrics store expensive, and the
// live charts plot exactly these two dimensions; per-label detail is available
// cumulatively through Summary, which is what the endpoint table renders from.
type PointKey struct {
	Metric   string `json:"metric"`
	Scenario string `json:"scenario"`
}

// EndpointPoint is one request name's activity within one time bucket.
//
// It carries no histogram, and therefore no percentiles. That is what makes a
// per-endpoint timeline affordable: a histogram per endpoint per second would
// cost gigabytes over an hour, where a sum and a count cost thirty-two bytes.
// Percentiles per endpoint are still available for the whole run, from the
// cumulative aggregates that Endpoints() merges.
type EndpointPoint struct {
	Requests uint64  `json:"requests"`
	Failures uint64  `json:"failures"`
	Sum      float64 `json:"-"`
	Observed uint64  `json:"-"`
}

// Avg returns the mean duration in milliseconds, or zero if nothing was timed.
func (p EndpointPoint) Avg() float64 {
	if p.Observed == 0 {
		return 0
	}
	return p.Sum / float64(p.Observed)
}

// ErrorRate returns the share of this endpoint's requests that failed.
func (p EndpointPoint) ErrorRate() float64 {
	if p.Requests == 0 {
		return 0
	}
	return float64(p.Failures) / float64(p.Requests)
}

// Bucket is the whole cluster's activity over one resolution interval.
type Bucket struct {
	Start     time.Time
	ActiveVUs uint32
	Points    map[PointKey]*Point
	Status    map[string]uint64

	// Endpoints is the per-request-name breakdown for this interval.
	Endpoints map[string]*EndpointPoint

	// vusByNode and hists exist only while the bucket is open, and are
	// released when it is finalised.
	vusByNode map[string]uint32
	hists     map[PointKey]*hdr.Histogram
}

// bucketIndex is a bucket's position on the resolution grid.
type bucketIndex int64

// Store accumulates every node's reports into one coherent view of a run.
//
// It keeps two things: a cumulative aggregate per series, at full label
// cardinality and full histogram fidelity, which drives the endpoint table and
// threshold evaluation; and a rolling window of time buckets at reduced
// dimensionality, which drives the live charts.
//
// Safe for concurrent use.
type Store struct {
	cfg StoreConfig

	mu         sync.RWMutex
	cumulative *aggMap
	scratch    *hdr.Histogram

	// open buckets still accept contributions from lagging nodes.
	open map[bucketIndex]*Bucket

	// closed is a ring of finalised buckets, oldest at head.
	closed   []Bucket
	head     int
	closedN  int
	capacity int

	started  time.Time
	lastSeen time.Time

	droppedSeries    uint64
	droppedLate      uint64
	droppedEndpoints uint64
	nodeDropped      map[string]uint64

	// failures explains what went wrong; the numeric metrics only count it.
	failures *failureStore
}

// NewStore builds an empty store for one run.
func NewStore(cfg StoreConfig) *Store {
	cfg.applyDefaults()
	capacity := int(cfg.Window / cfg.Resolution)
	if capacity < 1 {
		capacity = 1
	}

	return &Store{
		cfg:         cfg,
		cumulative:  newAggMap(cfg.Histogram),
		scratch:     cfg.Histogram.New(),
		open:        make(map[bucketIndex]*Bucket),
		closed:      make([]Bucket, capacity),
		capacity:    capacity,
		nodeDropped: make(map[string]uint64),
		failures:    newFailureStore(cfg.MaxFailureKinds),
	}
}

// Ingest folds one node's batch into the store.
//
// An error means the batch was rejected outright — a resolution mismatch, or
// a bucket so old it has already been evicted. Individual series that exceed
// the cardinality cap are dropped and counted rather than failing the batch,
// since one runaway label should not blind the operator to everything else.
func (s *Store) Ingest(batch *loadwavev1.MetricBatch) error {
	if batch == nil {
		return nil
	}

	start := batch.GetBucketStart().AsTime()
	if start.IsZero() {
		return fmt.Errorf("batch from node %q has no bucket start", batch.GetNodeId())
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started.IsZero() || start.Before(s.started) {
		s.started = start
	}
	if start.After(s.lastSeen) {
		s.lastSeen = start
	}
	if batch.GetDroppedSamples() > 0 {
		s.nodeDropped[batch.GetNodeId()] = batch.GetDroppedSamples()
	}

	idx := s.indexOf(start)
	bucket, err := s.bucketFor(idx, start)
	if err != nil {
		s.droppedLate++
		return err
	}

	// Track each node's contribution separately so that a redelivered batch
	// replaces the node's previous figure instead of doubling it.
	bucket.vusByNode[batch.GetNodeId()] = batch.GetActiveVus()
	bucket.ActiveVUs = 0
	for _, v := range bucket.vusByNode {
		bucket.ActiveVUs += v
	}

	for _, delta := range batch.GetSeries() {
		if err := s.ingestSeries(bucket, delta); err != nil {
			return err
		}
	}

	s.failures.ingest(batch.GetFailures())
	return nil
}

// Failures returns every kind of failure seen in the run, most frequent first.
func (s *Store) Failures() []FailureSummary { return s.failures.summaries() }

// ingestSeries folds one series delta into both the cumulative aggregate and
// the current bucket.
func (s *Store) ingestSeries(bucket *Bucket, delta *loadwavev1.SeriesDelta) error {
	labels := loadwave.LabelsFromMap(delta.GetTags())
	kind := DecodeKind(delta.GetKind())

	var hist *hdr.Histogram
	if snap := delta.GetHistogram(); snap != nil {
		decoded, err := DecodeHistogram(snap, s.cfg.Histogram)
		if err != nil {
			return fmt.Errorf("metric %q: %w", delta.GetMetric(), err)
		}
		hist = decoded
	}

	incoming := aggregate{
		kind:    kind,
		count:   delta.GetCount(),
		sum:     delta.GetSum(),
		minVal:  delta.GetMin(),
		maxVal:  delta.GetMax(),
		nonZero: delta.GetNonZero(),
		gauge:   delta.GetSum(),
		hist:    hist,
	}

	s.mergeCumulative(delta.GetMetric(), labels, kind, &incoming)
	s.mergeBucket(bucket, delta.GetMetric(), labels, kind, &incoming, hist)
	return nil
}

// mergeCumulative folds a delta into the run-long aggregate.
func (s *Store) mergeCumulative(
	metric string, labels loadwave.Labels, kind loadwave.MetricKind, incoming *aggregate,
) {
	target := s.cumulative.find(metric, labels)
	if target == nil {
		if s.cumulative.len() >= s.cfg.MaxSeries {
			s.droppedSeries++
			return
		}
		target = s.cumulative.getOrCreate(metric, labels, kind)
	}

	// A gauge's cumulative value is the latest reading, not a running sum;
	// mergeFrom adds, which is right across nodes within a bucket but wrong
	// across time.
	if kind == loadwave.KindGauge {
		target.count++
		target.gauge = incoming.gauge
		return
	}
	target.mergeFrom(incoming)
}

// mergeBucket folds a delta into the reduced-dimensionality bucket view.
func (s *Store) mergeBucket(
	bucket *Bucket, metric string, labels loadwave.Labels,
	kind loadwave.MetricKind, incoming *aggregate, hist *hdr.Histogram,
) {
	scenario, _ := labels.Get(loadwave.LabelScenario)

	// Record against the per-scenario series and, separately, against the
	// run-wide rollup, so the dashboard can chart either without a second
	// pass. When the metric carries no scenario the two keys coincide, and
	// adding twice would double every figure.
	s.addToBucket(bucket, PointKey{Metric: metric, Scenario: scenario}, kind, incoming, hist)
	if scenario != "" {
		s.addToBucket(bucket, PointKey{Metric: metric}, kind, incoming, hist)
	}

	if metric == loadwave.MetricHTTPReqs {
		if status, ok := labels.Get(loadwave.LabelStatus); ok {
			bucket.Status[statusClass(status)] += incoming.count
		}
	}

	if name, ok := labels.Get(loadwave.LabelName); ok && name != "" {
		s.addEndpointPoint(bucket, name, metric, incoming)
	}
}

// addEndpointPoint folds a delta into the per-endpoint timeline.
func (s *Store) addEndpointPoint(bucket *Bucket, name, metric string, incoming *aggregate) {
	// Only the three metrics the endpoint chart and its tooltip need. Every
	// other metric would widen the structure without widening what it can
	// show.
	switch metric {
	case loadwave.MetricHTTPReqs, loadwave.MetricHTTPReqDuration, loadwave.MetricHTTPReqFailed:
	default:
		return
	}

	point, ok := bucket.Endpoints[name]
	if !ok {
		if len(bucket.Endpoints) >= s.cfg.MaxChartedEndpoints {
			s.droppedEndpoints++
			return
		}
		point = &EndpointPoint{}
		bucket.Endpoints[name] = point
	}

	switch metric {
	case loadwave.MetricHTTPReqs:
		point.Requests += incoming.count
	case loadwave.MetricHTTPReqDuration:
		point.Sum += incoming.sum
		point.Observed += incoming.count
	case loadwave.MetricHTTPReqFailed:
		point.Failures += incoming.nonZero
	}
}

// addToBucket accumulates one delta under one bucket key.
func (s *Store) addToBucket(
	bucket *Bucket, key PointKey, kind loadwave.MetricKind, incoming *aggregate, hist *hdr.Histogram,
) {
	point, ok := bucket.Points[key]
	if !ok {
		point = &Point{}
		bucket.Points[key] = point
	}

	point.Count += incoming.count
	point.NonZero += incoming.nonZero

	if kind == loadwave.KindGauge {
		point.Sum += incoming.gauge
		return
	}

	point.Sum += incoming.sum
	if incoming.count > 0 {
		if point.Min == 0 || incoming.minVal < point.Min {
			point.Min = incoming.minVal
		}
		if incoming.maxVal > point.Max {
			point.Max = incoming.maxVal
		}
	}

	if hist == nil {
		return
	}
	acc, ok := bucket.hists[key]
	if !ok {
		acc = s.cfg.Histogram.New()
		bucket.hists[key] = acc
	}
	acc.Merge(hist)
}

// statusClass reduces an HTTP status to the handful of groups worth charting.
func statusClass(status string) string {
	if status == "" {
		return "other"
	}
	switch status[0] {
	case '0':
		return "error"
	case '1':
		return "1xx"
	case '2':
		return "2xx"
	case '3':
		return "3xx"
	case '4':
		return "4xx"
	case '5':
		return "5xx"
	default:
		return "other"
	}
}

// indexOf maps a timestamp onto the bucket grid.
func (s *Store) indexOf(t time.Time) bucketIndex {
	return bucketIndex(t.UnixNano() / int64(s.cfg.Resolution))
}

// bucketFor returns the open bucket for an index, creating it if the bucket is
// still within the acceptance window.
func (s *Store) bucketFor(idx bucketIndex, start time.Time) (*Bucket, error) {
	if bucket, ok := s.open[idx]; ok {
		return bucket, nil
	}

	// Refuse buckets that have already been finalised, rather than silently
	// creating a duplicate that would appear out of order in the chart.
	cutoff := s.indexOf(s.lastSeen.Add(-s.cfg.LateGrace))
	if idx < cutoff {
		return nil, fmt.Errorf(
			"bucket at %s arrived after its grace period and was dropped", start.Format(time.RFC3339))
	}

	bucket := &Bucket{
		Start:     start.Truncate(s.cfg.Resolution),
		Points:    make(map[PointKey]*Point),
		Status:    make(map[string]uint64),
		Endpoints: make(map[string]*EndpointPoint),
		vusByNode: make(map[string]uint32),
		hists:     make(map[PointKey]*hdr.Histogram),
	}
	s.open[idx] = bucket
	return bucket, nil
}

// CloseStale finalises every open bucket whose grace period has elapsed.
//
// The coordinator calls this on a ticker rather than only on ingest, so that a
// run whose traffic stops still gets its final buckets published instead of
// leaving the chart hanging one interval short.
func (s *Store) CloseStale(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := s.indexOf(now.Add(-s.cfg.LateGrace))
	ready := make([]bucketIndex, 0, len(s.open))
	for idx := range s.open {
		if idx < cutoff {
			ready = append(ready, idx)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i] < ready[j] })

	for _, idx := range ready {
		s.finalise(s.open[idx])
		delete(s.open, idx)
	}
}

// finalise computes a bucket's percentiles, releases its histograms and files
// it in the ring.
func (s *Store) finalise(bucket *Bucket) {
	for key, hist := range bucket.hists {
		point := bucket.Points[key]
		if point == nil || hist.TotalCount() == 0 {
			continue
		}
		point.Percentiles = percentilesOf(hist)
	}
	bucket.hists = nil
	bucket.vusByNode = nil

	slot := (s.head + s.closedN) % s.capacity
	if s.closedN == s.capacity {
		// Ring is full: overwrite the oldest and advance the head.
		slot = s.head
		s.head = (s.head + 1) % s.capacity
	} else {
		s.closedN++
	}
	s.closed[slot] = *bucket
}

// percentilesOf extracts the reported percentiles in natural units.
func percentilesOf(hist *hdr.Histogram) map[string]float64 {
	out := make(map[string]float64, len(reportedPercentiles))
	for i, q := range reportedPercentiles {
		out[PercentileKeys[i]] = UnscaleValue(hist.ValueAtQuantile(q))
	}
	return out
}

// Timeline returns finalised buckets starting at or after `since`, oldest
// first. A zero `since` returns the whole retained window.
func (s *Store) Timeline(since time.Time) []Bucket {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Bucket, 0, s.closedN)
	for i := range s.closedN {
		bucket := s.closed[(s.head+i)%s.capacity]
		if !since.IsZero() && !bucket.Start.After(since) {
			continue
		}
		out = append(out, bucket)
	}
	return out
}

// SeriesSummary is one series' cumulative state over the whole run.
type SeriesSummary struct {
	Metric      string             `json:"metric"`
	Kind        string             `json:"kind"`
	Tags        map[string]string  `json:"tags,omitempty"`
	Count       uint64             `json:"count"`
	Sum         float64            `json:"sum"`
	Min         float64            `json:"min"`
	Max         float64            `json:"max"`
	Avg         float64            `json:"avg"`
	Rate        float64            `json:"rate"`
	Percentiles map[string]float64 `json:"percentiles,omitempty"`
}

// Summary reports every cumulative series, sorted by metric then tags so the
// output is stable between calls and diffable between runs.
func (s *Store) Summary() []SeriesSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SeriesSummary, 0, s.cumulative.len())
	s.cumulative.each(func(a *aggregate) {
		summary := SeriesSummary{
			Metric: a.metric,
			Kind:   a.kind.String(),
			Tags:   a.labels.Map(),
			Count:  a.count,
			Sum:    a.sum,
			Min:    a.min(),
			Max:    a.max(),
		}
		if a.kind == loadwave.KindGauge {
			summary.Sum = a.gauge
		}
		if a.count > 0 {
			summary.Avg = a.sum / float64(a.count)
			summary.Rate = float64(a.nonZero) / float64(a.count)
		}
		if a.hist != nil && a.hist.TotalCount() > 0 {
			summary.Percentiles = percentilesOf(a.hist)
		}
		out = append(out, summary)
	})

	sort.Slice(out, func(i, j int) bool {
		if out[i].Metric != out[j].Metric {
			return out[i].Metric < out[j].Metric
		}
		return tagString(out[i].Tags) < tagString(out[j].Tags)
	})
	return out
}

// tagString renders tags deterministically for sorting.
func tagString(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]byte, 0, 64)
	for _, k := range keys {
		out = append(out, k...)
		out = append(out, '=')
		out = append(out, tags[k]...)
		out = append(out, ',')
	}
	return string(out)
}

// Aggregate returns the cumulative aggregate for one metric across every
// label combination, which is what thresholds are evaluated against.
func (s *Store) Aggregate(metric string) (SeriesSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := SeriesSummary{Metric: metric}
	s.scratch.Reset()

	found := false
	s.cumulative.each(func(a *aggregate) {
		if a.metric != metric {
			return
		}
		found = true
		total.Kind = a.kind.String()
		total.Count += a.count
		total.Sum += a.sum
		total.Rate += float64(a.nonZero)
		if total.Min == 0 || (a.count > 0 && a.min() < total.Min) {
			total.Min = a.min()
		}
		if a.max() > total.Max {
			total.Max = a.max()
		}
		if a.hist != nil {
			s.scratch.Merge(a.hist)
		}
	})
	if !found {
		return SeriesSummary{}, false
	}

	if total.Count > 0 {
		total.Avg = total.Sum / float64(total.Count)
		total.Rate /= float64(total.Count)
	}
	if s.scratch.TotalCount() > 0 {
		total.Percentiles = percentilesOf(s.scratch)
	}
	return total, true
}

// Totals returns one merged aggregate per metric, folded across every label
// combination.
//
// This is the only correct way to get a whole-run figure. Series are stored
// per label set — per endpoint, per status code — and the summary statistics
// of those slices cannot simply be averaged back together: a mean must be
// re-weighted by count, and a percentile has to be recomputed from the merged
// distribution. Callers that fold Summary() themselves get plausible-looking
// numbers that disagree with the thresholds, which is worse than no numbers.
func (s *Store) Totals() map[string]SeriesSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	totals := make(map[string]SeriesSummary)
	hists := make(map[string]*hdr.Histogram)

	s.cumulative.each(func(a *aggregate) {
		total, seen := totals[a.metric]
		if !seen {
			total = SeriesSummary{Metric: a.metric, Kind: a.kind.String(), Min: a.min()}
		}

		total.Count += a.count
		if a.kind == loadwave.KindGauge {
			total.Sum += a.gauge
		} else {
			total.Sum += a.sum
		}
		// Rate accumulates the numerator here and is divided through below.
		total.Rate += float64(a.nonZero)

		if a.count > 0 && (!seen || a.min() < total.Min) {
			total.Min = a.min()
		}
		if a.max() > total.Max {
			total.Max = a.max()
		}
		totals[a.metric] = total

		if a.hist == nil || a.hist.TotalCount() == 0 {
			return
		}
		merged, ok := hists[a.metric]
		if !ok {
			merged = s.cfg.Histogram.New()
			hists[a.metric] = merged
		}
		merged.Merge(a.hist)
	})

	for metric, total := range totals {
		if total.Count > 0 {
			total.Avg = total.Sum / float64(total.Count)
			total.Rate /= float64(total.Count)
		}
		if merged, ok := hists[metric]; ok && merged.TotalCount() > 0 {
			total.Percentiles = percentilesOf(merged)
		}
		totals[metric] = total
	}
	return totals
}

// EndpointSummary is one request name's whole-run view, merged across every
// status code and scenario that used it.
type EndpointSummary struct {
	Name        string             `json:"name"`
	Requests    uint64             `json:"requests"`
	Failures    uint64             `json:"failures"`
	ErrorRate   float64            `json:"errorRate"`
	Avg         float64            `json:"avg"`
	Min         float64            `json:"min"`
	Max         float64            `json:"max"`
	Percentiles map[string]float64 `json:"percentiles,omitempty"`
	Statuses    map[string]uint64  `json:"statuses,omitempty"`
	BytesIn     float64            `json:"bytesIn"`
}

// Endpoints returns one row per request name, sorted slowest first by p95.
//
// Series are stored split by status code as well as by name, so a per-endpoint
// percentile has to be recomputed from the merged distribution of all of that
// endpoint's slices. Taking the maximum of the per-status percentiles — the
// obvious shortcut — reports the tail of whichever status happened to be
// slowest rather than the endpoint's actual tail, and the two diverge most
// exactly when an endpoint starts failing, which is when the number matters.
func (s *Store) Endpoints() []EndpointSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type accumulator struct {
		summary EndpointSummary
		hist    *hdr.Histogram
		sum     float64
		seenMin bool
	}
	byName := make(map[string]*accumulator)

	get := func(name string) *accumulator {
		acc, ok := byName[name]
		if !ok {
			acc = &accumulator{summary: EndpointSummary{
				Name:     name,
				Statuses: make(map[string]uint64),
			}}
			byName[name] = acc
		}
		return acc
	}

	s.cumulative.each(func(a *aggregate) {
		name, ok := a.labels.Get(loadwave.LabelName)
		if !ok || name == "" {
			return
		}
		acc := get(name)

		switch a.metric {
		case loadwave.MetricHTTPReqDuration:
			acc.summary.Requests += a.count
			acc.sum += a.sum
			if a.count > 0 && (!acc.seenMin || a.min() < acc.summary.Min) {
				acc.summary.Min = a.min()
				acc.seenMin = true
			}
			if a.max() > acc.summary.Max {
				acc.summary.Max = a.max()
			}
			if a.hist != nil && a.hist.TotalCount() > 0 {
				if acc.hist == nil {
					acc.hist = s.cfg.Histogram.New()
				}
				acc.hist.Merge(a.hist)
			}

		case loadwave.MetricHTTPReqFailed:
			acc.summary.Failures += a.nonZero

		case loadwave.MetricHTTPReqs:
			if status, ok := a.labels.Get(loadwave.LabelStatus); ok {
				acc.summary.Statuses[status] += a.count
			}

		case loadwave.MetricHTTPReqBytesIn:
			acc.summary.BytesIn += a.sum
		}
	})

	out := make([]EndpointSummary, 0, len(byName))
	for _, acc := range byName {
		summary := acc.summary
		if summary.Requests > 0 {
			summary.Avg = acc.sum / float64(summary.Requests)
			summary.ErrorRate = float64(summary.Failures) / float64(summary.Requests)
		}
		if acc.hist != nil && acc.hist.TotalCount() > 0 {
			summary.Percentiles = percentilesOf(acc.hist)
		}
		out = append(out, summary)
	}

	// Slowest first: the only reason to read this table is to find what to
	// fix, and that is almost always the worst tail.
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i].Percentiles["p95"], out[j].Percentiles["p95"]
		if left != right {
			return left > right
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Stats reports the store's own health, so the dashboard can warn when the
// numbers it is showing are incomplete.
type Stats struct {
	Series           int       `json:"series"`
	ClosedBuckets    int       `json:"closedBuckets"`
	OpenBuckets      int       `json:"openBuckets"`
	DroppedSeries    uint64    `json:"droppedSeries"`
	DroppedLate      uint64    `json:"droppedLate"`
	DroppedByNode    uint64    `json:"droppedByNode"`
	DroppedEndpoints uint64    `json:"droppedEndpoints"`
	DroppedFailures  uint64    `json:"droppedFailureKinds"`
	Started          time.Time `json:"started"`
	LastSeen         time.Time `json:"lastSeen"`
}

// Stats returns a snapshot of the store's bookkeeping.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var byNode uint64
	for _, n := range s.nodeDropped {
		byNode += n
	}
	return Stats{
		Series:           s.cumulative.len(),
		ClosedBuckets:    s.closedN,
		OpenBuckets:      len(s.open),
		DroppedSeries:    s.droppedSeries,
		DroppedLate:      s.droppedLate,
		DroppedByNode:    byNode,
		DroppedEndpoints: s.droppedEndpoints,
		DroppedFailures:  s.failures.droppedKinds(),
		Started:          s.started,
		LastSeen:         s.lastSeen,
	}
}
