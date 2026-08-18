import { useCallback, useEffect, useRef, useState } from 'react';

import { connectStream, fetchHealth, fetchSnapshot, type StreamStatus } from '../api/client';
import type {
  AgentInfo,
  EndpointSummary,
  FailureSummary,
  Frame,
  RunEvent,
  RunSummary,
  SeriesSummary,
  Snapshot,
  ThresholdResult,
  Tick,
} from '../api/types';

/**
 * How many ticks are kept in memory. At one-second resolution this is an hour
 * of history, which matches the coordinator's own retention window; older
 * points are dropped rather than growing the tab's heap without limit during
 * a soak test.
 */
const MAX_TICKS = 3600;

/** How many events to keep. */
const MAX_EVENTS = 300;

/**
 * How often the whole snapshot is refetched while a run is live.
 *
 * Ticks arrive over the stream, but the endpoint table and the whole-run
 * totals are derived from full-cardinality data that would be wasteful to push
 * every second. Polling them on a slower cadence keeps the live charts smooth
 * while the tables stay current enough to act on.
 */
const SNAPSHOT_REFRESH_MS = 5000;

export interface LiveState {
  status: StreamStatus;
  loading: boolean;
  error: string | null;

  build: Snapshot['build'] | null;
  run: RunSummary | undefined;
  runs: RunSummary[];
  agents: AgentInfo[];
  ticks: Tick[];
  totals: Record<string, SeriesSummary>;
  endpoints: EndpointSummary[];
  failures: FailureSummary[];
  series: SeriesSummary[];
  events: RunEvent[];
  thresholds: ThresholdResult[];
  resolutionSeconds: number;
  /** Whether this deployment exposes the Power off control. */
  canShutDown: boolean;

  refresh: () => void;
}

/** Appends new ticks, replacing any whose bucket we already hold.
 *
 *  A reconnection replays recent buckets, and a late-arriving batch can
 *  restate one, so ticks are merged by timestamp rather than blindly pushed —
 *  otherwise the chart grows duplicate points that read as a throughput spike.
 */
function mergeTicks(existing: Tick[], incoming: Tick[]): Tick[] {
  if (incoming.length === 0) return existing;

  const byTime = new Map<number, Tick>();
  for (const tick of existing) byTime.set(tick.t, tick);
  for (const tick of incoming) byTime.set(tick.t, tick);

  const merged = [...byTime.values()].sort((a, b) => a.t - b.t);
  return merged.length > MAX_TICKS ? merged.slice(merged.length - MAX_TICKS) : merged;
}

/**
 * Subscribes to the coordinator and exposes everything the dashboard renders.
 *
 * The stream is the primary source and the REST snapshot fills in on connect,
 * on reconnect and on a slow poll. Keeping both means a dropped frame costs a
 * second of chart detail rather than leaving the page permanently stale.
 */
export function useLiveState(): LiveState {
  const [status, setStatus] = useState<StreamStatus>('connecting');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [build, setBuild] = useState<Snapshot['build'] | null>(null);
  const [run, setRun] = useState<RunSummary | undefined>();
  const [runs, setRuns] = useState<RunSummary[]>([]);
  const [agents, setAgents] = useState<AgentInfo[]>([]);
  const [ticks, setTicks] = useState<Tick[]>([]);
  const [totals, setTotals] = useState<Record<string, SeriesSummary>>({});
  const [endpoints, setEndpoints] = useState<EndpointSummary[]>([]);
  const [failures, setFailures] = useState<FailureSummary[]>([]);
  const [series, setSeries] = useState<SeriesSummary[]>([]);
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [thresholds, setThresholds] = useState<ThresholdResult[]>([]);
  const [resolutionSeconds, setResolutionSeconds] = useState(1);
  const [canShutDown, setCanShutDown] = useState(false);

  // Tracked in a ref rather than state: the tick handler needs to notice a
  // change of run without being re-created every time the run object does.
  const currentRunId = useRef<string | undefined>(undefined);

  const applySnapshot = useCallback((snapshot: Snapshot) => {
    setBuild(snapshot.build);
    setRun(snapshot.run);
    setRuns(snapshot.runs ?? []);
    setAgents(snapshot.agents ?? []);
    setTotals(snapshot.totals ?? {});
    setEndpoints(snapshot.endpoints ?? []);
    setFailures(snapshot.failures ?? []);
    setSeries(snapshot.series ?? []);
    setEvents((snapshot.events ?? []).slice(-MAX_EVENTS));
    setThresholds(snapshot.run?.thresholds ?? []);
    setResolutionSeconds(snapshot.resolutionSeconds || 1);

    // A snapshot is authoritative, so its ticks replace rather than merge.
    currentRunId.current = snapshot.run?.id;
    setTicks((snapshot.ticks ?? []).slice(-MAX_TICKS));
    setLoading(false);
    setError(null);
  }, []);

  const refresh = useCallback(() => {
    fetchSnapshot()
      .then(applySnapshot)
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : String(err));
        setLoading(false);
      });
  }, [applySnapshot]);

  // Whether the process can be stopped from here is fixed for its lifetime,
  // so it is asked once rather than polled.
  useEffect(() => {
    fetchHealth()
      .then((health) => setCanShutDown(health.canShutDown))
      .catch(() => setCanShutDown(false));
  }, []);

  // Initial load and the live stream.
  useEffect(() => {
    refresh();

    return connectStream({
      onStatus: setStatus,
      onFrame: (frame: Frame) => {
        if (frame.type === 'snapshot') {
          applySnapshot(frame.snapshot);
          return;
        }

        if (frame.type === 'event') {
          if (frame.events?.length) {
            setEvents((prev) => [...prev, ...frame.events!].slice(-MAX_EVENTS));
          }
          return;
        }

        // A tick.
        if (frame.run) {
          // Starting a new run must clear the previous run's chart rather
          // than appending to it.
          if (frame.run.id !== currentRunId.current) {
            currentRunId.current = frame.run.id;
            setTicks([]);
          }
          setRun(frame.run);
        }
        if (frame.agents) setAgents(frame.agents);
        if (frame.thresholds) setThresholds(frame.thresholds);
        if (frame.ticks?.length) {
          setTicks((prev) => mergeTicks(prev, frame.ticks!));
        }
      },
    });
  }, [applySnapshot, refresh]);

  // Slow poll for the data the stream deliberately omits.
  useEffect(() => {
    const timer = setInterval(() => {
      if (document.hidden) return;
      fetchSnapshot()
        .then((snapshot) => {
          setTotals(snapshot.totals ?? {});
          setEndpoints(snapshot.endpoints ?? []);
          setFailures(snapshot.failures ?? []);
          setSeries(snapshot.series ?? []);
          setRuns(snapshot.runs ?? []);
          setAgents(snapshot.agents ?? []);
          if (snapshot.run) setRun(snapshot.run);
        })
        .catch(() => {
          // The stream's own status already tells the operator the
          // coordinator is unreachable; a second error message would only
          // add noise.
        });
    }, SNAPSHOT_REFRESH_MS);

    return () => clearInterval(timer);
  }, []);

  return {
    status,
    loading,
    error,
    build,
    run,
    runs,
    agents,
    ticks,
    totals,
    endpoints,
    failures,
    series,
    events,
    thresholds,
    resolutionSeconds,
    canShutDown,
    refresh,
  };
}
