// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// RecorderConfig tunes the per-node aggregator.
type RecorderConfig struct {
	// Shards is how many independent partitions scalar metrics are spread
	// across, rounded up to a power of two. More shards mean less lock
	// contention between virtual users. Zero picks a value from GOMAXPROCS.
	Shards int

	// TrendShards is the same for trend metrics, and is deliberately smaller.
	// Each shard holds its own histogram for every trend series it sees, and
	// a histogram is kilobytes rather than the few dozen bytes a counter
	// costs — so widening this trades a lot of memory for a little
	// contention. Zero picks a small default. Clamped to at most Shards.
	TrendShards int

	// MaxSeries caps how many distinct series the node will track. Beyond it,
	// observations for new series are dropped and counted rather than
	// allowed to exhaust memory. A runaway tag — a user id used as a label —
	// is the usual cause, and a bounded, visibly lossy run beats an OOM.
	MaxSeries int

	// Histogram fixes trend resolution and must match every other node.
	Histogram HistogramConfig

	// MaxFailureKinds caps how many distinct kinds of failure are tracked.
	// Zero applies DefaultMaxFailureKinds.
	MaxFailureKinds int
}

// Defaults applied to a zero RecorderConfig.
const (
	DefaultMaxSeries      = 5000
	DefaultTrendShards    = 4
	defaultMaxScalarShard = 16
)

func (c *RecorderConfig) applyDefaults() {
	if c.Shards <= 0 {
		c.Shards = min(runtime.GOMAXPROCS(0), defaultMaxScalarShard)
	}
	c.Shards = roundUpPow2(c.Shards)

	if c.TrendShards <= 0 {
		c.TrendShards = DefaultTrendShards
	}
	c.TrendShards = min(roundUpPow2(c.TrendShards), c.Shards)

	if c.MaxSeries <= 0 {
		c.MaxSeries = DefaultMaxSeries
	}
	if c.Histogram.Validate() != nil {
		c.Histogram = DefaultHistogramConfig()
	}
	if c.MaxFailureKinds <= 0 {
		c.MaxFailureKinds = DefaultMaxFailureKinds
	}
}

// roundUpPow2 returns the smallest power of two greater than or equal to n.
func roundUpPow2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

// shard is one lock-protected partition of the aggregate space.
type shard struct {
	mu   sync.Mutex
	aggs *aggMap

	// Shards are hot and written constantly by different cores. The padding
	// keeps two shards off the same cache line, so a virtual user recording
	// into shard 3 does not invalidate shard 4's line on another core.
	_ [64]byte
}

// Recorder is a node's metric aggregator: the sink virtual users write to and
// the source the reporting loop flushes from.
//
// Recording is on the hottest path in the program — several observations per
// HTTP request, tens of thousands of requests per second — so it does no
// allocation in steady state and takes only a sharded mutex.
//
// Safe for concurrent use.
type Recorder struct {
	cfg       RecorderConfig
	shards    []*shard
	mask      uint64
	trendMask uint64

	// distinct bounds cardinality across all shards. It is only consulted
	// when a shard sees a series for the first time, so it is cold.
	distinctMu sync.Mutex
	distinct   map[seriesKey]struct{}

	dropped atomic.Uint64
	rr      atomic.Uint64

	// flushMu serialises flushing and guards merged, which is reused between
	// flushes so a steady-state run does not allocate one histogram per
	// series per second.
	flushMu sync.Mutex
	merged  *aggMap

	// failures explains what went wrong, which the numeric metrics cannot.
	failures *failureTable
}

// NewRecorder builds an aggregator.
func NewRecorder(cfg RecorderConfig) *Recorder {
	cfg.applyDefaults()

	shards := make([]*shard, cfg.Shards)
	for i := range shards {
		shards[i] = &shard{aggs: newAggMap(cfg.Histogram)}
	}

	return &Recorder{
		cfg:       cfg,
		shards:    shards,
		mask:      uint64(cfg.Shards - 1),
		trendMask: uint64(cfg.TrendShards - 1),
		distinct:  make(map[seriesKey]struct{}),
		merged:    newAggMap(cfg.Histogram),
		failures:  newFailureTable(cfg.MaxFailureKinds),
	}
}

// Config returns the effective configuration, with defaults resolved.
func (r *Recorder) Config() RecorderConfig { return r.cfg }

