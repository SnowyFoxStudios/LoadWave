// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	loadwavev1 "github.com/SnowyFoxStudios/LoadWave/gen/loadwave/v1"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

func TestHistogramRLERoundTrip(t *testing.T) {
	t.Parallel()

	cfg := DefaultHistogramConfig()
	hist := cfg.New()
	for i := 1; i <= 1000; i++ {
		if err := hist.RecordValue(cfg.ScaleValue(float64(i))); err != nil {
			t.Fatalf("RecordValue: %v", err)
		}
	}

	encoded := EncodeHistogram(hist)
	if encoded.GetTotalCount() != 1000 {
		t.Fatalf("total count = %d", encoded.GetTotalCount())
	}

	// The compression is the reason a distributed run's control plane stays
	// affordable, so a regression that silently disables it should fail here.
	full := CountsLen(cfg)
	if len(encoded.GetCountsRle()) >= full {
		t.Fatalf("RLE produced %d values for %d buckets — no compression", len(encoded.GetCountsRle()), full)
	}

	decoded, err := DecodeHistogram(encoded, cfg)
	if err != nil {
		t.Fatalf("DecodeHistogram: %v", err)
	}
	if decoded.TotalCount() != hist.TotalCount() {
		t.Fatalf("count changed: %d -> %d", hist.TotalCount(), decoded.TotalCount())
	}
	for _, q := range []float64{50, 90, 99} {
		if got, want := decoded.ValueAtQuantile(q), hist.ValueAtQuantile(q); got != want {
			t.Fatalf("p%v changed: %d -> %d", q, want, got)
		}
	}
}

func TestDecodeHistogramRejectsMismatchedResolution(t *testing.T) {
	t.Parallel()

	// Merging histograms with different bucket layouts would silently corrupt
	// every percentile in the run, so a node using another resolution has to
	// be refused rather than accommodated.
	other := HistogramConfig{Lowest: 1, Highest: 1000, SigFigs: 3}
	snapshot := EncodeHistogram(other.New())

	if _, err := DecodeHistogram(snapshot, DefaultHistogramConfig()); err == nil {
		t.Fatal("expected a resolution mismatch error")
	}
}

func TestDecodeHistogramRejectsMalformedEncoding(t *testing.T) {
	t.Parallel()

	cfg := DefaultHistogramConfig()
	cases := map[string][]int64{
		"trailing zero marker": {5, 0},
		"negative count":       {-3},
		"zero run length":      {0, 0},
		"negative run length":  {0, -5},
	}

	for name, counts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snapshot := &loadwavev1.HistogramSnapshot{
				LowestTrackableValue:  cfg.Lowest,
				HighestTrackableValue: cfg.Highest,
				SignificantFigures:    int32(cfg.SigFigs),
				CountsRle:             counts,
			}
			if _, err := DecodeHistogram(snapshot, cfg); err == nil {
				t.Fatal("malformed encoding was accepted")
			}
		})
	}
}

func TestHistogramClampsOutOfRangeValues(t *testing.T) {
	t.Parallel()

	cfg := DefaultHistogramConfig()

	// A request that hit its timeout is the most interesting sample in the
	// run; recording it at the ceiling understates it, but dropping it would
	// make the tail look better than it is.
	if got := cfg.ScaleValue(1e9); got != cfg.Highest {
		t.Errorf("huge value scaled to %d, want the ceiling %d", got, cfg.Highest)
	}
	if got := cfg.ScaleValue(0); got != cfg.Lowest {
		t.Errorf("zero scaled to %d, want the floor %d", got, cfg.Lowest)
	}
	if got := cfg.ScaleValue(math.NaN()); got != cfg.Lowest {
		t.Errorf("NaN scaled to %d", got)
	}
	if got := UnscaleValue(cfg.ScaleValue(42.5)); math.Abs(got-42.5) > 0.01 {
		t.Errorf("round trip of 42.5 gave %v", got)
	}
}

