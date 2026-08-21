import { useEffect, useMemo, useRef, type ReactNode } from 'react';
import uPlot from 'uplot';

import type { ChartColors } from '../lib/theme';
import { cn } from '../lib/cn';
import { formatTime } from '../lib/format';
import { advanceEdge, approach } from '../lib/motion';

export interface ChartSeries {
  label: string;
  color: string;
  /** 0–1. Non-stacked series draw a translucent area below the line. */
  fillOpacity?: number;
  /**
   * Drawn faint, for a series outside the current highlight.
   *
   * Kept rather than dropped: a highlighted line means little without the
   * others still there to compare it against.
   */
  muted?: boolean;
}

export interface TimeChartProps {
  title: string;
  /** One line of context under the title. Not a legend. */
  hint?: string;
  /** Controls for the chart itself, rendered in its header. */
  controls?: ReactNode;
  /** Bucket start times, epoch milliseconds. */
  timestamps: number[];
  /** One array per series, in display order, aligned to `timestamps`. */
  values: number[][];
  series: ChartSeries[];
  /** Renders a y value for the axis and the tooltip. */
  format: (value: number) => string;
  /** Stacks the series into bands summing to the total. */
  stacked?: boolean;
  height?: number;
  /** Charts sharing a key share a crosshair. */
  syncKey?: string;
  colors: ChartColors;
  emptyMessage?: string;
  /** Makes the legend clickable. Called with the series label. */
  onSelectSeries?: ((label: string) => void) | undefined;
  /** Labels of the currently highlighted series. */
  selected?: readonly string[];
  /** Shown under the legend, e.g. when series were left off the chart. */
  footnote?: string | undefined;
  /**
   * Scrolls the time axis continuously instead of stepping when data lands.
   *
   * Data arrives once a second; without this the plot jumps a whole bucket at
   * a time, which reads as the page reloading rather than as a live chart.
   * The caller is responsible for honouring the viewer's motion preference.
   */
  live?: boolean;
  /** Visible span in milliseconds. Omit to fit whatever data there is. */
  spanMs?: number | undefined;
}

/** How fast the y ceiling eases toward its target, per second. */
const Y_EASE_RATE = 4;

/** Escapes text destined for the tooltip's innerHTML.
 *
 *  Series labels are endpoint names, which come from the system under test by
 *  way of a URL path. Nothing derived from a target's responses should reach
 *  innerHTML unescaped. */
function escapeHTML(text: string): string {
  return text
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');
}

