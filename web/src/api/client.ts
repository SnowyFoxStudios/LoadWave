import type { RawDraft } from '../builder/model';
import type { Frame, Snapshot } from './types';

/** Base path for every REST call. Relative, because the coordinator serves
 *  this bundle itself and the dev server proxies the same prefix. */
const API = '/api/v1';

/** An API call that failed, carrying the server's message rather than a
 *  generic "fetch failed" that tells the operator nothing. */
export class ApiError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/** Shape of the coordinator's error responses. */
interface ErrorBody {
  error?: string;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${API}${path}`, {
    ...init,
    headers: { Accept: 'application/json', ...init?.headers },
  });

  if (!response.ok) {
    // The coordinator answers with {"error": "..."} for everything it
    // rejects; surfacing that verbatim is the difference between "409" and
    // "no agents are connected; start one with `loadwave agent`".
    let message = `${response.status} ${response.statusText}`;
    try {
      const body = (await response.json()) as ErrorBody;
      if (body.error) message = body.error;
    } catch {
      /* not JSON — the status line is the best we have */
    }
    throw new ApiError(message, response.status);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export function fetchSnapshot(): Promise<Snapshot> {
  return request<Snapshot>('/snapshot');
}

export function fetchRun(runId: string): Promise<Snapshot> {
  return request<Snapshot>(`/runs/${encodeURIComponent(runId)}`);
}

/** One scenario as the runner would see it. */
export interface ValidateScenario {
  name: string;
  weight: number;
  description?: string;
  steps: number;
  source: 'yaml' | 'go';
}

/** What a configuration would actually do, echoed back. */
export interface ValidateSummary {
  name: string;
  baseURL?: string;
  profile: string;
  peakVUs: number;
  durationSeconds: number;
  iterations?: number;
  iterationRate?: number;
  workersPerAgent?: number;
  betweenRequests: string;
  pacingDefaulted: boolean;
  scenarios: ValidateScenario[];
  thresholds?: string[];
}

export interface ValidateResult {
  valid: boolean;
  error?: string;
  summary?: ValidateSummary;
}

/**
 * Checks a configuration without running it.
 *
 * Answers 200 whether or not the configuration is valid, so an invalid one is
 * a result to render rather than an exception to catch.
 */
export function validateConfig(config: string): Promise<ValidateResult> {
  return request<ValidateResult>('/validate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/yaml' },
    body: config,
  });
}

/** Starts a run from a configuration in YAML or JSON. */
export function startRun(config: string): Promise<{ runId: string }> {
  return request<{ runId: string }>('/runs', {
    method: 'POST',
    headers: { 'Content-Type': 'application/yaml' },
    body: config,
  });
}

/** A run's configuration, as it was actually started. */
export interface RunConfig {
  yaml: string;
  /** The file this run was started from, or empty when there isn't one —
   *  a Go scenario, CLI flags with no file, or a config posted directly. */
  sourcePath: string;
  /** The same configuration, rendered for the Build form. Absent for the
   *  rare plan with nothing to render — assembled from flags alone, with no
   *  file and no scenario at all. */
  draft?: RawDraft;
}

/** Fetches the exact configuration a run was started from. */
export function fetchRunConfig(runId: string): Promise<RunConfig> {
  return request<RunConfig>(`/runs/${encodeURIComponent(runId)}/config`);
}

/** Saves edited YAML back to the file a run was started from.
 *
 *  Only valid for a run whose RunConfig.sourcePath is non-empty; the
 *  coordinator rejects anything else with 409.
 */
export function saveRunConfig(runId: string, config: string): Promise<{ savedTo: string }> {
  return request<{ savedTo: string }>(`/runs/${encodeURIComponent(runId)}/config`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/yaml' },
    body: config,
  });
}

export function stopRun(runId: string, graceful = true): Promise<unknown> {
  return request(`/runs/${encodeURIComponent(runId)}/stop`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ graceful }),
  });
}

/** Changes a running test's peak virtual user count.
 *
 *  `rampSeconds` spreads the change over a period instead of applying it in
 *  one tick. Zero is immediate.
 */
export function scaleRun(runId: string, vus: number, rampSeconds = 0): Promise<unknown> {
  return request(`/runs/${encodeURIComponent(runId)}/scale`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ vus, rampSeconds }),
  });
}

/** Health, including whether this deployment can be shut down from here. */
export interface Health {
  status: string;
  agents: number;
  canShutDown: boolean;
}

export function fetchHealth(): Promise<Health> {
  return request<Health>('/health');
}

/** Ends the LoadWave process.
 *
 *  Distinct from stopping a run: stopping leaves the coordinator up so the
 *  results stay readable and another run can be started. This is the deliberate
 *  "I am finished" action.
 */
export function shutdown(): Promise<unknown> {
  return request('/shutdown', { method: 'POST' });
}

/** URL of a run's downloadable HTML report.
 *
 *  A plain link rather than a fetch: the server sets Content-Disposition, so
 *  the browser handles the download itself and there is no blob to build,
 *  name and revoke.
 */
export function reportURL(runId: string): string {
  return `${API}/runs/${encodeURIComponent(runId)}/report.html`;
}

/** Connection state of the live stream, surfaced in the header. */
export type StreamStatus = 'connecting' | 'open' | 'closed';

export interface StreamHandlers {
  onFrame: (frame: Frame) => void;
  onStatus: (status: StreamStatus) => void;
}

/** Reconnection backoff bounds, in milliseconds. */
const RECONNECT_MIN = 500;
const RECONNECT_MAX = 10_000;

/**
 * Opens the live stream and keeps it open.
 *
 * Returns a function that closes it for good. Reconnection is automatic and
 * backs off, because the coordinator restarting mid-run is routine and the
 * dashboard should recover on its own rather than needing a page reload —
 * each reconnection begins with a fresh snapshot, so no history is lost.
 */
export function connectStream({ onFrame, onStatus }: StreamHandlers): () => void {
  let socket: WebSocket | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | undefined;
  let backoff = RECONNECT_MIN;
  let closed = false;

  const open = () => {
    if (closed) return;
    onStatus('connecting');

    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
    socket = new WebSocket(`${scheme}://${window.location.host}${API}/stream`);

    socket.onopen = () => {
      backoff = RECONNECT_MIN;
      onStatus('open');
    };

    socket.onmessage = (event: MessageEvent<string>) => {
      try {
        onFrame(JSON.parse(event.data) as Frame);
      } catch {
        // A frame we cannot parse is a version mismatch between this bundle
        // and the coordinator. Dropping it keeps the rest of the stream
        // working rather than tearing the connection down.
      }
    };

    socket.onclose = () => {
      socket = null;
      if (closed) return;
      onStatus('closed');
      retryTimer = setTimeout(open, backoff);
      backoff = Math.min(backoff * 2, RECONNECT_MAX);
    };

    socket.onerror = () => socket?.close();
  };

  open();

  return () => {
    closed = true;
    if (retryTimer) clearTimeout(retryTimer);
    socket?.close();
  };
}