func TestRecorderFlushProducesDeltas(t *testing.T) {
	t.Parallel()

	rec := NewRecorder(RecorderConfig{})
	labels := loadwave.NewLabels("name", "GET /x")

	rec.Count(loadwave.MetricHTTPReqs, labels, 1)
	rec.Count(loadwave.MetricHTTPReqs, labels, 1)
	rec.Trend(loadwave.MetricHTTPReqDuration, labels, 10)
	rec.Trend(loadwave.MetricHTTPReqDuration, labels, 30)
	rec.Rate(loadwave.MetricHTTPReqFailed, labels, true)
	rec.Rate(loadwave.MetricHTTPReqFailed, labels, false)

	batch := rec.Flush("run", "node", time.Now(), time.Second, 4)
	byMetric := indexBatch(batch)

	if got := byMetric[loadwave.MetricHTTPReqs].GetSum(); got != 2 {
		t.Errorf("http_reqs sum = %v, want 2", got)
	}
	duration := byMetric[loadwave.MetricHTTPReqDuration]
	if duration.GetCount() != 2 || duration.GetSum() != 40 {
		t.Errorf("duration = count %d sum %v", duration.GetCount(), duration.GetSum())
	}
	if duration.GetMin() != 10 || duration.GetMax() != 30 {
		t.Errorf("duration min/max = %v/%v", duration.GetMin(), duration.GetMax())
	}
	if duration.GetHistogram() == nil {
		t.Error("trend metric has no histogram")
	}
	failed := byMetric[loadwave.MetricHTTPReqFailed]
	if failed.GetCount() != 2 || failed.GetNonZero() != 1 {
		t.Errorf("rate = %d of %d", failed.GetNonZero(), failed.GetCount())
	}
	if batch.GetActiveVus() != 4 {
		t.Errorf("activeVUs = %d", batch.GetActiveVus())
	}

	// The second flush must be empty. Batches are deltas, so a recorder that
	// failed to reset would restate the same numbers every interval and
	// inflate the whole run.
	second := rec.Flush("run", "node", time.Now(), time.Second, 4)
	if len(second.GetSeries()) != 0 {
		t.Fatalf("second flush returned %d series, want none", len(second.GetSeries()))
	}
}

func TestRecorderEnforcesTheSeriesCap(t *testing.T) {
	t.Parallel()

	rec := NewRecorder(RecorderConfig{MaxSeries: 10})

	// A runaway tag — a user id used as a label — must be bounded rather than
	// allowed to exhaust memory.
	for i := range 100 {
		rec.Count("runaway", loadwave.NewLabels("user", string(rune('a'+i%26))+string(rune('a'+i/26))), 1)
	}

	if got := rec.SeriesCount(); got > 10 {
		t.Fatalf("tracked %d series, cap was 10", got)
	}
	if rec.Dropped() == 0 {
		t.Fatal("dropped counter never incremented; loss would be invisible")
	}
}

func TestRecorderIsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()

	rec := NewRecorder(RecorderConfig{})
	const writers, perWriter = 16, 500

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			vu := rec.ForVU(int64(id))
			labels := loadwave.NewLabels("name", "shared")
			for range perWriter {
				vu.Count(loadwave.MetricHTTPReqs, labels, 1)
				vu.Trend(loadwave.MetricHTTPReqDuration, labels, 5)
			}
		}(w)
	}
	wg.Wait()

	batch := rec.Flush("run", "node", time.Now(), time.Second, 0)
	byMetric := indexBatch(batch)

	if got, want := byMetric[loadwave.MetricHTTPReqs].GetSum(), float64(writers*perWriter); got != want {
		t.Fatalf("counter lost writes: %v, want %v", got, want)
	}
	if got, want := byMetric[loadwave.MetricHTTPReqDuration].GetCount(), uint64(writers*perWriter); got != want {
		t.Fatalf("trend lost writes: %d, want %d", got, want)
	}
}

func indexBatch(batch *loadwavev1.MetricBatch) map[string]*loadwavev1.SeriesDelta {
	out := make(map[string]*loadwavev1.SeriesDelta)
	for _, series := range batch.GetSeries() {
		out[series.GetMetric()] = series
	}
	return out
}

