// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package report renders a finished run as a single self-contained HTML file.
//
// The file has no scripts, no external stylesheet, no web font and no network
// dependency of any kind: the charts are inline SVG and the styling is inline
// CSS. That is the whole point. A load-test result gets attached to a ticket,
// mailed to somebody, or opened two years later to settle an argument about
// when a regression started, and any of those uses is defeated by a file that
// needs a CDN to render.
package report

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/buildinfo"
	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

//go:embed report.html
var reportTemplate string

// Categorical palette, matching the dashboard's.
//
// Both are validated as a set for contrast and colour-vision deficiency; see
// the note at the top of web/src/index.css. Do not change one in isolation.
var endpointColors = []string{
	"#2a78d6", "#eb6834", "#1baf7a", "#eda100",
	"#e87ba4", "#008300", "#4a3aa7", "#e34948",
}

// Status-class colours, matching the dashboard's.
var statusColors = map[string]string{
	"2xx":   "#0ca30c",
	"3xx":   "#2a78d6",
	"4xx":   "#eda100",
	"5xx":   "#d03b3b",
	"error": "#4a3aa7",
	"1xx":   "#7a7873",
	"other": "#7a7873",
}

// maxChartedEndpoints matches the dashboard: eight validated hues, and no
// ninth invented for a ninth endpoint.
const maxChartedEndpoints = 8

// statusOrder is the stacking order, by severity.
var statusOrder = []string{"2xx", "3xx", "4xx", "5xx", "error", "1xx", "other"}

// Data is everything the template renders.
type Data struct {
	Run         coordinator.Summary
	Generated   time.Time
	Version     string
	Verdict     Verdict
	Totals      map[string]metrics.SeriesSummary
	Headline    []Stat
	Charts      []Chart
	Endpoints   []metrics.EndpointSummary
	Failures    []metrics.FailureSummary
	Thresholds  []coordinator.ThresholdResult
	Agents      []coordinator.AgentInfo
	Events      []coordinator.Event
	Warnings    []string
	OmittedEnds int
}

// Verdict is the report's headline judgement.
type Verdict struct {
	// Passed is true only when the run completed and no threshold breached.
	Passed bool
	// Label is the short word shown in the banner.
	Label string
	// Detail explains it in a sentence.
	Detail string
}

// Stat is one headline figure.
type Stat struct {
	Label  string
	Value  string
	Detail string
}

// Build assembles a report from a coordinator snapshot.
func Build(snapshot coordinator.Snapshot, now time.Time) (Data, error) {
	if snapshot.Run == nil {
		return Data{}, errors.New("the snapshot has no run to report on")
	}

	data := Data{
		Run:        *snapshot.Run,
		Generated:  now,
		Version:    buildinfo.Version(),
		Totals:     snapshot.Totals,
		Endpoints:  snapshot.Endpoints,
		Failures:   snapshot.Failures,
		Thresholds: snapshot.Run.Thresholds,
		Agents:     snapshot.Agents,
		Events:     snapshot.Events,
	}
	data.Verdict = verdictOf(*snapshot.Run)
	data.Headline = headline(snapshot)
	data.Warnings = warnings(snapshot.Run.Stats)
	data.Charts, data.OmittedEnds = charts(snapshot)

	return data, nil
}

// Render writes the report as HTML.
func Render(w io.Writer, data Data) error {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"millis":  formatMillis,
		"count":   formatCount,
		"percent": formatPercent,
		"bytes":   formatBytes,
		"since":   func(t time.Time) string { return t.Format("15:04:05") },
		"stamp":   func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
		"seconds": formatSeconds,
		"code":    failureCode,
	}).Parse(reportTemplate)
	if err != nil {
		return fmt.Errorf("parse report template: %w", err)
	}

	if err := tmpl.Execute(w, data); err != nil {
		return fmt.Errorf("render report: %w", err)
	}
	return nil
}

// Filename suggests a name for the downloaded file.
func Filename(run coordinator.Summary) string {
	stamp := run.CreatedAt.UTC().Format("20060102-150405")
	name := run.Name
	if name == "" {
		name = "loadwave"
	}
	return fmt.Sprintf("%s-%s.html", name, stamp)
}

