import { useMemo, useState } from 'react';

import { fetchRunConfig, reportURL, scaleRun, shutdown, startRun, stopRun } from './api/client';
import { Metric, isTerminal, type RunPhase, type Tick } from './api/types';
import { useLiveState } from './hooks/useLiveState';
import {
  formatCompact,
  formatCount,
  formatElapsed,
  formatMillis,
  formatPercent,
  formatRate,
} from './lib/format';
import {
  scenariosByEndpoint,
  stepLines,
  sumScenarioAverages,
  withinPalette,
  type LatencyView,
} from './lib/latency';
import { useChartColors, useTheme } from './lib/theme';
import { useMotionPreference, type MotionChoice } from './lib/motion';
import { Agents, EndpointTable, EventLog, Failures, Thresholds } from './components/Panels';
import { StartRunDialog } from './components/StartRunDialog';
import { StatTile } from './components/StatTile';
import { TimeChart, type ChartSeries } from './components/TimeChart';
import { Badge, Button, Panel, Segmented, type Tone } from './components/ui';

/** Time windows offered above the charts. */
const RANGES: readonly { value: number; label: string }[] = [
  { value: 60, label: '1m' },
  { value: 300, label: '5m' },
  { value: 900, label: '15m' },
  { value: 0, label: 'All' },
];

/**
 * How the response-time chart rolls its averages up.
 *
 * Four heights over the same measurements. Total, Individual and Step are
 * means — how slow a request is, at whole-run, per-request and per-step-of-a-
 * scenario granularity. Scenario is a sum: a pass waits for each of its steps
 * in turn, so a scenario line is its own step lines added together, which is
 * the number to compare against a page budget and not one a mean can give.
 */
const LATENCY_VIEWS: readonly { value: LatencyView; label: string; title: string }[] = [
  { value: 'total', label: 'Total', title: 'The mean of every request' },
  { value: 'individual', label: 'Individual', title: 'One line per request, its own mean' },
  {
    value: 'step',
    label: 'Step',
    title: 'One line per step, grouped under the scenario that runs it',
  },
  {
    value: 'scenario',
    label: 'Scenario',
    title: "One line per scenario: its own steps' averages summed",
  },
];

/** One line of context per view, shown beside the chart's title. */
const LATENCY_HINTS: Record<LatencyView, string> = {
  total: 'mean across every request',
  individual: 'average per endpoint — click names to highlight them',
  step: 'average per step, grouped by scenario',
  scenario: "each scenario's own step averages summed",
};

/** How many endpoints get their own line before the rest are left off.
 *
 *  Bounded by the categorical palette, which has eight validated slots. A
 *  ninth hue would be indistinguishable from an existing one under colour
 *  vision deficiency, so the chart stops rather than inventing one.
 */
const MAX_CHARTED_ENDPOINTS = 8;

/** Steps and scenarios charted at once, bounded by the same eight palette
 *  slots, for the same reason. */
const MAX_CHARTED_STEPS = MAX_CHARTED_ENDPOINTS;
const MAX_CHARTED_SCENARIOS = MAX_CHARTED_ENDPOINTS;

/** Extra history kept beyond the selected window, so the continuously
 *  scrolling chart never runs past the end of its data. */
const WINDOW_SLACK_MS = 10_000;

/** Status classes in severity order, which is also the stacking order. */
const STATUS_ORDER = ['2xx', '3xx', '4xx', '5xx', 'error', '1xx', 'other'] as const;

/** Adds or removes one member, returning a new set for React to notice. */
function toggled(current: ReadonlySet<string>, name: string): ReadonlySet<string> {
  const next = new Set(current);
  if (!next.delete(name)) next.add(name);
  return next;
}

const PHASE_TONE: Record<RunPhase, Tone> = {
  pending: 'neutral',
  starting: 'accent',
  running: 'accent',
  stopping: 'warning',
  completed: 'good',
  failed: 'critical',
  aborted: 'warning',
};

