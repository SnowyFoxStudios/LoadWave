// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SnowyFoxStudios/LoadWave/internal/coordinator"
	"github.com/SnowyFoxStudios/LoadWave/internal/metrics"
	"github.com/SnowyFoxStudios/LoadWave/pkg/loadwave"
)

// reporter renders run progress and the final report to a terminal.
type reporter struct {
	out   io.Writer
	tty   bool
	quiet bool

	lastLine int
	started  time.Time

	// iterations accumulates across ticks. Each tick reports only its own
	// bucket, but a progress line showing "37 iterations" a minute into a run
	// reads as though nothing is happening.
	iterations uint64
}

// newReporter builds a reporter for the given stream.
func newReporter(out io.Writer, quiet bool) *reporter {
	return &reporter{out: out, tty: isTerminal(out), quiet: quiet, started: time.Now()}
}

// isTerminal reports whether w is an interactive terminal.
//
// Only an interactive terminal gets the in-place progress line; anything else
// — a pipe, a CI log, a file — gets plain appended lines, because carriage
// returns in a build log are worse than useless.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// progress renders one live status line.
func (r *reporter) progress(tick coordinator.TickDTO, run *coordinator.Summary) {
	if r.quiet {
		return
	}

	elapsed := time.Duration(0)
	if run != nil && run.ElapsedSec > 0 {
		elapsed = time.Duration(run.ElapsedSec * float64(time.Second))
	}

	r.iterations += tick.Iterations

	line := fmt.Sprintf(
		"%s  vus %-6d  rps %-9s  p95 %-9s  err %-7s  iters %s",
		formatClock(elapsed),
		tick.VUs,
		formatRate(tick.RPS),
		formatMillis(tick.P95),
		formatPercent(tick.ErrorRate),
		formatCount(r.iterations))

	if !r.tty {
		fmt.Fprintln(r.out, line)
		return
	}

	// Pad to the previous length so a shorter line does not leave characters
	// behind from the one before it.
	if pad := r.lastLine - len(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	r.lastLine = len(strings.TrimRight(line, " "))
	fmt.Fprintf(r.out, "\r%s", line)
}

// endProgress closes off the live line so the report starts cleanly.
func (r *reporter) endProgress() {
	if r.tty && !r.quiet && r.lastLine > 0 {
		fmt.Fprintln(r.out)
	}
}

// final prints the complete run report.
func (r *reporter) final(snapshot coordinator.Snapshot) {
	r.endProgress()

	run := snapshot.Run
	if run == nil {
		fmt.Fprintln(r.out, "\nNo run to report.")
		return
	}

	fmt.Fprintf(r.out, "\n%s\n", strings.Repeat("─", 78))
	fmt.Fprintf(r.out, "  %s — %s\n", run.Name, run.Phase)
	fmt.Fprintf(r.out, "  %s\n", run.Profile)
	if run.BaseURL != "" {
		fmt.Fprintf(r.out, "  target %s\n", run.BaseURL)
	}
	fmt.Fprintf(r.out, "  ran for %s across %d agent(s)\n",
		time.Duration(run.ElapsedSec*float64(time.Second)).Round(time.Second),
		len(run.Participants))
	if run.StopReason != "" {
		fmt.Fprintf(r.out, "  stopped: %s\n", run.StopReason)
	}
	fmt.Fprintf(r.out, "%s\n\n", strings.Repeat("─", 78))

	r.printTotals(snapshot)
	r.printEndpoints(snapshot)
	r.printThresholds(run.Thresholds)
	r.printWarnings(run.Stats)
}

// printTotals renders the headline metrics.
func (r *reporter) printTotals(snapshot coordinator.Snapshot) {
	totals := snapshot.Totals
	if totals == nil {
		totals = map[string]metrics.SeriesSummary{}
	}

	elapsed := time.Duration(0)
	if snapshot.Run != nil {
		elapsed = time.Duration(snapshot.Run.ElapsedSec * float64(time.Second))
	}

	writer := tabwriter.NewWriter(r.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "  METRIC\tVALUE")

	requests := totals[loadwave.MetricHTTPReqs]
	fmt.Fprintf(writer, "  requests\t%s\n", formatCount(requests.Count))
	if elapsed > 0 {
		fmt.Fprintf(writer, "  throughput\t%s\n",
			formatRate(float64(requests.Count)/elapsed.Seconds()))
	}
	fmt.Fprintf(writer, "  iterations\t%s\n", formatCount(totals[loadwave.MetricIterations].Count))

	if duration, ok := totals[loadwave.MetricHTTPReqDuration]; ok && duration.Count > 0 {
		fmt.Fprintf(writer, "  duration avg\t%s\n", formatMillis(duration.Avg))
		for _, key := range metrics.PercentileKeys {
			fmt.Fprintf(writer, "  duration %s\t%s\n", key, formatMillis(duration.Percentiles[key]))
		}
		fmt.Fprintf(writer, "  duration max\t%s\n", formatMillis(duration.Max))
	}
	if waiting, ok := totals[loadwave.MetricHTTPReqWaiting]; ok && waiting.Count > 0 {
		fmt.Fprintf(writer, "  waiting p95\t%s\n", formatMillis(waiting.Percentiles["p95"]))
	}

	if failed, ok := totals[loadwave.MetricHTTPReqFailed]; ok && failed.Count > 0 {
		fmt.Fprintf(writer, "  failed requests\t%s (%s)\n",
			formatCount(ratioCount(failed)), formatPercent(failed.Rate))
	}
	if checks, ok := totals[loadwave.MetricChecks]; ok && checks.Count > 0 {
		fmt.Fprintf(writer, "  checks passed\t%s of %s\n",
			formatCount(ratioCount(checks)), formatCount(checks.Count))
	}
	if iterFailed, ok := totals[loadwave.MetricIterationFailed]; ok && iterFailed.Count > 0 && iterFailed.Rate > 0 {
		fmt.Fprintf(writer, "  failed iterations\t%s (%s)\n",
			formatCount(ratioCount(iterFailed)), formatPercent(iterFailed.Rate))
	}
	if bytesIn := totals[loadwave.MetricHTTPReqBytesIn]; bytesIn.Sum > 0 {
		fmt.Fprintf(writer, "  received\t%s\n", formatBytes(bytesIn.Sum))
	}
	if bytesOut := totals[loadwave.MetricHTTPReqBytesOut]; bytesOut.Sum > 0 {
		fmt.Fprintf(writer, "  sent\t%s\n", formatBytes(bytesOut.Sum))
	}

	_ = writer.Flush()
	fmt.Fprintln(r.out)
}

// ratioCount recovers the numerator of a rate metric.
//
// Rates travel as a fraction plus a denominator rather than as two counters,
// so the count of truthy observations has to be reconstituted. The rounding
// keeps floating point error from turning 1002 into 1001.
func ratioCount(summary metrics.SeriesSummary) uint64 {
	return uint64(summary.Rate*float64(summary.Count) + 0.5)
}

// printEndpoints renders the per-request-name breakdown.
func (r *reporter) printEndpoints(snapshot coordinator.Snapshot) {
	if len(snapshot.Endpoints) == 0 {
		return
	}

	writer := tabwriter.NewWriter(r.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "  REQUEST\tCOUNT\tAVG\tP95\tP99\tMAX\tERRORS")

	for _, endpoint := range snapshot.Endpoints {
		fmt.Fprintf(writer, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			truncate(endpoint.Name, 44),
			formatCount(endpoint.Requests),
			formatMillis(endpoint.Avg),
			formatMillis(endpoint.Percentiles["p95"]),
			formatMillis(endpoint.Percentiles["p99"]),
			formatMillis(endpoint.Max),
			formatPercent(endpoint.ErrorRate))
	}
	_ = writer.Flush()
	fmt.Fprintln(r.out)
}

// printThresholds renders the pass/fail verdicts.
func (r *reporter) printThresholds(results []coordinator.ThresholdResult) {
	if len(results) == 0 {
		return
	}

	writer := tabwriter.NewWriter(r.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "  THRESHOLD\tACTUAL\tRESULT")

	for _, result := range results {
		switch {
		case !result.Evaluated:
			fmt.Fprintf(writer, "  %s\t—\tnot measured\n", result.Description)
		case result.Passed:
			fmt.Fprintf(writer, "  %s\t%s\tpass\n", result.Description, formatNumber(result.Actual))
		default:
			fmt.Fprintf(writer, "  %s\t%s\tFAIL\n", result.Description, formatNumber(result.Actual))
		}
	}
	_ = writer.Flush()
	fmt.Fprintln(r.out)
}

// printWarnings surfaces anything that makes the numbers above incomplete.
//
// Silently reporting understated figures would be the worst possible failure
// mode for a measurement tool, so dropped samples get their own callout rather
// than a log line nobody reads.
func (r *reporter) printWarnings(stats metrics.Stats) {
	var warnings []string

	if stats.DroppedByNode > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%s samples were dropped by nodes that hit their series cap; "+
				"a high-cardinality tag is the usual cause",
			formatCount(stats.DroppedByNode)))
	}
	if stats.DroppedSeries > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d series were dropped by the coordinator's cardinality limit", stats.DroppedSeries))
	}
	if stats.DroppedLate > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d metric batches arrived too late to be counted; check clock skew between hosts",
			stats.DroppedLate))
	}

	for _, warning := range warnings {
		fmt.Fprintf(r.out, "  warning: %s\n", warning)
	}
	if len(warnings) > 0 {
		fmt.Fprintln(r.out)
	}
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// formatClock renders an elapsed duration as mm:ss.
func formatClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// formatMillis renders a millisecond figure with a sensible unit.
func formatMillis(ms float64) string {
	switch {
	case ms <= 0:
		return "—"
	case ms < 1:
		return fmt.Sprintf("%.0fµs", ms*1000)
	case ms < 1000:
		return fmt.Sprintf("%.1fms", ms)
	default:
		return fmt.Sprintf("%.2fs", ms/1000)
	}
}

