// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The report is read long after the run, often by somebody who was not there,
// so these lean further toward being unambiguous than the terminal's do.

// formatMillis renders a millisecond figure with a readable unit.
func formatMillis(ms float64) string {
	switch {
	case ms <= 0:
		return "—"
	case ms < 1:
		return fmt.Sprintf("%.0fµs", ms*1000)
	case ms < 1000:
		if ms < 10 {
			return fmt.Sprintf("%.1fms", ms)
		}
		return fmt.Sprintf("%.0fms", ms)
	default:
		return fmt.Sprintf("%.2fs", ms/1000)
	}
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

// formatPercent renders a ratio as a percentage.
func formatPercent(ratio float64) string {
	switch {
	case ratio <= 0:
		return "0%"
	case ratio < 0.0001:
		return "<0.01%"
	default:
		return fmt.Sprintf("%.2f%%", ratio*100)
	}
}

// formatBytes renders a byte count in binary units.
func formatBytes(n float64) string {
	if n <= 0 {
		return "—"
	}
	units := [...]string{"B", "KiB", "MiB", "GiB", "TiB"}

	value, index := n, 0
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%.0f %s", value, units[index])
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}

// formatSeconds renders an elapsed duration in words.
func formatSeconds(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d <= 0:
		return "—"
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