export default function App() {
  const live = useLiveState();
  const { resolved, choice, setChoice } = useTheme();
  const colors = useChartColors(resolved);
  const motion = useMotionPreference();

  const [rangeSeconds, setRangeSeconds] = useState<number>(300);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [poweredOff, setPoweredOff] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);

  const { run, ticks, totals, agents, endpoints, failures, series, events, thresholds } = live;

  // Runs the same configuration again, with no detour through the editor —
  // for the common case of "that looked fine, do it again" or "fixed the
  // target, rerun it" without re-describing the test from scratch. Fetching
  // the run's own config rather than caching what was last submitted means
  // this also replays a test that arrived via `loadwave run file.yaml`,
  // which the dashboard never held a copy of to begin with.
  const quickStart = () => {
    if (!run) return;
    setRestarting(true);
    setRestartError(null);
    fetchRunConfig(run.id)
      .then((config) => startRun(config.yaml))
      .then(() => live.refresh())
      .catch((err: unknown) => setRestartError(err instanceof Error ? err.message : String(err)))
      .finally(() => setRestarting(false));
  };

  // How far up the hierarchy the response-time chart aggregates.
  const [latencyView, setLatencyView] = useState<LatencyView>('individual');

  // What the response-time chart is highlighting.
  //
  // A highlight rather than an isolation: the other lines stay on the chart,
  // dimmed. A request is only slow relative to its neighbours, and hiding them
  // takes away the comparison that makes the highlighted one worth looking at.
  // Several can be picked at once, which is what makes two of them comparable.
  const [highlightedRequests, setHighlightedRequests] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
  const [highlightedScenarios, setHighlightedScenarios] = useState<ReadonlySet<string>>(
    () => new Set(),
  );

  const highlightCount =
    latencyView === 'scenario' ? highlightedScenarios.size : highlightedRequests.size;

  const clearHighlight = () => {
    setHighlightedRequests(new Set());
    setHighlightedScenarios(new Set());
  };

  // Highlighting a request says nothing on a chart that does not draw one line
  // per request, so a click from the table below brings the view back to one
  // that does. Step already qualifies, and is left alone.
  const toggleRequest = (name: string) => {
    setLatencyView((view) => (view === 'step' ? view : 'individual'));
    setHighlightedRequests((current) => toggled(current, name));
  };

  const toggleScenario = (name: string) => {
    setHighlightedScenarios((current) => toggled(current, name));
  };

  const windowed = useMemo<Tick[]>(() => {
    if (rangeSeconds === 0 || ticks.length === 0) return ticks;
    // Measured back from the newest data point rather than from the wall
    // clock, so a finished run still shows its last minute instead of
    // scrolling itself blank.
    //
    // The slack keeps a little more data than the chart shows. While the
    // chart scrolls continuously its visible window runs slightly ahead of
    // the newest bucket, and without the margin the left edge would run off
    // the end of the data between ticks.
    const newest = ticks[ticks.length - 1]!.t;
    const cutoff = newest - rangeSeconds * 1000 - WINDOW_SLACK_MS;
    return ticks.filter((tick) => tick.t >= cutoff);
  }, [ticks, rangeSeconds]);

  const timestamps = useMemo(() => windowed.map((tick) => tick.t), [windowed]);
  const latest = windowed[windowed.length - 1];

  const vuSeries = useMemo(() => [windowed.map((t) => t.vus)], [windowed]);
  const rpsSeries = useMemo(() => [windowed.map((t) => t.rps)], [windowed]);
  // Every endpoint seen in the window, busiest first. The chart shows the
  // leading few; the Requests table below lists them all.
  const endpointNames = useMemo(() => {
    const requests = new Map<string, number>();
    for (const tick of windowed) {
      for (const [name, point] of Object.entries(tick.endpoints ?? {})) {
        requests.set(name, (requests.get(name) ?? 0) + point.requests);
      }
    }
    return [...requests.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .map(([name]) => name);
  }, [windowed]);

  // Which endpoints get a line, chosen by traffic. Never a ninth hue: past
  // the palette's eight validated slots, endpoints are left off the chart
  // rather than given a repeated colour. They stay one click away in the
  // table below.
  const chartedEndpoints = useMemo(
    () =>
      withinPalette(endpointNames, MAX_CHARTED_ENDPOINTS, (name) => highlightedRequests.has(name)),
    [endpointNames, highlightedRequests],
  );

  // Colour follows the endpoint, not its rank on the chart.
  //
  // Slots are assigned by name within the charted set, which gives two
  // properties that matter: the eight lines on screen always have eight
  // distinct hues, and isolating one endpoint does not repaint the others —
  // a reader who learned that checkout is orange keeps that.
  const endpointColor = useMemo(() => {
    const byName = [...chartedEndpoints].sort((a, b) => a.localeCompare(b));
    const map = new Map<string, string>();
    byName.forEach((name, i) => map.set(name, colors.categorical[i] ?? colors.textMuted));
    return map;
  }, [chartedEndpoints, colors]);

  const latencySeries = useMemo(
    () => chartedEndpoints.map((name) => windowed.map((t) => t.endpoints?.[name]?.avg ?? 0)),
    [windowed, chartedEndpoints],
  );

  // Which scenarios issue which request. Derived from the whole-run series
  // rather than from the ticks, which carry no scenario for an endpoint; see
  // scenariosByEndpoint.
  const endpointOwners = useMemo(() => scenariosByEndpoint(series), [series]);

  // Scenarios seen in the window, busiest first — the same ordering rule as
  // the endpoints, so the two views agree on what is prominent.
  const scenarioNames = useMemo(() => {
    const requests = new Map<string, number>();
    for (const tick of windowed) {
      for (const [name, point] of Object.entries(tick.scenarios ?? {})) {
        requests.set(name, (requests.get(name) ?? 0) + point.requests);
      }
    }
    return [...requests.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .map(([name]) => name);
  }, [windowed]);

  const chartedScenarios = useMemo(
    () =>
      withinPalette(scenarioNames, MAX_CHARTED_SCENARIOS, (name) => highlightedScenarios.has(name)),
    [scenarioNames, highlightedScenarios],
  );

  const totalLatencySeries = useMemo(() => [windowed.map((t) => t.avg)], [windowed]);

  // One line per step of a scenario. Same measurements as the per-request
  // view, said with the scenario each step belongs to — which is the level
  // the scenario sums below are built from.
  const allSteps = useMemo(
    () => stepLines(endpointNames, endpointOwners),
    [endpointNames, endpointOwners],
  );

  const chartedSteps = useMemo(
    () => withinPalette(allSteps, MAX_CHARTED_STEPS, (step) => highlightedRequests.has(step.name)),
    [allSteps, highlightedRequests],
  );

  const stepLatencySeries = useMemo(
    () => chartedSteps.map((step) => windowed.map((t) => t.endpoints?.[step.name]?.avg ?? 0)),
    [windowed, chartedSteps],
  );

  const scenarioLatencySeries = useMemo(
    () =>
      chartedScenarios.map((scenario) =>
        windowed.map((t) => sumScenarioAverages(t.endpoints, scenario, endpointOwners)),
      ),
    [windowed, chartedScenarios, endpointOwners],
  );

  // Only the status classes actually seen are charted, so a clean run does not
  // carry four empty bands and a legend full of zeroes.
  const statusKeys = useMemo(() => {
    const present = new Set<string>();
    for (const tick of windowed) {
      for (const key of Object.keys(tick.status ?? {})) present.add(key);
    }
    return STATUS_ORDER.filter((key) => present.has(key));
  }, [windowed]);

  const statusSeries = useMemo(
    () => statusKeys.map((key) => windowed.map((tick) => tick.status?.[key] ?? 0)),
    [windowed, statusKeys],
  );

  const latencySpecs = useMemo<ChartSeries[]>(
    () =>
      chartedEndpoints.map((name) => ({
        label: name,
        color: endpointColor.get(name) ?? colors.categorical[0] ?? colors.textMuted,
        muted: highlightedRequests.size > 0 && !highlightedRequests.has(name),
      })),
    [chartedEndpoints, endpointColor, colors, highlightedRequests],
  );

  // Colour follows the step's identity, not its rank, for the same reason it
  // follows the endpoint's.
  const stepSpecs = useMemo<ChartSeries[]>(() => {
    const byKey = [...chartedSteps].map((step) => step.key).sort((a, b) => a.localeCompare(b));
    return chartedSteps.map((step) => ({
      label: step.label,
      color: colors.categorical[byKey.indexOf(step.key)] ?? colors.textMuted,
      muted: highlightedRequests.size > 0 && !highlightedRequests.has(step.name),
    }));
  }, [chartedSteps, colors, highlightedRequests]);

  // Colour follows the scenario by name, for the same reason it follows the
  // endpoint: a hue a reader has learned should not move when the traffic
  // ranking does.
  const scenarioSpecs = useMemo<ChartSeries[]>(() => {
    const byName = [...chartedScenarios].sort((a, b) => a.localeCompare(b));
    return chartedScenarios.map((name) => ({
      label: name,
      color: colors.categorical[byName.indexOf(name)] ?? colors.textMuted,
      muted: highlightedScenarios.size > 0 && !highlightedScenarios.has(name),
    }));
  }, [chartedScenarios, colors, highlightedScenarios]);

  // The one-line Total view shares the hue the latency tiles use, so switching
  // views reads as one chart changing altitude rather than as four unrelated
  // charts.
  const totalLatencySpec = useMemo<ChartSeries[]>(
    () => [{ label: 'average', color: colors.latency[2], fillOpacity: 0.14 }],
    [colors],
  );

  // A step's legend entry reads "scenario · step"; the highlight is keyed on
  // the request underneath, which is what the table below names too.
  const toggleStep = (label: string) => {
    const step = chartedSteps.find((entry) => entry.label === label);
    if (step) toggleRequest(step.name);
  };

  const responseSelected = useMemo<string[]>(() => {
    if (latencyView === 'individual') return [...highlightedRequests];
    if (latencyView === 'step') {
      return chartedSteps.filter((step) => highlightedRequests.has(step.name)).map((s) => s.label);
    }
    if (latencyView === 'scenario') return [...highlightedScenarios];
    return [];
  }, [latencyView, highlightedRequests, highlightedScenarios, chartedSteps]);

  const responseValues =
    latencyView === 'total'
      ? totalLatencySeries
      : latencyView === 'step'
        ? stepLatencySeries
        : latencyView === 'scenario'
          ? scenarioLatencySeries
          : latencySeries;

  // What the reader needs told about the view they are on: which lines were
  // left off, and how to undo a highlight they no longer want.
  const latencyFootnote = useMemo<string | undefined>(() => {
    const notes: string[] = [];

    if (latencyView === 'individual' || latencyView === 'step') {
      const shown = latencyView === 'individual' ? chartedEndpoints.length : chartedSteps.length;
      const total = latencyView === 'individual' ? endpointNames.length : allSteps.length;
      const noun = latencyView === 'individual' ? 'endpoints' : 'steps';

      if (total > shown) {
        notes.push(
          `Showing the ${shown} busiest of ${total} ${noun}. Highlighting one from the ` +
            'table below always gives it a line.',
        );
      }
      if (
        latencyView === 'step' &&
        chartedSteps.some((step) => (endpointOwners.get(step.name)?.length ?? 0) > 1)
      ) {
        notes.push(
          'A step more than one scenario runs is charted under each of them; ' +
            'those lines carry the same measurements.',
        );
      }
    }

    if (latencyView === 'scenario') {
      if (scenarioNames.length > chartedScenarios.length) {
        notes.push(
          `Showing the ${chartedScenarios.length} busiest of ${scenarioNames.length} scenarios.`,
        );
      } else if (chartedScenarios.length === 0) {
        notes.push('No scenario has reported a request in this window.');
      }
    }

    if (highlightCount > 0) {
      notes.push(
        `Highlighting ${highlightCount}; the rest are dimmed, not hidden. ` +
          'Click a highlighted name again to drop it.',
      );
    }

    return notes.length > 0 ? notes.join(' ') : undefined;
  }, [
    latencyView,
    endpointNames,
    chartedEndpoints,
    endpointOwners,
    allSteps,
    chartedSteps,
    scenarioNames,
    chartedScenarios,
    highlightCount,
  ]);

  const responseSpecs =
    latencyView === 'total'
      ? totalLatencySpec
      : latencyView === 'step'
        ? stepSpecs
        : latencyView === 'scenario'
          ? scenarioSpecs
          : latencySpecs;

  const statusSpecs = useMemo<ChartSeries[]>(
    () => statusKeys.map((key) => ({ label: key, color: colors.status[key] ?? colors.textMuted })),
    [statusKeys, colors],
  );

  const active = run !== undefined && !isTerminal(run.phase);
  const requests = totals[Metric.httpReqs]?.count ?? 0;
  const iterations = totals[Metric.iterations]?.count ?? 0;
  const failureRate = totals[Metric.httpReqFailed]?.rate ?? 0;
  const checkRate = totals[Metric.checks]?.rate;
  const p95 = totals[Metric.httpReqDuration]?.percentiles?.p95 ?? 0;

  if (poweredOff) return <PoweredOff />;

  return (
    <div className="mx-auto flex min-h-full max-w-[110rem] flex-col gap-4 p-4 lg:p-6">
      <Header
        live={live}
        active={active}
        themeChoice={choice}
        onThemeChange={setChoice}
        onEditScript={() => setDialogOpen(true)}
        onQuickStart={quickStart}
        canQuickStart={run !== undefined}
        quickStarting={restarting}
        onPowerOff={() => {
          void shutdown().finally(() => setPoweredOff(true));
        }}
      />

      {live.error ? (
        <p
          role="alert"
          className="border-critical/50 text-critical rounded-lg border px-4 py-3 text-sm"
        >
          {live.error}
        </p>
      ) : null}

      {restartError ? (
        <p
          role="alert"
          className="border-critical/50 text-critical rounded-lg border px-4 py-3 text-sm"
        >
          Could not start: {restartError}
        </p>
      ) : null}

      {run ? (
        <>
          <section
            aria-label="Headline metrics"
            className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6"
          >
            <StatTile
              label="Virtual users"
              value={latest?.vus ?? 0}
              format={formatCount}
              detail={`peak target ${formatCount(run.peakVUs)}`}
              accent={colors.vus}
              animate={motion.smooth}
            />
            <StatTile
              label="Requests / sec"
              value={latest?.rps ?? 0}
              format={formatRate}
              detail={`${formatCompact(requests)} total`}
              accent={colors.rps}
              animate={motion.smooth}
            />
            <StatTile
              label="p95 latency"
              value={p95}
              format={formatMillis}
              detail="whole run"
              animate={motion.smooth}
            />
            <StatTile
              label="Error rate"
              value={failureRate}
              format={(v) => formatPercent(v)}
              detail={`${formatCompact(totals[Metric.httpReqFailed]?.count ?? 0)} requests judged`}
              tone={failureRate > 0 ? 'critical' : 'good'}
              animate={motion.smooth}
            />
            <StatTile
              label="Iterations"
              value={iterations}
              format={formatCompact}
              detail="completed"
              animate={motion.smooth}
            />
            <StatTile
              label="Checks"
              value={checkRate ?? null}
              format={(v) => formatPercent(v, 1)}
              detail={checkRate === undefined ? 'no checks defined' : 'passing'}
              tone={checkRate !== undefined && checkRate < 1 ? 'warning' : 'default'}
              animate={motion.smooth}
            />
          </section>

          <div className="flex flex-wrap items-center gap-2">
            <span className="text-ink-3 text-xs font-medium">Window</span>
            <Segmented
              label="Time window"
              options={RANGES}
              value={rangeSeconds}
              onChange={setRangeSeconds}
            />
            <span className="tnum text-ink-3 text-xs">
              {windowed.length} of {ticks.length} points · {live.resolutionSeconds}s resolution
            </span>

            <label className="ml-auto flex items-center gap-1.5">
              <span className="text-ink-3 text-xs font-medium">Motion</span>
              <select
                value={motion.choice}
                onChange={(event) => motion.setChoice(event.target.value as MotionChoice)}
                title={
                  motion.choice === 'system'
                    ? motion.systemReduced
                      ? 'Your system asks for reduced motion, so charts step instead of scrolling. Choose Smooth to override.'
                      : 'Following your system setting, which allows motion.'
                    : 'Overriding your system setting.'
                }
                className="border-line bg-surface text-ink rounded-md border px-1.5 py-0.5 text-xs"
              >
                <option value="system">
                  System{motion.systemReduced ? ' (stepped)' : ' (smooth)'}
                </option>
                <option value="smooth">Smooth</option>
                <option value="stepped">Stepped</option>
              </select>
            </label>
          </div>

          <section aria-label="Charts" className="grid gap-3 xl:grid-cols-2">
            <TimeChart
              title="Virtual users"
              hint="concurrent simulated clients"
              timestamps={timestamps}
              values={vuSeries}
              series={[{ label: 'VUs', color: colors.vus, fillOpacity: 0.14 }]}
              format={(v) => formatCount(v)}
              colors={colors}
              syncKey="loadwave"
              live={active && motion.smooth}
              spanMs={rangeSeconds * 1000}
            />
            <TimeChart
              title="Requests per second"
              hint="throughput as measured by the generators"
              timestamps={timestamps}
              values={rpsSeries}
              series={[{ label: 'req/s', color: colors.rps, fillOpacity: 0.14 }]}
              format={(v) => formatRate(v)}
              colors={colors}
              syncKey="loadwave"
              live={active && motion.smooth}
              spanMs={rangeSeconds * 1000}
            />
            <TimeChart
              title="Response time"
              hint={LATENCY_HINTS[latencyView]}
              controls={
                <div className="flex items-center gap-2">
                  {highlightCount > 0 ? (
                    <button
                      type="button"
                      onClick={clearHighlight}
                      className="text-ink-3 hover:text-ink text-xs underline underline-offset-2"
                    >
                      Clear highlight
                    </button>
                  ) : null}
                  <Segmented
                    label="Response time view"
                    options={LATENCY_VIEWS}
                    value={latencyView}
                    onChange={setLatencyView}
                  />
                </div>
              }
              timestamps={timestamps}
              values={responseValues}
              series={responseSpecs}
              format={(v) => formatMillis(v)}
              colors={colors}
              syncKey="loadwave"
              live={active && motion.smooth}
              spanMs={rangeSeconds * 1000}
              onSelectSeries={
                latencyView === 'individual'
                  ? toggleRequest
                  : latencyView === 'step'
                    ? toggleStep
                    : latencyView === 'scenario'
                      ? toggleScenario
                      : undefined
              }
              selected={responseSelected}
              emptyMessage="No requests recorded yet."
              footnote={latencyFootnote}
            />
            <TimeChart
              title="Responses by status"
              hint="stacked; transport failures counted separately"
              timestamps={timestamps}
              values={statusSeries}
              series={statusSpecs}
              format={(v) => formatCount(v)}
              stacked
              colors={colors}
              syncKey="loadwave"
              live={active && motion.smooth}
              spanMs={rangeSeconds * 1000}
              emptyMessage="No responses recorded yet."
            />
          </section>

          <div className="grid gap-3 xl:grid-cols-2">
            <Thresholds results={thresholds.length > 0 ? thresholds : run.thresholds} />
            <Agents agents={agents} />
          </div>

          <EndpointTable
            endpoints={endpoints}
            selected={highlightedRequests}
            onSelect={toggleRequest}
          />

          <Failures failures={failures} stats={run.stats} />

          <div className="grid gap-3 xl:grid-cols-[2fr_1fr]">
            <EventLog events={events} />
            <RunDetails
              run={run}
              onScale={(vus, rampSeconds) => void scaleRun(run.id, vus, rampSeconds)}
              active={active}
            />
          </div>
        </>
      ) : (
        <Panel title="No run yet">
          <div className="px-4 py-10 text-center">
            <p className="text-ink-2 text-sm">
              {agents.length === 0
                ? 'No agents are connected. Start one, then launch a run.'
                : `${agents.length} ${agents.length === 1 ? 'agent' : 'agents'} ready.`}
            </p>
            <div className="mt-4">
              <Button
                variant="primary"
                onClick={() => setDialogOpen(true)}
                disabled={agents.length === 0}
              >
                Edit script
              </Button>
            </div>
          </div>
        </Panel>
      )}

      <StartRunDialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onStarted={() => live.refresh()}
        runId={run?.id}
      />
    </div>
  );
}

function Header({
  live,
  active,
  themeChoice,
  onThemeChange,
  onEditScript,
  onQuickStart,
  canQuickStart,
  quickStarting,
  onPowerOff,
}: {
  live: ReturnType<typeof useLiveState>;
  active: boolean;
  themeChoice: 'light' | 'dark' | 'system';
  onThemeChange: (next: 'light' | 'dark' | 'system') => void;
  /** Opens the scenario editor — the "New run" dialog, kept for building a
   *  test from scratch or changing one before running it. */
  onEditScript: () => void;
  /** Runs the displayed run's own configuration again, with no editor in the
   *  way — the button most clicks actually want. */
  onQuickStart: () => void;
  /** Whether there is a configuration to replay at all. */
  canQuickStart: boolean;
  quickStarting: boolean;
  onPowerOff: () => void;
}) {
  const { run, status, build, agents, canShutDown } = live;

  // Two-step rather than a dialog. Powering off ends the process, and it sits
  // next to Stop run — which deliberately does not — so the two must not be
  // one misplaced click apart.
  const [confirmOff, setConfirmOff] = useState(false);

  return (
    <header className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-3">
        <svg viewBox="0 0 32 32" className="size-7" role="img" aria-label="LoadWave">
          <path
            d="M2 20c4 0 4-8 8-8s4 8 8 8 4-8 8-8 4 8 4 8"
            fill="none"
            stroke="var(--accent)"
            strokeWidth="3"
            strokeLinecap="round"
          />
        </svg>
        <div>
          <h1 className="text-base leading-tight font-semibold">LoadWave</h1>
          <p className="text-ink-3 text-xs">
            {build ? `${build.version} · ${build.platform}` : 'connecting…'}
          </p>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        {run ? (
          <>
            <span className="text-ink text-sm font-medium">{run.name}</span>
            <Badge tone={PHASE_TONE[run.phase]} dot>
              {run.phase}
            </Badge>
            <span className="tnum text-ink-2 text-sm">{formatElapsed(run.elapsedSeconds)}</span>
          </>
        ) : null}

        <Badge
          tone={status === 'open' ? 'good' : status === 'connecting' ? 'neutral' : 'critical'}
          dot
        >
          {status === 'open' ? 'live' : status}
        </Badge>

        <span className="tnum text-ink-3 text-xs">
          {agents.length} {agents.length === 1 ? 'agent' : 'agents'}
        </span>

        <select
          value={themeChoice}
          onChange={(event) => onThemeChange(event.target.value as 'light' | 'dark' | 'system')}
          aria-label="Colour theme"
          className="border-line bg-surface text-ink rounded-md border px-2 py-1.5 text-sm"
        >
          <option value="system">System</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>

        {run ? (
          // A plain anchor, so the browser downloads it without any of this
          // page's state being involved. Offered during a run too — a report
          // of the run so far is often exactly what somebody wants to send.
          <a
            href={reportURL(run.id)}
            download
            className="border-line-strong bg-surface text-ink hover:bg-surface-2 inline-flex items-center gap-1.5 rounded-md border px-3 py-1.5 text-sm font-medium"
            title="Download a self-contained HTML report, charts included"
          >
            Download report
          </a>
        ) : null}

        {active && run ? (
          <Button
            variant="danger"
            onClick={() => void stopRun(run.id, true)}
            title="End this run. LoadWave keeps running, so the results stay here."
          >
            Stop run
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onEditScript} disabled={agents.length === 0}>
              Edit script
            </Button>
            <Button
              variant="primary"
              onClick={onQuickStart}
              disabled={agents.length === 0 || !canQuickStart || quickStarting}
              title={
                canQuickStart
                  ? 'Run this configuration again, unchanged'
                  : 'Nothing has run yet — start with Edit script'
              }
            >
              {quickStarting ? 'Starting…' : 'Start'}
            </Button>
          </>
        )}

        {canShutDown ? (
          confirmOff ? (
            <span className="flex items-center gap-1.5">
              <Button variant="danger" onClick={onPowerOff}>
                Confirm shutdown
              </Button>
              <Button variant="ghost" onClick={() => setConfirmOff(false)}>
                Cancel
              </Button>
            </span>
          ) : (
            <Button
              variant="ghost"
              onClick={() => setConfirmOff(true)}
              title="Stop LoadWave itself. Any running test ends and the process exits."
            >
              Power off
            </Button>
          )
        ) : null}
      </div>
    </header>
  );
}