/** Converts a hex color to rgba at the given alpha, for area fills. */
function withAlpha(hex: string, alpha: number): string {
  const parsed = hex.replace('#', '');
  if (parsed.length !== 6) return hex;
  const r = parseInt(parsed.slice(0, 2), 16);
  const g = parseInt(parsed.slice(2, 4), 16);
  const b = parseInt(parsed.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

/**
 * Accumulates series into stacked bands.
 *
 * Each output band is the running total up to and including that series, so
 * drawing them filled-to-zero from the topmost down produces the stack. uPlot
 * has no stacking of its own, and doing it here keeps the arithmetic in one
 * visible place rather than inside a draw hook.
 */
function accumulate(values: number[][]): number[][] {
  const bands: number[][] = [];
  let running: number[] | null = null;

  for (const row of values) {
    const next = row.map((value, i) => (running?.[i] ?? 0) + (Number.isFinite(value) ? value : 0));
    bands.push(next);
    running = next;
  }
  return bands;
}

/**
 * Builds the crosshair tooltip.
 *
 * `readRaw` is a getter rather than the array itself. uPlot plugins are
 * constructed once, with the plot, and every tick replaces the data arrays —
 * so a captured reference goes stale within a second and the hovered index
 * runs off its end, leaving a tooltip with a timestamp and no values.
 */
function tooltipPlugin(
  series: ChartSeries[],
  format: (value: number) => string,
  readRaw: () => number[][],
): uPlot.Plugin {
  let tip: HTMLDivElement | null = null;

  return {
    hooks: {
      init: (u) => {
        tip = document.createElement('div');
        tip.className = 'lw-tooltip';
        tip.style.display = 'none';
        u.over.appendChild(tip);
      },

      setCursor: (u) => {
        if (!tip) return;

        const { idx, left, top } = u.cursor;
        if (idx === null || idx === undefined || left === undefined || left < 0) {
          tip.style.display = 'none';
          return;
        }

        const timestamp = u.data[0]?.[idx];
        if (timestamp === undefined || timestamp === null) {
          tip.style.display = 'none';
          return;
        }

        // Values come from the pre-stack arrays: a reader wants each series'
        // own number, not the cumulative height of its band.
        const raw = readRaw();
        const rows = series
          .map((spec, i) => {
            const value = raw[i]?.[idx];
            const shown = value === undefined || !Number.isFinite(value) ? '—' : format(value);
            return `<div class="lw-tooltip-row">
                <span class="lw-swatch" style="background:${escapeHTML(spec.color)}"></span>
                <span class="lw-tooltip-label">${escapeHTML(spec.label)}</span>
                <span class="lw-tooltip-value">${escapeHTML(shown)}</span>
              </div>`;
          })
          .join('');

        tip.innerHTML =
          `<div class="lw-tooltip-time">${escapeHTML(formatTime(Number(timestamp) * 1000))}</div>` +
          rows;
        tip.style.display = 'block';

        // Flip to the other side of the cursor near the right edge, so the
        // tooltip never runs off the plot it belongs to.
        const width = tip.offsetWidth;
        const flip = left + width + 24 > u.over.clientWidth;
        tip.style.left = `${flip ? left - width - 12 : left + 12}px`;
        tip.style.top = `${Math.max(4, (top ?? 0) - 12)}px`;
      },

      destroy: () => {
        tip?.remove();
        tip = null;
      },
    },
  };
}

/**
 * A live time-series chart.
 *
 * uPlot is used rather than a declarative charting library because this is
 * redrawn every second for the length of a run, often four charts at once; a
 * virtual-DOM chart spends more time diffing than the data is worth.
 */
export function TimeChart({
  title,
  hint,
  controls,
  timestamps,
  values,
  series,
  format,
  stacked = false,
  height = 200,
  syncKey,
  colors,
  emptyMessage = 'Waiting for data…',
  onSelectSeries,
  selected = [],
  footnote,
  live = false,
  spanMs,
}: TimeChartProps) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const plotRef = useRef<uPlot | null>(null);

  // The animation loop reads these rather than closing over render values, so
  // it can keep running across renders without being torn down and restarted
  // sixty times a second.
  const frameState = useRef<{
    data: uPlot.AlignedData | null;
    raw: number[][];
    yMax: number;
    spanMs: number;
    /** Right edge of the visible window, in the same seconds uPlot uses. */
    edge: number;
  }>({ data: null, raw: [], yMax: 0, spanMs: 0, edge: 0 });
  // `live` already accounts for the viewer's motion preference; gating it
  // again here would make the setting silently ineffective.
  const animating = live;

  // uPlot wants seconds; the API speaks milliseconds.
  const data = useMemo<uPlot.AlignedData>(() => {
    const xs = timestamps.map((t) => t / 1000);
    const ys = stacked ? accumulate(values) : values;
    // Stacked bands are drawn largest-first so the smaller ones paint on top.
    const ordered = stacked ? [...ys].reverse() : ys;
    return [xs, ...ordered] as uPlot.AlignedData;
  }, [timestamps, values, stacked]);

  const hasData = timestamps.length > 1;

  // Recreated whenever anything structural changes — the series set, the
  // stacking mode, or the theme's colors. uPlot options are fixed at
  // construction, and rebuilding is cheap next to fighting that.
  const signature = useMemo(
    () =>
      JSON.stringify([
        series.map((s) => [s.label, s.color, s.fillOpacity, s.muted]),
        stacked,
        colors.surface,
        colors.grid,
        colors.axis,
        colors.textMuted,
        height,
        syncKey,
      ]),
    [series, stacked, colors, height, syncKey],
  );

  useEffect(() => {
    const container = containerRef.current;
    if (!container || !hasData) return;

    const drawOrder = stacked ? [...series].reverse() : series;

    const options: uPlot.Options = {
      width: container.clientWidth || 600,
      height,
      // The custom legend below the plot carries identity and current values.
      legend: { show: false },
      padding: [12, 12, 0, 0],
      cursor: {
        y: false,
        points: { show: false },
        drag: { x: false, y: false, setScale: false },
        ...(syncKey ? { sync: { key: syncKey } } : {}),
      },
      scales: {
        x: { time: true },
        // While animating, the loop below owns both scales; this range is the
        // static fallback for a finished run or a reduced-motion viewer.
        y: { range: (_u, _min, max) => [0, max === 0 ? 1 : max * 1.1] },
      },
      axes: [
        {
          stroke: colors.textMuted,
          grid: { stroke: colors.grid, width: 1 },
          ticks: { stroke: colors.grid, width: 1, size: 4 },
          font: '11px ui-sans-serif, system-ui, sans-serif',
          space: 70,
        },
        {
          stroke: colors.textMuted,
          grid: { stroke: colors.grid, width: 1 },
          ticks: { show: false },
          font: '11px ui-sans-serif, system-ui, sans-serif',
          size: 52,
          values: (_u, splits) => splits.map((v) => (v === 0 ? '0' : format(v))),
        },
      ],
      series: [
        {},
        ...drawOrder.map<uPlot.Series>((spec) => {
          // A muted line keeps its hue at low alpha rather than turning grey,
          // so it still reads as the same series once the highlight is
          // dropped — and stays distinguishable from its muted neighbours.
          const fill =
            stacked || spec.muted
              ? stacked
                ? spec.color
                : null
              : spec.fillOpacity
                ? withAlpha(spec.color, spec.fillOpacity)
                : null;

          return {
            label: spec.label,
            // A 2px surface-coloured edge is what separates adjacent stacked
            // bands; without it two similar fills merge into one shape.
            stroke: stacked
              ? colors.surface
              : spec.muted
                ? withAlpha(spec.color, 0.25)
                : spec.color,
            width: spec.muted ? 1 : 2,
            points: { show: false },
            ...(fill === null ? {} : { fill }),
          };
        }),
      ],
      plugins: [tooltipPlugin(series, format, () => frameState.current.raw)],
    };

    const plot = new uPlot(options, data, container);
    plotRef.current = plot;

    const observer = new ResizeObserver(([entry]) => {
      const width = entry?.contentRect.width;
      if (width && width > 0) plot.setSize({ width, height });
    });
    observer.observe(container);

    return () => {
      observer.disconnect();
      plot.destroy();
      plotRef.current = null;
    };
    // `data` and `values` deliberately drive the separate update effect below
    // rather than a full rebuild on every tick.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature, hasData]);

  // Cheap path: hand uPlot the new arrays without rebuilding the plot.
  //
  // `resetScales` is false while animating: uPlot would otherwise snap both
  // axes to the new data's extent, which is precisely the jump the animation
  // exists to remove. The loop below sets them instead.
  useEffect(() => {
    frameState.current.data = data;
    // Pre-stack values, which is what the tooltip reports: a reader wants an
    // endpoint's own latency, not the cumulative height of its band.
    frameState.current.raw = values;
    plotRef.current?.setData(data, !animating);
  }, [data, values, animating]);

  // Keep the loop's view of the visible span current without restarting it.
  useEffect(() => {
    frameState.current.spanMs = spanMs ?? 0;
  }, [spanMs]);

  // The animation loop.
  //
  // Two things are eased. The x window advances with the wall clock rather
  // than with data arrival, so the plot slides continuously instead of
  // stepping once a second. The y ceiling approaches its target rather than
  // snapping, so one slow request does not squash the whole chart in a single
  // frame. Both are what separate a live chart from a page that keeps
  // reloading.
  useEffect(() => {
    if (!animating || !hasData) return;

    let frame = 0;
    let last = performance.now();

    const step = (now: number) => {
      frame = requestAnimationFrame(step);

      const plot = plotRef.current;
      // A hidden tab still fires rAF in some browsers, and redrawing a chart
      // nobody is looking at is pure waste.
      if (!plot || document.hidden) {
        last = now;
        return;
      }

      const delta = Math.min((now - last) / 1000, 0.25);
      last = now;

      const state = frameState.current;
      if (!state.data) return;
      const xs = state.data[0] as number[];
      if (xs.length === 0) return;

      // The edge is locked to the newest datum rather than to a guessed
      // offset from now; see advanceEdge for why.
      state.edge = advanceEdge(state.edge, xs[xs.length - 1]!, delta);

      const end = state.edge;
      const span = state.spanMs > 0 ? state.spanMs / 1000 : Math.max(end - xs[0]!, 1);
      const start = end - span;

      // Target the tallest value actually inside the window, so scrolling
      // past a spike releases the ceiling again.
      let peak = 0;
      for (let s = 1; s < state.data.length; s += 1) {
        const ys = state.data[s] as (number | null)[];
        for (let i = 0; i < xs.length; i += 1) {
          const x = xs[i]!;
          if (x < start || x > end) continue;
          const y = ys[i];
          if (typeof y === 'number' && y > peak) peak = y;
        }
      }

      const target = peak === 0 ? 1 : peak * 1.1;
      state.yMax = approach(state.yMax || target, target, Y_EASE_RATE, delta);

      plot.batch(() => {
        plot.setScale('x', { min: start, max: end });
        plot.setScale('y', { min: 0, max: state.yMax });
      });
    };

    frame = requestAnimationFrame(step);
    return () => cancelAnimationFrame(frame);
  }, [animating, hasData]);

  return (
    <figure className="border-line bg-surface flex flex-col rounded-lg border">
      <figcaption className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1.5 px-4 pt-3 pb-1">
        <h3 className="text-ink text-sm font-semibold">{title}</h3>
        <div className="flex flex-wrap items-center justify-end gap-x-3 gap-y-1.5">
          {hint ? <p className="text-ink-3 text-xs">{hint}</p> : null}
          {controls}
        </div>
      </figcaption>

      {hasData ? (
        <div ref={containerRef} className="lw-plot w-full px-1" />
      ) : (
        <div
          className="text-ink-3 flex items-center justify-center px-4 text-sm"
          style={{ height }}
        >
          {emptyMessage}
        </div>
      )}

      {series.length > 1 || onSelectSeries ? (
        <ul className="flex flex-wrap gap-x-4 gap-y-1 px-4 pt-1 pb-3">
          {series.map((spec, i) => {
            const latest = values[i]?.at(-1);
            // The value printed beside every swatch is what relieves the
            // low-contrast slots: identity never rests on hue alone.
            const content = (
              <>
                <span
                  aria-hidden="true"
                  className="inline-block size-2.5 shrink-0 rounded-full"
                  style={{ background: spec.color }}
                />
                <span className="text-ink-2">{spec.label}</span>
                <span className="tnum text-ink font-medium">
                  {latest === undefined ? '—' : format(latest)}
                </span>
              </>
            );

            if (!onSelectSeries) {
              return (
                <li
                  key={spec.label}
                  className={cn('flex items-center gap-1.5 text-xs', spec.muted && 'opacity-45')}
                >
                  {content}
                </li>
              );
            }

            const highlighted = selected.includes(spec.label);
            return (
              <li key={spec.label}>
                <button
                  type="button"
                  onClick={() => onSelectSeries(spec.label)}
                  aria-pressed={highlighted}
                  title={
                    highlighted
                      ? `Stop highlighting ${spec.label}`
                      : `Highlight ${spec.label}, dimming the rest`
                  }
                  className={cn(
                    'hover:bg-surface-2 flex items-center gap-1.5 rounded px-1 py-0.5 text-xs',
                    spec.muted && 'opacity-45',
                  )}
                >
                  {content}
                </button>
              </li>
            );
          })}
        </ul>
      ) : (
        <div className="text-ink-3 px-4 pt-1 pb-3 text-xs">
          {values[0]?.length ? (
            <span className="tnum">
              now <span className="text-ink font-medium">{format(values[0].at(-1) ?? 0)}</span>
            </span>
          ) : null}
        </div>
      )}

      {footnote ? <p className="text-ink-3 px-4 pb-3 text-xs">{footnote}</p> : null}
    </figure>
  );
}
