// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package report

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// Chart geometry, in the SVG's own coordinate space. The rendered element
// scales to its container, so these are proportions rather than pixels.
const (
	chartWidth   = 840
	chartHeight  = 240
	marginLeft   = 56
	marginRight  = 14
	marginTop    = 12
	marginBottom = 28

	plotWidth  = chartWidth - marginLeft - marginRight
	plotHeight = chartHeight - marginTop - marginBottom

	gridLines = 4
)

// maxPlotPoints caps how many points a series contributes to the SVG.
//
// An hour-long run holds 3,600 buckets; eight series of those would be nearly
// thirty thousand coordinate pairs in a file somebody is going to open in a
// browser. Averaging down to this keeps the file small and the line legible,
// and no detail visible at this width is lost.
const maxPlotPoints = 420

// Series is one line on a chart.
type Series struct {
	Label string
	Color string
	// Points are aligned with the chart's X values.
	Points []float64
	// Fill draws a translucent area beneath the line. Single-series charts
	// use it; a multi-series chart would be unreadable with eight of them.
	Fill bool
}

// Chart is a time series rendered as standalone SVG.
//
// Everything is inline — no script, no external stylesheet, no web font — so
// the report stays readable years later, offline, attached to a ticket.
type Chart struct {
	Title  string
	Hint   string
	X      []time.Time
	Series []Series
	// Format renders a Y value for the axis and the legend.
	Format func(float64) string
	// Stacked draws the series as cumulative bands rather than lines.
	Stacked bool
}

// Empty reports whether there is anything to draw.
func (c Chart) Empty() bool {
	if len(c.X) < 2 {
		return true
	}
	for _, s := range c.Series {
		for _, v := range s.Points {
			if v > 0 {
				return false
			}
		}
	}
	return false
}

// LegendEntry is one row of a chart's legend.
type LegendEntry struct {
	Label string
	Color string
	Value string
}

// Legend returns the series with their final values.
//
// The value beside each swatch is what keeps identity off colour alone, which
// matters more in a printed or forwarded report than on screen.
func (c Chart) Legend() []LegendEntry {
	out := make([]LegendEntry, 0, len(c.Series))
	for _, s := range c.Series {
		value := "—"
		if len(s.Points) > 0 {
			value = c.format(s.Points[len(s.Points)-1])
		}
		out = append(out, LegendEntry{Label: s.Label, Color: s.Color, Value: value})
	}
	return out
}

func (c Chart) format(v float64) string {
	if c.Format == nil {
		return fmt.Sprintf("%.0f", v)
	}
	return c.Format(v)
}

// SVG renders the chart.
func (c Chart) SVG() template.HTML {
	if len(c.X) < 2 || len(c.Series) == 0 {
		return ""
	}

	xs, series := c.downsample()
	plotted := series
	if c.Stacked {
		plotted = accumulate(series)
	}

	upper := c.upperBound(plotted)

	var b strings.Builder
	fmt.Fprintf(&b,
		`<svg class="chart" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="%s">`,
		chartWidth, chartHeight, template.HTMLEscapeString(c.Title))

	c.writeGrid(&b, upper)
	c.writeXLabels(&b, xs)

	// Stacked bands are drawn largest first so the smaller ones paint on top.
	order := make([]int, 0, len(plotted))
	if c.Stacked {
		for i := len(plotted) - 1; i >= 0; i-- {
			order = append(order, i)
		}
	} else {
		for i := range plotted {
			order = append(order, i)
		}
	}

	for _, i := range order {
		c.writeSeries(&b, plotted[i], upper)
	}

	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // every interpolation above is escaped or numeric.
}

// upperBound picks the Y ceiling, with headroom so the peak is not clipped.
func (c Chart) upperBound(series []Series) float64 {
	upper := 0.0
	for _, s := range series {
		for _, v := range s.Points {
			if v > upper {
				upper = v
			}
		}
	}
	if upper <= 0 {
		return 1
	}
	return upper * 1.1
}

// writeGrid draws the horizontal rules and their value labels.
func (c Chart) writeGrid(b *strings.Builder, upper float64) {
	for i := 0; i <= gridLines; i++ {
		fraction := float64(i) / gridLines
		y := marginTop + plotHeight*(1-fraction)
		value := upper * fraction

		fmt.Fprintf(b, `<line class="grid" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/>`,
			marginLeft, y, marginLeft+plotWidth, y)
		fmt.Fprintf(b, `<text class="ylabel" x="%d" y="%.1f">%s</text>`,
			marginLeft-8, y+3.5, template.HTMLEscapeString(c.format(value)))
	}
}

