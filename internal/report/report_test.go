// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package report_test

import (
	"strings"
	"testing"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/internal/report"
)

// sampleSnapshot builds a snapshot resembling a finished run.
func sampleSnapshot(breached bool) coordinator.Snapshot {
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	ticks := make([]coordinator.TickDTO, 0, 30)
	for i := range 30 {
		ticks = append(ticks, coordinator.TickDTO{
			T:      start.Add(time.Duration(i) * time.Second).UnixMilli(),
			VUs:    uint32(i),
			RPS:    float64(i * 3),
			Status: map[string]uint64{"2xx": uint64(i * 3), "5xx": uint64(i / 10)},
			Endpoints: map[string]coordinator.EndpointTick{
				"list products": {Avg: 12 + float64(i), Requests: uint64(i * 2)},
				"create order":  {Avg: 80 + float64(i*2), Requests: uint64(i), ErrorRate: 0.05},
			},
		})
	}

	return coordinator.Snapshot{
		Run: &coordinator.Summary{
			ID:         "run-20260817-120000-abc123",
			Name:       "storefront",
			Phase:      coordinator.PhaseCompleted,
			CreatedAt:  start,
			ElapsedSec: 30,
			PeakVUs:    50,
			Profile:    "30s to 50 VUs",
			BaseURL:    "https://staging.example.com",
			Breached:   breached,
			Thresholds: []coordinator.ThresholdResult{
				{
					Description: "http_req_duration p95 < 500",
					Evaluated:   true,
					Passed:      !breached,
					Actual:      46.3,
				},
				{Description: "never_seen p95 < 1", Evaluated: false},
			},
			Stats: metrics.Stats{Series: 12},
		},
		Ticks: ticks,
		Totals: map[string]metrics.SeriesSummary{
			"http_reqs":         {Count: 1800},
			"iterations":        {Count: 900},
			"http_req_duration": {Count: 1800, Avg: 31, Percentiles: map[string]float64{"p95": 46.3, "p99": 91}},
			"http_req_failed":   {Count: 1800, Rate: 0.015},
			"checks":            {Count: 1800, Rate: 0.985},
		},
		Endpoints: []metrics.EndpointSummary{
			{Name: "create order", Requests: 600, Avg: 80, ErrorRate: 0.05,
				Percentiles: map[string]float64{"p95": 120, "p99": 300}},
			{Name: "list products", Requests: 1200, Avg: 13,
				Percentiles: map[string]float64{"p95": 20, "p99": 24}},
		},
		Failures: []metrics.FailureSummary{
			{Name: "create order", Method: "POST", Status: 502, Message: "payment declined", Count: 27},
			{Name: "list products", Method: "GET", ErrorClass: "connection_refused", Count: 3},
		},
	}
}

func render(t *testing.T, snapshot coordinator.Snapshot) string {
	t.Helper()

	data, err := report.Build(snapshot, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var out strings.Builder
	if err := report.Render(&out, data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out.String()
}

// The report's whole value is being openable years later, offline, from an
// email attachment. Anything fetched over the network defeats that.
func TestReportIsSelfContained(t *testing.T) {
	t.Parallel()

	html := render(t, sampleSnapshot(false))

	for _, forbidden := range []string{"<script", "src=\"http", "href=\"http", "@import", "url(http"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("report contains %q; it must have no external dependency", forbidden)
		}
	}
	if !strings.Contains(html, "<svg class=\"chart\"") {
		t.Error("no inline chart was rendered")
	}
}

func TestReportContainsTheResults(t *testing.T) {
	t.Parallel()

	html := render(t, sampleSnapshot(false))

	for _, want := range []string{
		"storefront",
		"run-20260817-120000-abc123",
		"Virtual users",
		"Response time",
		"Responses by status",
		"create order",
		"list products",
		// The failure detail is the half the metrics cannot express.
		"payment declined",
		"connection refused",
		"502",
		// A threshold that never ran must not be reported as a pass.
		"not measured",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestReportVerdict(t *testing.T) {
	t.Parallel()

	passing := render(t, sampleSnapshot(false))
	if !strings.Contains(passing, "Passed") {
		t.Error("a clean run should be reported as passed")
	}

	breaching := render(t, sampleSnapshot(true))
	if !strings.Contains(breaching, "Thresholds breached") {
		t.Error("a breaching run should say so in the banner")
	}
	// The verdict must be readable without colour, for print and for
	// colour-vision deficiency.
	if !strings.Contains(breaching, "did not hold") {
		t.Error("the breach should be explained in words, not only by colour")
	}
}

func TestReportRejectsSnapshotWithNoRun(t *testing.T) {
	t.Parallel()

	if _, err := report.Build(coordinator.Snapshot{}, time.Now()); err == nil {
		t.Fatal("a snapshot with no run was accepted")
	}
}

// A run that produced nothing must still render rather than crash: an
// operator whose test failed immediately still wants the report that says so.
func TestReportSurvivesAnEmptyRun(t *testing.T) {
	t.Parallel()

	html := render(t, coordinator.Snapshot{
		Run: &coordinator.Summary{
			ID:    "run-empty",
			Name:  "empty",
			Phase: coordinator.PhaseFailed,
			Stats: metrics.Stats{},
		},
	})

	if !strings.Contains(html, "Run failed") {
		t.Error("a failed run should be reported as such")
	}
	if !strings.Contains(html, "No requests were recorded") {
		t.Error("an empty run should say so rather than showing an empty table")
	}
}