// buildBatch assembles a wire batch the way a worker would.
func buildBatch(t *testing.T, nodeID string, start time.Time, vus uint32, values ...float64) *loadwavev1.MetricBatch {
	t.Helper()

	rec := NewRecorder(RecorderConfig{})
	labels := loadwave.NewLabels(loadwave.LabelScenario, "browse", loadwave.LabelName, "GET /x",
		loadwave.LabelStatus, "200")

	for _, v := range values {
		rec.Count(loadwave.MetricHTTPReqs, labels, 1)
		rec.Trend(loadwave.MetricHTTPReqDuration, labels, v)
		rec.Rate(loadwave.MetricHTTPReqFailed, labels, false)
	}

	batch := rec.Flush("run-1", nodeID, start, time.Second, vus)
	batch.BucketStart = timestamppb.New(start)
	batch.BucketWidth = durationpb.New(time.Second)
	return batch
}

func TestStoreMergesAcrossNodes(t *testing.T) {
	t.Parallel()

	store := NewStore(StoreConfig{Resolution: time.Second, LateGrace: time.Second})
	bucket := time.Now().Truncate(time.Second)

	// Two nodes reporting the same bucket. The merged p99 has to come from
	// the combined distribution: averaging the two nodes' percentiles would
	// be wrong, which is the whole reason histograms travel over the wire.
	if err := store.Ingest(buildBatch(t, "node-a", bucket, 5, 10, 10, 10, 10)); err != nil {
		t.Fatalf("Ingest node-a: %v", err)
	}
	if err := store.Ingest(buildBatch(t, "node-b", bucket, 7, 100, 100, 100, 100)); err != nil {
		t.Fatalf("Ingest node-b: %v", err)
	}

	totals := store.Totals()
	reqs := totals[loadwave.MetricHTTPReqs]
	if reqs.Count != 8 {
		t.Fatalf("merged request count = %d, want 8", reqs.Count)
	}

	duration := totals[loadwave.MetricHTTPReqDuration]
	if duration.Count != 8 {
		t.Fatalf("merged duration count = %d, want 8", duration.Count)
	}
	if got := duration.Percentiles["p99"]; got < 90 || got > 110 {
		t.Fatalf("merged p99 = %v, want roughly 100", got)
	}
	if got := duration.Percentiles["p50"]; got > 60 {
		t.Fatalf("merged p50 = %v, want roughly 10 to 55", got)
	}

	store.CloseStale(bucket.Add(10 * time.Second))
	timeline := store.Timeline(time.Time{})
	if len(timeline) != 1 {
		t.Fatalf("timeline has %d buckets, want 1", len(timeline))
	}

	// Each node's VU count is tracked separately and summed, so the cluster
	// total is 12 rather than whichever node reported last.
	if got := timeline[0].ActiveVUs; got != 12 {
		t.Fatalf("bucket VUs = %d, want 12", got)
	}
	if got := timeline[0].Status["2xx"]; got != 8 {
		t.Fatalf("2xx count = %d, want 8", got)
	}
}

func TestStoreIgnoresRedeliveredVUCounts(t *testing.T) {
	t.Parallel()

	store := NewStore(StoreConfig{Resolution: time.Second, LateGrace: time.Second})
	bucket := time.Now().Truncate(time.Second)

	// A reconnecting node can restate a bucket. Its VU count must replace
	// its own previous figure, not add to it.
	_ = store.Ingest(buildBatch(t, "node-a", bucket, 5, 10))
	_ = store.Ingest(buildBatch(t, "node-a", bucket, 5, 10))

	store.CloseStale(bucket.Add(10 * time.Second))
	timeline := store.Timeline(time.Time{})
	if len(timeline) != 1 {
		t.Fatalf("timeline has %d buckets", len(timeline))
	}
	if got := timeline[0].ActiveVUs; got != 5 {
		t.Fatalf("VUs = %d after a redelivery, want 5", got)
	}
}