// verdictOf reduces the run to a single judgement.
//
// The distinction the banner has to carry is the same one the exit codes make:
// a run that could not be carried out is a different thing from a run that
// went fine and found the service too slow.
func verdictOf(run coordinator.Summary) Verdict {
	switch {
	case run.Phase == coordinator.PhaseFailed:
		return Verdict{Label: "Run failed", Detail: orDefault(run.Failure, "the run could not be completed")}
	case run.Breached:
		failing := 0
		for _, t := range run.Thresholds {
			if t.Evaluated && !t.Passed {
				failing++
			}
		}
		return Verdict{
			Label:  "Thresholds breached",
			Detail: fmt.Sprintf("The run completed, but %s did not hold.", plural(failing, "threshold", "thresholds")),
		}
	case run.Phase == coordinator.PhaseAborted:
		return Verdict{Label: "Aborted", Detail: orDefault(run.StopReason, "the run was stopped early")}
	case len(run.Thresholds) == 0:
		return Verdict{
			Passed: true,
			Label:  "Completed",
			Detail: "No thresholds were defined, so there is nothing to pass or fail.",
		}
	default:
		return Verdict{
			Passed: true,
			Label:  "Passed",
			Detail: fmt.Sprintf("The run completed and all %s held.", plural(len(run.Thresholds), "threshold", "thresholds")),
		}
	}
}

// headline builds the summary figures.
func headline(snapshot coordinator.Snapshot) []Stat {
	totals := snapshot.Totals
	elapsed := time.Duration(snapshot.Run.ElapsedSec * float64(time.Second))

	stats := []Stat{
		{Label: "Requests", Value: formatCount(totals[loadwave.MetricHTTPReqs].Count)},
	}
	if elapsed > 0 {
		stats = append(stats, Stat{
			Label: "Throughput",
			Value: formatCount(uint64(float64(totals[loadwave.MetricHTTPReqs].Count)/elapsed.Seconds())) + "/s",
		})
	}
	stats = append(stats,
		Stat{Label: "Iterations", Value: formatCount(totals[loadwave.MetricIterations].Count)},
		Stat{Label: "Peak VUs", Value: formatCount(uint64(snapshot.Run.PeakVUs))},
	)

	if duration, ok := totals[loadwave.MetricHTTPReqDuration]; ok && duration.Count > 0 {
		stats = append(stats,
			Stat{Label: "Avg", Value: formatMillis(duration.Avg)},
			Stat{Label: "p95", Value: formatMillis(duration.Percentiles["p95"])},
			Stat{Label: "p99", Value: formatMillis(duration.Percentiles["p99"])},
		)
	}
	if failed, ok := totals[loadwave.MetricHTTPReqFailed]; ok && failed.Count > 0 {
		stats = append(stats, Stat{
			Label:  "Error rate",
			Value:  formatPercent(failed.Rate),
			Detail: fmt.Sprintf("%s of %s", formatCount(ratioCount(failed)), formatCount(failed.Count)),
		})
	}
	if checks, ok := totals[loadwave.MetricChecks]; ok && checks.Count > 0 {
		stats = append(stats, Stat{
			Label:  "Checks",
			Value:  formatPercent(checks.Rate),
			Detail: fmt.Sprintf("%s of %s", formatCount(ratioCount(checks)), formatCount(checks.Count)),
		})
	}
	return stats
}

