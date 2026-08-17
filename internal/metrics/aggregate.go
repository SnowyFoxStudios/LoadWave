// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"math"

	hdr "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// seriesKey identifies a time series by metric name and label hash.
//
// The hash alone cannot be trusted to identify a series — two distinct label
// sets can collide — so every lookup confirms the match against the stored
// labels and walks a chain on the rare occasion two series share a bucket.
// Keying on a concatenated string instead would be correct without the chain,
// but would allocate on every single observation.
type seriesKey struct {
	metric string
	hash   uint64
}

// aggregate accumulates observations for one series over one interval.
type aggregate struct {
	metric string
	labels loadwave.Labels
	kind   loadwave.MetricKind

	// count is the number of observations, whatever the kind.
	count uint64
	// sum totals the observed values. For a counter this is the quantity
	// that matters; for a trend it feeds the mean.
	sum float64
	// minVal and maxVal are +Inf and -Inf while count is zero.
	minVal float64
	maxVal float64
	// nonZero counts truthy observations, for rate metrics.
	nonZero uint64
	// gauge holds the most recent value set, for gauge metrics.
	gauge float64
	// hist is allocated only for trend metrics.
	hist *hdr.Histogram

	// next chains series whose label hashes collided.
	next *aggregate
}

// newAggregate creates an aggregate, allocating a histogram for trends.
func newAggregate(metric string, labels loadwave.Labels, kind loadwave.MetricKind, cfg HistogramConfig) *aggregate {
	a := &aggregate{
		metric: metric,
		labels: labels,
		kind:   kind,
		minVal: math.Inf(1),
		maxVal: math.Inf(-1),
	}
	if kind == loadwave.KindTrend {
		a.hist = cfg.New()
	}
	return a
}

// observe folds one value into the aggregate.
func (a *aggregate) observe(value float64, truthy bool, cfg HistogramConfig) {
	a.count++

	switch a.kind {
	case loadwave.KindGauge:
		a.gauge = value
		return
	case loadwave.KindRate:
		if truthy {
			a.nonZero++
		}
		return
	case loadwave.KindCounter, loadwave.KindTrend:
	}

	a.sum += value
	if value < a.minVal {
		a.minVal = value
	}
	if value > a.maxVal {
		a.maxVal = value
	}

	if a.hist != nil {
		// RecordValue only errors when the value is out of range, and
		// ScaleValue has already clamped it into range, so there is nothing
		// left to handle.
		_ = a.hist.RecordValue(cfg.ScaleValue(value))
	}
}

// mergeFrom folds another aggregate for the same series into this one.
func (a *aggregate) mergeFrom(other *aggregate) {
	if other.count == 0 {
		return
	}

	a.count += other.count
	a.sum += other.sum
	a.nonZero += other.nonZero
	if other.minVal < a.minVal {
		a.minVal = other.minVal
	}
	if other.maxVal > a.maxVal {
		a.maxVal = other.maxVal
	}

	// Gauges from different nodes describe different populations — this
	// node's active VUs plus that node's active VUs — so they add.
	if a.kind == loadwave.KindGauge {
		a.gauge += other.gauge
	}

	if a.hist != nil && other.hist != nil {
		a.hist.Merge(other.hist)
	}
}

// reset clears the accumulated values while keeping the allocated histogram,
// so a steady-state run stops allocating entirely after its first interval.
func (a *aggregate) reset() {
	a.count = 0
	a.sum = 0
	a.nonZero = 0
	a.gauge = 0
	a.minVal = math.Inf(1)
	a.maxVal = math.Inf(-1)
	if a.hist != nil {
		a.hist.Reset()
	}
}

// min returns the smallest observation, or zero if there were none.
func (a *aggregate) min() float64 {
	if a.count == 0 || math.IsInf(a.minVal, 1) {
		return 0
	}
	return a.minVal
}

// max returns the largest observation, or zero if there were none.
func (a *aggregate) max() float64 {
	if a.count == 0 || math.IsInf(a.maxVal, -1) {
		return 0
	}
	return a.maxVal
}

// aggMap is a collection of aggregates indexed by series.
//
// It is not safe for concurrent use; callers hold whatever lock is
// appropriate — a shard mutex on the recording path, nothing at all on the
// single-goroutine flush path.
type aggMap struct {
	entries map[seriesKey]*aggregate
	cfg     HistogramConfig
	size    int
}

func newAggMap(cfg HistogramConfig) *aggMap {
	return &aggMap{entries: make(map[seriesKey]*aggregate), cfg: cfg}
}

// find returns the aggregate for a series, or nil if it is not present.
func (m *aggMap) find(metric string, labels loadwave.Labels) *aggregate {
	for a := m.entries[seriesKey{metric, labels.Hash()}]; a != nil; a = a.next {
		if a.labels.Equal(labels) {
			return a
		}
	}
	return nil
}

// getOrCreate returns the aggregate for a series, creating it if it is not
// already present.
func (m *aggMap) getOrCreate(
	metric string, labels loadwave.Labels, kind loadwave.MetricKind,
) *aggregate {
	key := seriesKey{metric, labels.Hash()}
	head := m.entries[key]
	for a := head; a != nil; a = a.next {
		if a.labels.Equal(labels) {
			return a
		}
	}

	created := newAggregate(metric, labels, kind, m.cfg)
	created.next = head
	m.entries[key] = created
	m.size++
	return created
}

// each visits every aggregate. Iteration order is unspecified.
func (m *aggMap) each(visit func(*aggregate)) {
	for _, head := range m.entries {
		for a := head; a != nil; a = a.next {
			visit(a)
		}
	}
}

// resetAll zeroes every aggregate without releasing them.
func (m *aggMap) resetAll() {
	m.each((*aggregate).reset)
}

// len reports how many series are held.
func (m *aggMap) len() int { return m.size }
