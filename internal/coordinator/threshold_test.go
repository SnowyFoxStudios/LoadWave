// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package coordinator

import (
	"testing"
	"time"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// storeWith builds a store holding one bucket of the given latencies, half of
// which are marked failed when `failures` says so.
func storeWith(t *testing.T, latencies []float64, failures int) *metrics.Store {
	t.Helper()

	rec := metrics.NewRecorder(metrics.RecorderConfig{})
	labels := loadwave.NewLabels(loadwave.LabelName, "GET /x")

	for i, latency := range latencies {
		rec.Count(loadwave.MetricHTTPReqs, labels, 1)
		rec.Trend(loadwave.MetricHTTPReqDuration, labels, latency)
		rec.Rate(loadwave.MetricHTTPReqFailed, labels, i < failures)
	}

	store := metrics.NewStore(metrics.StoreConfig{Resolution: time.Second})
	if err := store.Ingest(rec.Flush("run", "node", time.Now(), time.Second, 1)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return store
}

func threshold(metric string, stat loadwavev1.ThresholdStat, op loadwavev1.ThresholdOp, value float64) *loadwavev1.Threshold {
	return &loadwavev1.Threshold{Metric: metric, Stat: stat, Op: op, Value: value}
}

func TestEvaluateThresholds(t *testing.T) {
	t.Parallel()

	// 100 requests: ten at 500ms, ninety at 10ms. Ten are failures.
	latencies := make([]float64, 0, 100)
	for range 10 {
		latencies = append(latencies, 500)
	}
	for range 90 {
		latencies = append(latencies, 10)
	}
	store := storeWith(t, latencies, 10)

	results := EvaluateThresholds(store, []*loadwavev1.Threshold{
		threshold(loadwave.MetricHTTPReqDuration, loadwavev1.ThresholdStat_THRESHOLD_STAT_P95,
			loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 600),
		threshold(loadwave.MetricHTTPReqDuration, loadwavev1.ThresholdStat_THRESHOLD_STAT_P95,
			loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 100),
		threshold(loadwave.MetricHTTPReqFailed, loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE,
			loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 0.2),
		threshold(loadwave.MetricHTTPReqs, loadwavev1.ThresholdStat_THRESHOLD_STAT_COUNT,
			loadwavev1.ThresholdOp_THRESHOLD_OP_GTE, 100),
	})

	if len(results) != 4 {
		t.Fatalf("got %d results", len(results))
	}

	// p95 of that distribution is 500ms, so under 600 passes and under 100
	// fails. Getting this from the merged histogram is the point.
	if !results[0].Evaluated || !results[0].Passed {
		t.Errorf("p95 < 600 should pass, got %+v", results[0])
	}
	if !results[1].Evaluated || results[1].Passed {
		t.Errorf("p95 < 100 should fail, got %+v", results[1])
	}
	if !results[2].Passed {
		t.Errorf("failure rate 0.10 < 0.20 should pass, got %+v", results[2])
	}
	if !results[3].Passed {
		t.Errorf("count >= 100 should pass, got %+v", results[3])
	}

	if !AnyBreached(results) {
		t.Error("AnyBreached should be true")
	}
	if results[0].Description != "http_req_duration p95 < 600" {
		t.Errorf("description = %q", results[0].Description)
	}
}

func TestThresholdOnMissingMetricIsUnevaluated(t *testing.T) {
	t.Parallel()

	store := storeWith(t, []float64{10}, 0)

	// "We never measured it" and "it was fine" are very different answers to
	// give a CI pipeline, so an absent metric must not silently pass.
	results := EvaluateThresholds(store, []*loadwavev1.Threshold{
		threshold("never_emitted", loadwavev1.ThresholdStat_THRESHOLD_STAT_P95,
			loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 1),
	})

	if results[0].Evaluated {
		t.Fatalf("a metric that was never produced was evaluated: %+v", results[0])
	}
	if results[0].Passed {
		t.Fatal("an unevaluated threshold must not report a pass")
	}
	if AnyBreached(results) {
		t.Fatal("an unevaluated threshold must not report a breach either")
	}
}

func TestPercentileThresholdOnACounterIsUnevaluated(t *testing.T) {
	t.Parallel()

	store := storeWith(t, []float64{10, 20}, 0)

	// Asking for the p95 of a counter is a configuration mistake. Reporting
	// it as unevaluated surfaces that, where comparing against zero would
	// quietly pass forever.
	results := EvaluateThresholds(store, []*loadwavev1.Threshold{
		threshold(loadwave.MetricHTTPReqs, loadwavev1.ThresholdStat_THRESHOLD_STAT_P99,
			loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 1),
	})

	if results[0].Evaluated {
		t.Fatalf("a percentile of a counter was evaluated: %+v", results[0])
	}
}

func TestAbortRequested(t *testing.T) {
	t.Parallel()

	store := storeWith(t, []float64{1000, 1000}, 2)

	failing := threshold(loadwave.MetricHTTPReqFailed, loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE,
		loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 0.01)
	failing.AbortOnFail = true

	results := EvaluateThresholds(store, []*loadwavev1.Threshold{failing})
	breach, abort := AbortRequested(results)
	if !abort {
		t.Fatal("a failing abort-on-fail threshold should request an abort")
	}
	if breach.Metric != loadwave.MetricHTTPReqFailed {
		t.Fatalf("wrong breach reported: %+v", breach)
	}

	// Without the flag, a breach fails the run at the end but does not cut
	// it short.
	plain := threshold(loadwave.MetricHTTPReqFailed, loadwavev1.ThresholdStat_THRESHOLD_STAT_RATE,
		loadwavev1.ThresholdOp_THRESHOLD_OP_LT, 0.01)
	if _, abort := AbortRequested(EvaluateThresholds(store, []*loadwavev1.Threshold{plain})); abort {
		t.Fatal("a plain threshold should not request an abort")
	}
}

func TestThresholdOperators(t *testing.T) {
	t.Parallel()

	store := storeWith(t, []float64{10, 10, 10, 10}, 0)
	const count = 4

	cases := []struct {
		op    loadwavev1.ThresholdOp
		value float64
		want  bool
	}{
		{loadwavev1.ThresholdOp_THRESHOLD_OP_LT, count + 1, true},
		{loadwavev1.ThresholdOp_THRESHOLD_OP_LT, count, false},
		{loadwavev1.ThresholdOp_THRESHOLD_OP_LTE, count, true},
		{loadwavev1.ThresholdOp_THRESHOLD_OP_GT, count - 1, true},
		{loadwavev1.ThresholdOp_THRESHOLD_OP_GT, count, false},
		{loadwavev1.ThresholdOp_THRESHOLD_OP_GTE, count, true},
	}

	for _, tc := range cases {
		results := EvaluateThresholds(store, []*loadwavev1.Threshold{
			threshold(loadwave.MetricHTTPReqs, loadwavev1.ThresholdStat_THRESHOLD_STAT_COUNT, tc.op, tc.value),
		})
		if results[0].Passed != tc.want {
			t.Errorf("count %s %v = %v, want %v",
				opLabels[tc.op], tc.value, results[0].Passed, tc.want)
		}
	}
}

func TestRunBreachLatches(t *testing.T) {
	t.Parallel()

	run := newRun("r", "test", &loadwavev1.TestPlan{}, metrics.NewStore(metrics.StoreConfig{}), 1)

	run.setThresholds([]ThresholdResult{{Evaluated: true, Passed: false}})
	if !run.Breached() {
		t.Fatal("a failing threshold should mark the run breached")
	}

	// A p95 that recovers by the end of the run still breached. A CI gate
	// looking only at the final instant would let a real regression through.
	run.setThresholds([]ThresholdResult{{Evaluated: true, Passed: true}})
	if !run.Breached() {
		t.Fatal("a breach must latch for the life of the run")
	}
}

func TestRunPhaseIsTerminalOnce(t *testing.T) {
	t.Parallel()

	run := newRun("r", "test", &loadwavev1.TestPlan{}, metrics.NewStore(metrics.StoreConfig{}), 1)

	if !run.setPhase(loadwavev1.RunPhase_RUN_PHASE_RUNNING, "") {
		t.Fatal("running should be accepted")
	}
	if !run.setPhase(loadwavev1.RunPhase_RUN_PHASE_ABORTED, "operator") {
		t.Fatal("aborted should be accepted")
	}

	// A straggler agent reporting "completed" after an abort must not be
	// able to rewrite history, and the exit code with it.
	if run.setPhase(loadwavev1.RunPhase_RUN_PHASE_COMPLETED, "late") {
		t.Fatal("a terminal phase was overwritten")
	}
	if run.Phase() != loadwavev1.RunPhase_RUN_PHASE_ABORTED {
		t.Fatalf("phase = %s, want aborted", run.Phase())
	}
}
