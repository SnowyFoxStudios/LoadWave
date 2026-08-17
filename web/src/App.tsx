import { useMemo, useState } from 'react';

import { reportURL, scaleRun, shutdown, stopRun } from './api/client';
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
import { useChartColors, useTheme } from './lib/theme';
import { useMotionPreference, type MotionChoice } from './lib/motion';
import { Agents, EndpointTable, EventLog, Failures, Thresholds } from './components/Panels';
import { StartRunDialog } from './components/StartRunDialog';
import { StatTile } from './components/StatTile';
import { TimeChart, type ChartSeries } from './components/TimeChart';
import { Badge, Button, Panel, type Tone } from './components/ui';
import { cn } from './lib/cn';

/** Time windows offered above the charts. */
const RANGES = [
  { label: '1m', seconds: 60 },
  { label: '5m', seconds: 300 },
  { label: '15m', seconds: 900 },
  { label: 'All', seconds: 0 },
] as const;

/** How many endpoints get their own line before the rest are left off.
 *
 *  Bounded by the categorical palette, which has eight validated slots. A
 *  ninth hue would be indistinguishable from an existing one under colour
 *  vision deficiency, so the chart stops rather than inventing one.
 */
const MAX_CHARTED_ENDPOINTS = 8;

/** Extra history kept beyond the selected window, so the continuously
 *  scrolling chart never runs past the end of its data. */
const WINDOW_SLACK_MS = 10_000;

/** Status classes in severity order, which is also the stacking order. */
const STATUS_ORDER = ['2xx', '3xx', '4xx', '5xx', 'error', '1xx', 'other'] as const;

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

  const { run, ticks, totals, agents, endpoints, failures, events, thresholds } = live;

  // The endpoint the response-time chart is isolated to, if any.
  const [selectedEndpoint, setSelectedEndpoint] = useState<string | null>(null);
  const toggleEndpoint = (name: string) =>
    setSelectedEndpoint((current) => (current === name ? null : name));

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
    () => endpointNames.slice(0, MAX_CHARTED_ENDPOINTS),
    [endpointNames],
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

  // An endpoint picked from the table might not be one of the charted eight.
  const visibleEndpoints = useMemo(
    () => (selectedEndpoint ? [selectedEndpoint] : chartedEndpoints),
    [selectedEndpoint, chartedEndpoints],
  );

  const latencySeries = useMemo(
    () => visibleEndpoints.map((name) => windowed.map((t) => t.endpoints?.[name]?.avg ?? 0)),
    [windowed, visibleEndpoints],
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
      visibleEndpoints.map((name) => ({
        label: name,
        color: endpointColor.get(name) ?? colors.categorical[0] ?? colors.textMuted,
      })),
    [visibleEndpoints, endpointColor, colors],
  );

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
        onStart={() => setDialogOpen(true)}
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
            <div className="border-line inline-flex overflow-hidden rounded-md border">
              {RANGES.map((range) => (
                <button
                  key={range.label}
                  type="button"
                  onClick={() => setRangeSeconds(range.seconds)}
                  aria-pressed={rangeSeconds === range.seconds}
                  className={cn(
                    'px-2.5 py-1 text-xs font-medium',
                    rangeSeconds === range.seconds
                      ? 'bg-accent-soft text-accent'
                      : 'text-ink-2 hover:bg-surface-2',
                  )}
                >
                  {range.label}
                </button>
              ))}
            </div>
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
              hint={
                selectedEndpoint
                  ? `average for ${selectedEndpoint}`
                  : 'average per endpoint — click one to isolate it'
              }
              timestamps={timestamps}
              values={latencySeries}
              series={latencySpecs}
              format={(v) => formatMillis(v)}
              colors={colors}
              syncKey="loadwave"
              live={active && motion.smooth}
              spanMs={rangeSeconds * 1000}
              onSelectSeries={toggleEndpoint}
              selected={selectedEndpoint}
              emptyMessage="No requests recorded yet."
              footnote={
                selectedEndpoint
                  ? 'Showing one endpoint. Click it again, or another row below, to show them all.'
                  : endpointNames.length > MAX_CHARTED_ENDPOINTS
                    ? `Showing the ${MAX_CHARTED_ENDPOINTS} busiest of ${endpointNames.length} endpoints. ` +
                      'Click any row below to chart one of the others.'
                    : undefined
              }
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
            selected={selectedEndpoint}
            onSelect={toggleEndpoint}
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
                Start a run
              </Button>
            </div>
          </div>
        </Panel>
      )}

      <StartRunDialog
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onStarted={() => live.refresh()}
      />
    </div>
  );
}

function Header({
  live,
  active,
  themeChoice,
  onThemeChange,
  onStart,
  onPowerOff,
}: {
  live: ReturnType<typeof useLiveState>;
  active: boolean;
  themeChoice: 'light' | 'dark' | 'system';
  onThemeChange: (next: 'light' | 'dark' | 'system') => void;
  onStart: () => void;
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
          <Button variant="primary" onClick={onStart} disabled={agents.length === 0}>
            Start run
          </Button>
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