// formatRate renders requests per second.
func formatRate(rps float64) string {
	switch {
	case rps <= 0:
		return "0/s"
	case rps < 10:
		return fmt.Sprintf("%.1f/s", rps)
	case rps < 1000:
		return fmt.Sprintf("%.0f/s", rps)
	default:
		return fmt.Sprintf("%.1fk/s", rps/1000)
	}
}

// formatPercent renders a ratio as a percentage.
func formatPercent(ratio float64) string {
	if ratio <= 0 {
		return "0%"
	}
	if ratio < 0.001 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.2f%%", ratio*100)
}

// formatCount renders a whole number with thousands separators.
func formatCount(n uint64) string {
	text := strconv.FormatUint(n, 10)
	if len(text) <= 3 {
		return text
	}

	var b strings.Builder
	lead := len(text) % 3
	if lead > 0 {
		b.WriteString(text[:lead])
	}
	for i := lead; i < len(text); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(text[i : i+3])
	}
	return b.String()
}

// formatNumber renders a threshold's observed value compactly.
func formatNumber(v float64) string {
	switch {
	case v == float64(int64(v)):
		return strconv.FormatInt(int64(v), 10)
	case v < 1:
		return fmt.Sprintf("%.4f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

// formatBytes renders a byte count in binary units.
func formatBytes(n float64) string {
	const unit = 1024.0
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}

	value, index := n, 0
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", value, units[index])
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

// truncate shortens a string with an ellipsis.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 1 {
		return s[:limit]
	}
	return s[:limit-1] + "…"
}
