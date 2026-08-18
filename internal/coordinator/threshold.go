// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"fmt"
	"math"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
)

// ThresholdResult is one threshold's verdict.
type ThresholdResult struct {
	Metric string  `json:"metric"`
	Stat   string  `json:"stat"`
	Op     string  `json:"op"`
	Value  float64 `json:"value"`

	// Actual is the observed statistic. Meaningful only when Evaluated.
	Actual float64 `json:"actual"`

	// Evaluated is false when the metric has not been observed yet. A
	// threshold on a metric that never appeared is reported as unevaluated
	// rather than as a pass, because "we never measured it" and "it was fine"
	// are very different things to hand back to a CI pipeline.
	Evaluated bool `json:"evaluated"`

	Passed bool `json:"passed"`

	// AbortOnFail means a breach should end the run immediately.
	AbortOnFail bool `json:"abortOnFail"`

	// Description renders the assertion for display, e.g.
	// "http_req_duration p95 < 500".
	Description string `json:"description"`
}

// statLabels render the wire enum for display and lookup.
var statLabels = map[loadwavev1.ThresholdStat]string{
	loadwavev1.ThresholdStat_THRESHOLD_STAT_COUNT: "count",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE:  "rate",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_AVG:   "avg",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_MIN:   "min",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_MAX:   "max",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_P50:   "p50",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_P90:   "p90",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_P95:   "p95",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_P99:   "p99",
	loadwavev1.ThresholdStat_THRESHOLD_STAT_P999:  "p999",
}

// opLabels render the comparison for display.
var opLabels = map[loadwavev1.ThresholdOp]string{
	loadwavev1.ThresholdOp_THRESHOLD_OP_LT:  "<",
	loadwavev1.ThresholdOp_THRESHOLD_OP_LTE: "<=",
	loadwavev1.ThresholdOp_THRESHOLD_OP_GT:  ">",
	loadwavev1.ThresholdOp_THRESHOLD_OP_GTE: ">=",
}

// EvaluateThresholds checks every threshold against the run's cumulative
// metrics.
//
// Evaluation is cumulative rather than windowed: a threshold answers "was this
// run acceptable overall", which is the question a CI gate is asking. A
// momentary spike that the run recovered from does not fail the build unless
// it moved the whole-run statistic past the line.
func EvaluateThresholds(store *metrics.Store, thresholds []*loadwavev1.Threshold) []ThresholdResult {
	results := make([]ThresholdResult, 0, len(thresholds))

	for _, t := range thresholds {
		statLabel := statLabels[t.GetStat()]
		opLabel := opLabels[t.GetOp()]

		result := ThresholdResult{
			Metric:      t.GetMetric(),
			Stat:        statLabel,
			Op:          opLabel,
			Value:       t.GetValue(),
			AbortOnFail: t.GetAbortOnFail(),
			Description: fmt.Sprintf("%s %s %s %g", t.GetMetric(), statLabel, opLabel, t.GetValue()),
		}

		summary, found := store.Aggregate(t.GetMetric())
		if !found || summary.Count == 0 {
			results = append(results, result)
			continue
		}

		actual, ok := statValue(summary, t.GetStat())
		if !ok {
			results = append(results, result)
			continue
		}

		result.Evaluated = true
		result.Actual = actual
		result.Passed = compare(actual, t.GetOp(), t.GetValue())
		results = append(results, result)
	}
	return results
}

// statValue pulls one statistic out of a series summary.
func statValue(summary metrics.SeriesSummary, stat loadwavev1.ThresholdStat) (float64, bool) {
	switch stat {
	case loadwavev1.ThresholdStat_THRESHOLD_STAT_COUNT:
		return float64(summary.Count), true
	case loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE:
		return summary.Rate, true
	case loadwavev1.ThresholdStat_THRESHOLD_STAT_AVG:
		return summary.Avg, true
	case loadwavev1.ThresholdStat_THRESHOLD_STAT_MIN:
		return summary.Min, true
	case loadwavev1.ThresholdStat_THRESHOLD_STAT_MAX:
		return summary.Max, true
	default:
		// The rest are percentiles, handled below.
	}

	// The remaining statistics are percentiles, which only exist for metrics
	// that carry a distribution. Asking for the p95 of a counter is a
	// configuration mistake, and reporting it as unevaluated surfaces that
	// rather than quietly comparing against zero.
	key, ok := statLabels[stat]
	if !ok {
		return 0, false
	}
	value, ok := summary.Percentiles[key]
	return value, ok
}

// compare applies a threshold's operator.
func compare(actual float64, op loadwavev1.ThresholdOp, want float64) bool {
	if math.IsNaN(actual) {
		return false
	}
	switch op {
	case loadwavev1.ThresholdOp_THRESHOLD_OP_LT:
		return actual < want
	case loadwavev1.ThresholdOp_THRESHOLD_OP_LTE:
		return actual <= want
	case loadwavev1.ThresholdOp_THRESHOLD_OP_GT:
		return actual > want
	case loadwavev1.ThresholdOp_THRESHOLD_OP_GTE:
		return actual >= want
	default:
		return false
	}
}

// AnyBreached reports whether any evaluated threshold failed.
func AnyBreached(results []ThresholdResult) bool {
	for _, r := range results {
		if r.Evaluated && !r.Passed {
			return true
		}
	}
	return false
}

// AbortRequested reports whether a failing threshold asked for the run to be
// cut short.
func AbortRequested(results []ThresholdResult) (ThresholdResult, bool) {
	for _, r := range results {
		if r.Evaluated && !r.Passed && r.AbortOnFail {
			return r, true
		}
	}
	return ThresholdResult{}, false
}
