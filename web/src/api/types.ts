/**
 * Wire types, mirroring the Go structs the coordinator serves.
 *
 * These are hand-written rather than generated. The API surface is small and
 * stable, and hand-writing it keeps the build free of a codegen step that
 * every contributor would otherwise have to install and remember to re-run.
 * `internal/coordinator/stream.go` is the source of truth; keep the two in
 * step when either changes.
 */

/** Lifecycle phase of a run, as spelled by the API. */
export type RunPhase =
  'pending' | 'starting' | 'running' | 'stopping' | 'completed' | 'failed' | 'aborted';

/** Phases in which a run is over and its numbers are final. */
export const TERMINAL_PHASES: readonly RunPhase[] = ['completed', 'failed', 'aborted'];

export function isTerminal(phase: RunPhase | undefined): boolean {
  return phase !== undefined && TERMINAL_PHASES.includes(phase);
}

export interface BuildInfo {
  version: string;
  commit: string;
  buildDate: string;
  goVersion: string;
  platform: string;
  dirty: boolean;
}

export interface Participant {
  agentId: string;
  vuQuota: number;
  rateQuota: number;
  shardIndex: number;
  phase: RunPhase;
  message?: string;
  dispatched: boolean;
}

export interface ThresholdResult {
  metric: string;
  stat: string;
  op: string;
  value: number;
  actual: number;
  /** False when the metric was never observed — distinct from a pass. */
  evaluated: boolean;
  passed: boolean;
  abortOnFail: boolean;
  description: string;
}

export interface StoreStats {
  series: number;
  closedBuckets: number;
  openBuckets: number;
  droppedSeries: number;
  droppedLate: number;
  droppedByNode: number;
  droppedEndpoints: number;
  droppedFailureKinds: number;
  started: string;
  lastSeen: string;
}

export interface RunSummary {
  id: string;
  name: string;
  phase: RunPhase;
  createdAt: string;
  startAt?: string;
  startedAt?: string;
  endedAt?: string;
  elapsedSeconds: number;
  peakVUs: number;
  profile: string;
  baseURL: string;
  stopReason?: string;
  failure?: string;
  thresholdsBreached: boolean;
  tags?: Record<string, string>;
  participants: Participant[];
  thresholds: ThresholdResult[];
  stats: StoreStats;
}

export interface AgentInfo {
  id: string;
  hostname: string;
  version: string;
  cores: number;
  maxWorkers: number;
  maxVUs: number;
  labels?: Record<string, string>;
  remoteAddr: string;
  joinedAt: string;
  lastSeen: string;
  activeVUs: number;
  healthyWorkers: number;
  healthy: boolean;
  vuQuota: number;
  /** This agent process's own footprint — supervision, not the load its
   *  workers generate. */
  cpuPercent: number;
  memBytes: number;
  workers: WorkerInfo[];
}

/** One worker process's resource usage, as its agent reported it. */
export interface WorkerInfo {
  id: string;
  index: number;
  activeVUs: number;
  cpuPercent: number;
  memBytes: number;
}

/** One endpoint's slice of a time bucket.
 *
 *  Average only. Percentiles per endpoint per second would mean a histogram
 *  per endpoint per second, which the store deliberately does not keep;
 *  whole-run percentiles are in `Snapshot.endpoints`.
 */
export interface EndpointTick {
  avg: number;
  requests: number;
  errorRate: number;
}

export interface ScenarioTick {
  vus: number;
  iterations: number;
  requests: number;
  errorRate: number;
  p95: number;
}

/** One resolution interval of a run, pre-flattened for charting. */
export interface Tick {
  /** Bucket start, in epoch milliseconds. */
  t: number;
  vus: number;
  requests: number;
  failures: number;
  iterations: number;
  rps: number;
  errorRate: number;
  avg: number;
  p50: number;
  p90: number;
  p95: number;
  p99: number;
  status?: Record<string, number>;
  scenarios?: Record<string, ScenarioTick>;
  endpoints?: Record<string, EndpointTick>;
}

export interface SeriesSummary {
  metric: string;
  kind: string;
  tags?: Record<string, string>;
  count: number;
  sum: number;
  min: number;
  max: number;
  avg: number;
  rate: number;
  percentiles?: Record<string, number>;
}

export interface RunEvent {
  time: string;
  level: 'debug' | 'info' | 'warn' | 'error';
  source: string;
  message: string;
  fields?: Record<string, string>;
}

export interface EndpointSummary {
  name: string;
  requests: number;
  failures: number;
  errorRate: number;
  avg: number;
  min: number;
  max: number;
  percentiles?: Record<string, number>;
  statuses?: Record<string, number>;
  bytesIn: number;
}

/** One kind of failure, aggregated over the whole run. */
export interface FailureSummary {
  name: string;
  method: string;
  /** HTTP status, or 0 when no response arrived. */
  status: number;
  /** Transport failure class; empty when a response was received. */
  errorClass?: string;
  /** A short excerpt of the response body or the transport error. */
  message?: string;
  count: number;
  lastSeen: string;
}

export interface Snapshot {
  build: BuildInfo;
  run?: RunSummary;
  runs: RunSummary[];
  agents: AgentInfo[];
  ticks: Tick[];
  series: SeriesSummary[];
  events: RunEvent[];
  resolutionSeconds: number;
  /**
   * Correctly merged whole-run aggregate per metric.
   *
   * Always prefer these over folding `series` in the browser: percentiles
   * cannot be averaged, and a mean has to be re-weighted by count, so a
   * client-side fold produces numbers that disagree with the thresholds.
   */
  totals?: Record<string, SeriesSummary>;
  /**
   * Per-request-name breakdown, already sorted slowest-first. Percentiles are
   * recomputed server-side from each endpoint's merged distribution, which
   * cannot be reconstructed from `series` in the browser.
   */
  endpoints?: EndpointSummary[];
  /** What went wrong, and why — the half the metrics cannot express. */
  failures?: FailureSummary[];
}

/** Incremental update pushed over the WebSocket. */
export interface Update {
  type: 'tick' | 'event';
  run?: RunSummary;
  agents?: AgentInfo[];
  ticks?: Tick[];
  thresholds?: ThresholdResult[];
  events?: RunEvent[];
}

/** The first frame on a new connection, carrying the full current state. */
export interface SnapshotFrame {
  type: 'snapshot';
  snapshot: Snapshot;
}

/** Envelope for every stream frame. */
export type Frame = SnapshotFrame | Update;

/** Standard metric names, matching pkg/loadwave/metrics.go. */
export const Metric = {
  iterations: 'iterations',
  iterationDuration: 'iteration_duration',
  vus: 'vus',
  httpReqs: 'http_reqs',
  httpReqDuration: 'http_req_duration',
  httpReqWaiting: 'http_req_waiting',
  httpReqFailed: 'http_req_failed',
  httpReqBytesIn: 'http_req_bytes_in',
  httpReqBytesOut: 'http_req_bytes_out',
  checks: 'checks',
} as const;