/** Shown once the process has been asked to exit. */
function PoweredOff() {
  return (
    <div className="mx-auto flex min-h-full max-w-md flex-col items-center justify-center gap-3 p-8 text-center">
      <svg viewBox="0 0 32 32" className="size-9" role="img" aria-label="LoadWave">
        <path
          d="M2 20c4 0 4-8 8-8s4 8 8 8 4-8 8-8 4 8 4 8"
          fill="none"
          stroke="var(--text-muted)"
          strokeWidth="3"
          strokeLinecap="round"
        />
      </svg>
      <h1 className="text-lg font-semibold">LoadWave has shut down</h1>
      <p className="text-ink-2 text-sm">
        The process has exited. Any report you already downloaded is self-contained and still works
        — nothing here was needed to render it.
      </p>
      <p className="text-ink-3 text-sm">Start it again from your terminal.</p>
    </div>
  );
}

function RunDetails({
  run,
  active,
  onScale,
}: {
  run: NonNullable<ReturnType<typeof useLiveState>['run']>;
  active: boolean;
  onScale: (vus: number, rampSeconds: number) => void;
}) {
  const [target, setTarget] = useState(String(run.peakVUs));
  // Defaults to a gradual change rather than an instant one. Introducing a
  // few hundred virtual users in a single tick measures how the service
  // survives a thundering herd, which is rarely the question being asked.
  const [ramp, setRamp] = useState('30');

  return (
    <Panel title="Run">
      <dl className="divide-line divide-y text-sm">
        {[
          ['ID', run.id],
          ['Profile', run.profile],
          ['Target', run.baseURL || '—'],
          ['Agents', `${run.participants.length}`],
          ['Series', `${run.stats.series}`],
          run.stopReason ? ['Stopped', run.stopReason] : null,
          run.failure ? ['Failure', run.failure] : null,
        ]
          .filter((entry): entry is [string, string] => entry !== null)
          .map(([label, value]) => (
            <div key={label} className="flex gap-3 px-4 py-2">
              <dt className="text-ink-3 w-24 shrink-0">{label}</dt>
              <dd className="text-ink-2 min-w-0 flex-1 break-words">{value}</dd>
            </div>
          ))}
      </dl>

      {active ? (
        <div className="border-line border-t px-4 py-3">
          <div className="flex items-end gap-2">
            <label className="flex-1">
              <span className="text-ink-3 text-xs">Peak virtual users</span>
              <input
                type="number"
                min={0}
                value={target}
                onChange={(event) => setTarget(event.target.value)}
                className="tnum border-line bg-page text-ink mt-1 w-full rounded-md border px-2 py-1.5 text-sm"
              />
            </label>
            <label className="w-28">
              <span className="text-ink-3 text-xs">Ramp over</span>
              <div className="mt-1 flex items-center gap-1">
                <input
                  type="number"
                  min={0}
                  step={5}
                  value={ramp}
                  onChange={(event) => setRamp(event.target.value)}
                  className="tnum border-line bg-page text-ink w-full rounded-md border px-2 py-1.5 text-sm"
                />
                <span className="text-ink-3 text-xs">s</span>
              </div>
            </label>
            <Button
              onClick={() => {
                const vus = Number.parseInt(target, 10);
                const seconds = Number.parseFloat(ramp);
                if (!Number.isFinite(vus) || vus < 0) return;
                onScale(vus, Number.isFinite(seconds) && seconds > 0 ? seconds : 0);
              }}
            >
              Apply
            </Button>
          </div>
          <p className="text-ink-3 mt-1.5 text-xs">
            {Number.parseFloat(ramp) > 0
              ? `New users are introduced evenly over ${ramp}s rather than all at once.`
              : 'Applied immediately — every new virtual user starts in the same tick.'}
          </p>
        </div>
      ) : null}

      {run.stats.droppedByNode > 0 || run.stats.droppedLate > 0 ? (
        <p className="border-line text-warning border-t px-4 py-2 text-xs">
          Some samples were dropped, so these figures understate reality:{' '}
          {formatCount(run.stats.droppedByNode)} by nodes at their series cap,{' '}
          {formatCount(run.stats.droppedLate)} batches too late to count.
        </p>
      ) : null}
    </Panel>
  );
}