// charts builds the four time-series charts, and reports how many endpoints
// were left off the response-time chart.
func charts(snapshot coordinator.Snapshot) ([]Chart, int) {
	ticks := snapshot.Ticks
	if len(ticks) < 2 {
		return nil, 0
	}

	xs := make([]time.Time, len(ticks))
	vus := make([]float64, len(ticks))
	rps := make([]float64, len(ticks))
	for i, tick := range ticks {
		xs[i] = time.UnixMilli(tick.T)
		vus[i] = float64(tick.VUs)
		rps[i] = tick.RPS
	}

	out := []Chart{
		{
			Title:  "Virtual users",
			Hint:   "concurrent simulated clients",
			X:      xs,
			Format: func(v float64) string { return formatCount(uint64(v)) },
			Series: []Series{{Label: "VUs", Color: "#2a78d6", Points: vus, Fill: true}},
		},
		{
			Title:  "Requests per second",
			Hint:   "throughput as measured by the generators",
			X:      xs,
			Format: func(v float64) string { return formatCount(uint64(v)) },
			Series: []Series{{Label: "req/s", Color: "#1baf7a", Points: rps, Fill: true}},
		},
	}

	names, omitted := chartedEndpoints(ticks)
	if len(names) > 0 {
		series := make([]Series, 0, len(names))
		// Coloured by name within the charted set, exactly as the dashboard
		// does, so a report and the live view agree.
		byName := append([]string(nil), names...)
		sort.Strings(byName)
		slot := make(map[string]string, len(byName))
		for i, name := range byName {
			slot[name] = endpointColors[i%len(endpointColors)]
		}

		for _, name := range names {
			points := make([]float64, len(ticks))
			for i, tick := range ticks {
				if point, ok := tick.Endpoints[name]; ok {
					points[i] = point.Avg
				}
			}
			series = append(series, Series{Label: name, Color: slot[name], Points: points})
		}

		out = append(out, Chart{
			Title:  "Response time",
			Hint:   "average per endpoint",
			X:      xs,
			Format: formatMillis,
			Series: series,
		})
	}

	if status := statusSeries(ticks); len(status) > 0 {
		out = append(out, Chart{
			Title:   "Responses by status",
			Hint:    "stacked; transport failures counted separately",
			X:       xs,
			Format:  func(v float64) string { return formatCount(uint64(v)) },
			Stacked: true,
			Series:  status,
		})
	}

	return out, omitted
}

// chartedEndpoints picks the busiest endpoints, and counts the rest.
func chartedEndpoints(ticks []coordinator.TickDTO) ([]string, int) {
	requests := map[string]uint64{}
	for _, tick := range ticks {
		for name, point := range tick.Endpoints {
			requests[name] += point.Requests
		}
	}
	if len(requests) == 0 {
		return nil, 0
	}

	names := make([]string, 0, len(requests))
	for name := range requests {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if requests[names[i]] != requests[names[j]] {
			return requests[names[i]] > requests[names[j]]
		}
		return names[i] < names[j]
	})

	if len(names) <= maxChartedEndpoints {
		return names, 0
	}
	return names[:maxChartedEndpoints], len(names) - maxChartedEndpoints
}

// statusSeries builds the stacked status-class bands.
func statusSeries(ticks []coordinator.TickDTO) []Series {
	present := map[string]bool{}
	for _, tick := range ticks {
		for class := range tick.Status {
			present[class] = true
		}
	}

	out := make([]Series, 0, len(present))
	for _, class := range statusOrder {
		if !present[class] {
			continue
		}
		points := make([]float64, len(ticks))
		for i, tick := range ticks {
			points[i] = float64(tick.Status[class])
		}
		out = append(out, Series{Label: class, Color: statusColors[class], Points: points})
	}
	return out
}

// warnings surfaces anything that makes the figures incomplete.
//
// A report that quietly understates the results would be worse than no report,
// so this is stated at the top rather than buried.
func warnings(stats metrics.Stats) []string {
	var out []string

	if stats.DroppedByNode > 0 {
		out = append(out, formatCount(stats.DroppedByNode)+" samples were dropped by nodes that hit their series cap, so the figures below understate the run. A high-cardinality tag is the usual cause.")
	}
	if stats.DroppedSeries > 0 {
		out = append(out, formatCount(stats.DroppedSeries)+" series were dropped by the coordinator's cardinality limit.")
	}
	if stats.DroppedLate > 0 {
		out = append(out, formatCount(stats.DroppedLate)+" metric batches arrived too late to be counted. Check clock skew between hosts.")
	}
	if stats.DroppedEndpoints > 0 {
		out = append(out, formatCount(stats.DroppedEndpoints)+" endpoint observations were left out of the timeline, which hit its endpoint cap.")
	}
	if stats.DroppedFailures > 0 {
		out = append(out, formatCount(stats.DroppedFailures)+" further kinds of failure were not recorded; the counts shown are complete only for the kinds listed.")
	}
	return out
}

// failureCode renders the status column of the failures table.
func failureCode(failure metrics.FailureSummary) string {
	if failure.Status > 0 {
		return strconv.Itoa(int(failure.Status))
	}
	if failure.ErrorClass != "" {
		return strings.ReplaceAll(failure.ErrorClass, "_", " ")
	}
	return "no response"
}

func ratioCount(summary metrics.SeriesSummary) uint64 {
	return uint64(summary.Rate*float64(summary.Count) + 0.5)
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