func TestStoreRejectsBucketsPastTheGracePeriod(t *testing.T) {
	t.Parallel()

	store := NewStore(StoreConfig{Resolution: time.Second, LateGrace: 2 * time.Second})
	now := time.Now().Truncate(time.Second)

	if err := store.Ingest(buildBatch(t, "node-a", now, 1, 10)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Far enough behind that its bucket has already been published; silently
	// creating it would make the chart go backwards.
	if err := store.Ingest(buildBatch(t, "node-b", now.Add(-30*time.Second), 1, 10)); err == nil {
		t.Fatal("a badly late bucket was accepted")
	}
	if store.Stats().DroppedLate == 0 {
		t.Error("late drops were not counted")
	}
}

func TestStoreEndpointsMergePerName(t *testing.T) {
	t.Parallel()

	store := NewStore(StoreConfig{Resolution: time.Second, LateGrace: time.Second})
	bucket := time.Now().Truncate(time.Second)

	rec := NewRecorder(RecorderConfig{})
	fast := loadwave.NewLabels(loadwave.LabelName, "GET /fast", loadwave.LabelStatus, "200")
	slowOK := loadwave.NewLabels(loadwave.LabelName, "GET /slow", loadwave.LabelStatus, "200")
	slowBad := loadwave.NewLabels(loadwave.LabelName, "GET /slow", loadwave.LabelStatus, "500")

	for range 100 {
		rec.Count(loadwave.MetricHTTPReqs, fast, 1)
		rec.Trend(loadwave.MetricHTTPReqDuration, fast, 5)
		rec.Rate(loadwave.MetricHTTPReqFailed, fast, false)
	}
	for range 90 {
		rec.Count(loadwave.MetricHTTPReqs, slowOK, 1)
		rec.Trend(loadwave.MetricHTTPReqDuration, slowOK, 200)
		rec.Rate(loadwave.MetricHTTPReqFailed, slowOK, false)
	}
	for range 10 {
		rec.Count(loadwave.MetricHTTPReqs, slowBad, 1)
		rec.Trend(loadwave.MetricHTTPReqDuration, slowBad, 900)
		rec.Rate(loadwave.MetricHTTPReqFailed, slowBad, true)
	}

	if err := store.Ingest(rec.Flush("run-1", "node-a", bucket, time.Second, 1)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	endpoints := store.Endpoints()
	if len(endpoints) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(endpoints))
	}
	// Sorted slowest first, because that is what an operator is looking for.
	if endpoints[0].Name != "GET /slow" {
		t.Fatalf("first endpoint = %q, want the slowest", endpoints[0].Name)
	}

	slow := endpoints[0]
	if slow.Requests != 100 {
		t.Errorf("slow requests = %d, want 100", slow.Requests)
	}
	if math.Abs(slow.ErrorRate-0.10) > 0.001 {
		t.Errorf("slow error rate = %v, want 0.10", slow.ErrorRate)
	}
	// The two status slices must be merged before the percentile is taken.
	// Reading the p95 off the 500s alone would report ~900ms; off the 200s
	// alone, ~200ms. The true p95 of the combined distribution is ~900.
	if got := slow.Percentiles["p95"]; got < 800 {
		t.Errorf("slow p95 = %v; the status slices were not merged", got)
	}
	if got := slow.Percentiles["p50"]; got > 250 {
		t.Errorf("slow p50 = %v, want roughly 200", got)
	}
}

func TestStoreEvictsBeyondTheWindow(t *testing.T) {
	t.Parallel()

	store := NewStore(StoreConfig{
		Resolution: time.Second,
		Window:     5 * time.Second,
		LateGrace:  time.Second,
	})
	start := time.Now().Truncate(time.Second)

	for i := range 20 {
		bucket := start.Add(time.Duration(i) * time.Second)
		_ = store.Ingest(buildBatch(t, "node-a", bucket, 1, 10))
		store.CloseStale(bucket.Add(3 * time.Second))
	}

	timeline := store.Timeline(time.Time{})
	if len(timeline) > 5 {
		t.Fatalf("retained %d buckets, window allows 5", len(timeline))
	}
	// Whatever survives must still be in order, or the chart draws backwards.
	if !slices.IsSortedFunc(timeline, func(a, b Bucket) int { return a.Start.Compare(b.Start) }) {
		t.Fatal("timeline is not in chronological order")
	}
}

func BenchmarkRecorderTrend(b *testing.B) {
	rec := NewRecorder(RecorderConfig{})
	vu := rec.ForVU(1)
	labels := loadwave.NewLabels("name", "GET /x", "status", "200")
	b.ReportAllocs()

	for b.Loop() {
		vu.Trend(loadwave.MetricHTTPReqDuration, labels, 12.5)
	}
}