// ForVU returns a Recorder view pinned to one shard.
//
// Pinning by virtual user rather than choosing a shard per observation spreads
// load evenly while keeping each VU's writes on one lock, which is both faster
// and kinder to the cache than a global round robin.
func (r *Recorder) ForVU(vuID int64) loadwave.Recorder {
	return pinnedRecorder{rec: r, hint: mixHash(uint64(vuID))}
}

// mixHash spreads sequential ids across shards. Virtual user ids are handed
// out consecutively, so masking them directly would be fine — but ids are also
// allocated in per-worker ranges, and without mixing, a worker granted the
// range [1024, 2048) would land every one of its VUs on the same few shards.
func mixHash(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// pinnedRecorder is the per-VU view returned by ForVU.
type pinnedRecorder struct {
	rec  *Recorder
	hint uint64
}

func (p pinnedRecorder) Count(metric string, labels loadwave.Labels, delta float64) {
	p.rec.record(loadwave.KindCounter, metric, labels, delta, false, p.hint)
}

func (p pinnedRecorder) Trend(metric string, labels loadwave.Labels, value float64) {
	p.rec.record(loadwave.KindTrend, metric, labels, value, false, p.hint)
}

func (p pinnedRecorder) Rate(metric string, labels loadwave.Labels, ok bool) {
	p.rec.record(loadwave.KindRate, metric, labels, 0, ok, p.hint)
}

func (p pinnedRecorder) Gauge(metric string, labels loadwave.Labels, value float64) {
	p.rec.record(loadwave.KindGauge, metric, labels, value, false, p.hint)
}

// ReportFailure implements loadwave.FailureReporter.
//
// Reached only on the failure path, so a healthy run never pays for it.
// Failures are aggregated by kind, so there is nothing per-virtual-user to
// keep and the pinned view simply forwards.
func (r *Recorder) ReportFailure(f loadwave.Failure) {
	r.failures.record(f, time.Now())
}

func (p pinnedRecorder) ReportFailure(f loadwave.Failure) { p.rec.ReportFailure(f) }

// Both the shared recorder and each virtual user's view report failures.
var (
	_ loadwave.FailureReporter = (*Recorder)(nil)
	_ loadwave.FailureReporter = pinnedRecorder{}
)

// The Recorder itself satisfies loadwave.Recorder, for callers that are not a
// virtual user — the worker reporting its own VU count, for instance. These
// are infrequent, so a round-robin shard hint is good enough.
var _ loadwave.Recorder = (*Recorder)(nil)

// Count adds to a counter. See ForVU for the per-virtual-user path.
func (r *Recorder) Count(metric string, labels loadwave.Labels, delta float64) {
	r.record(loadwave.KindCounter, metric, labels, delta, false, r.rr.Add(1))
}

// Trend records one observation in a distribution.
func (r *Recorder) Trend(metric string, labels loadwave.Labels, value float64) {
	r.record(loadwave.KindTrend, metric, labels, value, false, r.rr.Add(1))
}

// Rate records one boolean observation.
func (r *Recorder) Rate(metric string, labels loadwave.Labels, ok bool) {
	r.record(loadwave.KindRate, metric, labels, 0, ok, r.rr.Add(1))
}

// Gauge sets the current value of a gauge.
func (r *Recorder) Gauge(metric string, labels loadwave.Labels, value float64) {
	r.record(loadwave.KindGauge, metric, labels, value, false, r.rr.Add(1))
}

// record folds one observation into the right shard.
func (r *Recorder) record(
	kind loadwave.MetricKind, metric string, labels loadwave.Labels, value float64, truthy bool, hint uint64,
) {
	idx := hint & r.mask
	if kind == loadwave.KindTrend {
		idx = hint & r.trendMask
	}
	s := r.shards[idx]

	s.mu.Lock()
	agg := s.aggs.find(metric, labels)
	if agg == nil {
		if !r.admit(metric, labels) {
			s.mu.Unlock()
			r.dropped.Add(1)
			return
		}
		agg = s.aggs.getOrCreate(metric, labels, kind)
	}
	agg.observe(value, truthy, r.cfg.Histogram)
	s.mu.Unlock()
}

// admit reports whether a new series fits within the cardinality budget.
//
// Called with a shard lock held; it must never reach back for one, and no
// other path takes these two locks in the opposite order.
func (r *Recorder) admit(metric string, labels loadwave.Labels) bool {
	key := seriesKey{metric, labels.Hash()}

	r.distinctMu.Lock()
	defer r.distinctMu.Unlock()

	if _, known := r.distinct[key]; known {
		return true
	}
	if len(r.distinct) >= r.cfg.MaxSeries {
		return false
	}
	r.distinct[key] = struct{}{}
	return true
}

// Dropped reports how many observations were discarded for exceeding the
// series cap. A non-zero value means the reported numbers understate reality,
// which the dashboard surfaces rather than hides.
func (r *Recorder) Dropped() uint64 { return r.dropped.Load() }

// SeriesCount reports how many distinct series have been seen.
func (r *Recorder) SeriesCount() int {
	r.distinctMu.Lock()
	defer r.distinctMu.Unlock()
	return len(r.distinct)
}

// Flush drains everything recorded since the previous call and returns it as
// a wire batch, leaving the recorder empty and ready for the next interval.
//
// Batches carry deltas rather than running totals. A dropped batch then costs
// exactly one interval of data instead of skewing every interval that follows,
// which matters because a node reconnecting after a partition is a routine
// event rather than an exceptional one.
func (r *Recorder) Flush(
	runID, nodeID string, bucketStart time.Time, width time.Duration, activeVUs uint32,
) *loadwavev1.MetricBatch {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.merged.resetAll()

	for _, s := range r.shards {
		s.mu.Lock()
		s.aggs.each(func(a *aggregate) {
			if a.count == 0 {
				return
			}
			dst := r.merged.getOrCreate(a.metric, a.labels, a.kind)
			dst.mergeFrom(a)
		})
		s.aggs.resetAll()
		s.mu.Unlock()
	}

	batch := &loadwavev1.MetricBatch{
		RunId:          runID,
		NodeId:         nodeID,
		BucketStart:    timestamppb.New(bucketStart),
		BucketWidth:    durationpb.New(width),
		ActiveVus:      activeVUs,
		DroppedSamples: r.dropped.Load(),
		Failures:       r.failures.drain(),
		Series:         make([]*loadwavev1.SeriesDelta, 0, r.merged.len()),
	}

	r.merged.each(func(a *aggregate) {
		if a.count == 0 {
			return
		}
		batch.Series = append(batch.Series, encodeDelta(a))
	})
	return batch
}

// encodeDelta converts one aggregate into its wire form.
func encodeDelta(a *aggregate) *loadwavev1.SeriesDelta {
	delta := &loadwavev1.SeriesDelta{
		Metric:  a.metric,
		Kind:    encodeKind(a.kind),
		Tags:    a.labels.Map(),
		Count:   a.count,
		Sum:     a.sum,
		Min:     a.min(),
		Max:     a.max(),
		NonZero: a.nonZero,
	}
	if a.kind == loadwave.KindGauge {
		delta.Sum = a.gauge
	}
	if a.hist != nil && a.hist.TotalCount() > 0 {
		delta.Histogram = EncodeHistogram(a.hist)
	}
	return delta
}

// encodeKind maps the SDK's metric kind onto the wire enum.
func encodeKind(k loadwave.MetricKind) loadwavev1.MetricKind {
	switch k {
	case loadwave.KindCounter:
		return loadwavev1.MetricKind_METRIC_KIND_COUNTER
	case loadwave.KindGauge:
		return loadwavev1.MetricKind_METRIC_KIND_GAUGE
	case loadwave.KindTrend:
		return loadwavev1.MetricKind_METRIC_KIND_TREND
	case loadwave.KindRate:
		return loadwavev1.MetricKind_METRIC_KIND_RATE
	default:
		return loadwavev1.MetricKind_METRIC_KIND_UNSPECIFIED
	}
}

// DecodeKind maps the wire enum back onto the SDK's metric kind.
func DecodeKind(k loadwavev1.MetricKind) loadwave.MetricKind {
	switch k {
	case loadwavev1.MetricKind_METRIC_KIND_COUNTER:
		return loadwave.KindCounter
	case loadwavev1.MetricKind_METRIC_KIND_GAUGE:
		return loadwave.KindGauge
	case loadwavev1.MetricKind_METRIC_KIND_TREND:
		return loadwave.KindTrend
	case loadwavev1.MetricKind_METRIC_KIND_RATE:
		return loadwave.KindRate
	default:
		return loadwave.KindCounter
	}
}