// writeXLabels draws a handful of time labels along the bottom.
func (c Chart) writeXLabels(b *strings.Builder, xs []time.Time) {
	const labels = 5
	for i := range labels {
		fraction := float64(i) / (labels - 1)
		index := int(fraction * float64(len(xs)-1))
		x := marginLeft + plotWidth*fraction

		anchor := "middle"
		switch i {
		case 0:
			anchor = "start"
		case labels - 1:
			anchor = "end"
		}

		fmt.Fprintf(b, `<text class="xlabel" x="%.1f" y="%d" text-anchor="%s">%s</text>`,
			x, chartHeight-8, anchor, xs[index].Format("15:04:05"))
	}
}

// writeSeries draws one line, and its area if it has one.
func (c Chart) writeSeries(b *strings.Builder, s Series, upper float64) {
	if len(s.Points) == 0 {
		return
	}

	coords := make([]string, 0, len(s.Points))
	for i, v := range s.Points {
		x := float64(marginLeft)
		if len(s.Points) > 1 {
			x += float64(plotWidth) * float64(i) / float64(len(s.Points)-1)
		}
		y := marginTop + plotHeight*(1-clamp(v/upper))
		coords = append(coords, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	line := strings.Join(coords, " ")

	if s.Fill || c.Stacked {
		// Close the path down to the baseline to make it an area.
		fmt.Fprintf(b,
			`<polygon fill="%s" fill-opacity="%s" points="%.1f,%d %s %.1f,%d"/>`,
			s.Color, fillOpacity(c.Stacked),
			float64(marginLeft), marginTop+plotHeight,
			line,
			float64(marginLeft+plotWidth), marginTop+plotHeight)
	}

	// Stacked bands are separated by a surface-coloured edge rather than
	// their own colour, so two similar fills do not merge into one shape.
	stroke := s.Color
	class := "line"
	if c.Stacked {
		stroke = "var(--surface)"
		class = "band-edge"
	}
	fmt.Fprintf(b, `<polyline class="%s" fill="none" stroke="%s" points="%s"/>`, class, stroke, line)
}

// fillOpacity is solid for a stacked band and translucent under a line.
func fillOpacity(stacked bool) string {
	if stacked {
		return "1"
	}
	return "0.14"
}

// clamp keeps a normalised value inside the plot.
func clamp(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// accumulate turns series into cumulative bands for stacking.
func accumulate(series []Series) []Series {
	out := make([]Series, len(series))
	var running []float64

	for i, s := range series {
		next := make([]float64, len(s.Points))
		for j, v := range s.Points {
			previous := 0.0
			if running != nil && j < len(running) {
				previous = running[j]
			}
			if math.IsNaN(v) {
				v = 0
			}
			next[j] = previous + v
		}
		out[i] = Series{Label: s.Label, Color: s.Color, Points: next}
		running = next
	}
	return out
}

// downsample averages the data down to at most maxPlotPoints per series.
//
// Averaging rather than sampling: taking every nth point would drop spikes
// entirely, and a latency spike is exactly what somebody opens this report to
// look at. The mean of each window keeps them visible, flattened only as far
// as the pixel width already would.
func (c Chart) downsample() ([]time.Time, []Series) {
	count := len(c.X)
	if count <= maxPlotPoints {
		return c.X, c.Series
	}

	buckets := maxPlotPoints
	xs := make([]time.Time, buckets)
	out := make([]Series, len(c.Series))
	for i, s := range c.Series {
		out[i] = Series{Label: s.Label, Color: s.Color, Fill: s.Fill, Points: make([]float64, buckets)}
	}

	for bucket := range buckets {
		start := bucket * count / buckets
		end := (bucket + 1) * count / buckets
		if end <= start {
			end = start + 1
		}
		if end > count {
			end = count
		}
		xs[bucket] = c.X[start]

		for i, s := range c.Series {
			sum, n := 0.0, 0
			for j := start; j < end && j < len(s.Points); j++ {
				sum += s.Points[j]
				n++
			}
			if n > 0 {
				out[i].Points[bucket] = sum / float64(n)
			}
		}
	}
	return xs, out
}
