/**
 * The scenario builder's state, and how it becomes a configuration.
 *
 * Numeric and duration fields are held as strings rather than numbers. A form
 * field is text while it is being typed — "1", "1s", "" and "12x" are all
 * states a user passes through — and coercing on every keystroke fights the
 * person doing the typing. Conversion happens once, in toConfig, where an
 * unparseable value can be reported properly instead of silently becoming
 * zero.
 */

import type { YamlValue } from '../lib/yaml';

/** A key/value row, as edited in the form. */
export interface Pair {
  id: string;
  key: string;
  value: string;
}

export type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE' | 'HEAD';

export const HTTP_METHODS: readonly HttpMethod[] = [
  'GET',
  'POST',
  'PUT',
  'PATCH',
  'DELETE',
  'HEAD',
];

/** Methods that conventionally carry a body. Used only to decide what the form
 *  offers by default; the server does not care. */
const BODY_METHODS: readonly HttpMethod[] = ['POST', 'PUT', 'PATCH'];

export type BodyKind = 'none' | 'json' | 'form' | 'raw';

export type StepKind = 'request' | 'think';

/** One step of a scenario. */
export interface Step {
  id: string;
  kind: StepKind;

  // request
  name: string;
  method: HttpMethod;
  url: string;
  expect: string;
  headers: Pair[];
  query: Pair[];
  bodyKind: BodyKind;
  body: string;
  form: Pair[];
  capture: Pair[];
  betweenRequests: string;
  timeout: string;

  // think
  think: string;
}

export interface Scenario {
  id: string;
  name: string;
  weight: string;
  description: string;
  steps: Step[];
}

export interface Threshold {
  id: string;
  metric: string;
  stat: string;
  op: string;
  value: string;
  abortOnFail: boolean;
}

export interface Stage {
  id: string;
  duration: string;
  target: string;
}

export type Executor = 'constant-vus' | 'ramping-vus';

/** Everything the builder edits. */
export interface Draft {
  name: string;
  baseURL: string;

  executor: Executor;
  vus: string;
  duration: string;
  stages: Stage[];
  gracefulStop: string;
  maxIterationRate: string;
  iterations: string;

  betweenRequests: string;
  workersPerAgent: string;

  timeout: string;
  followRedirects: boolean;
  insecureSkipTLSVerify: boolean;
  headers: Pair[];

  tags: Pair[];
  thresholds: Threshold[];
  scenarios: Scenario[];
}

/** Metrics offered in the threshold picker. */
export const THRESHOLD_METRICS = [
  'http_req_duration',
  'http_req_waiting',
  'http_req_failed',
  'http_reqs',
  'checks',
  'iterations',
  'iteration_duration',
  'iteration_failed',
] as const;

export const THRESHOLD_STATS = [
  'p95',
  'p99',
  'p90',
  'p50',
  'avg',
  'min',
  'max',
  'rate',
  'count',
] as const;

export const THRESHOLD_OPS = ['<', '<=', '>', '>='] as const;

/** Fresh identifiers for list rows. Only ever called from event handlers. */
function newId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `id-${Math.random().toString(36).slice(2)}`;
}

export function newPair(key = '', value = ''): Pair {
  return { id: newId(), key, value };
}

export function newStage(duration = '30s', target = '50'): Stage {
  return { id: newId(), duration, target };
}

export function newThreshold(): Threshold {
  return {
    id: newId(),
    metric: 'http_req_duration',
    stat: 'p95',
    op: '<',
    value: '500',
    abortOnFail: false,
  };
}

export function newStep(kind: StepKind = 'request'): Step {
  return {
    id: newId(),
    kind,
    name: '',
    method: 'GET',
    url: '/',
    expect: '200',
    headers: [],
    query: [],
    bodyKind: 'none',
    body: '',
    form: [],
    capture: [],
    betweenRequests: '',
    timeout: '',
    think: '1s-3s',
  };
}

export function newScenario(name = 'browse'): Scenario {
  return { id: newId(), name, weight: '1', description: '', steps: [newStep()] };
}

/** The draft a new builder starts from: a complete, runnable test. */
export function newDraft(): Draft {
  return {
    name: 'my-test',
    baseURL: 'http://localhost:8080',
    executor: 'ramping-vus',
    vus: '50',
    duration: '5m',
    stages: [newStage('30s', '25'), newStage('2m', '25'), newStage('30s', '0')],
    gracefulStop: '30s',
    maxIterationRate: '',
    iterations: '',
    betweenRequests: '',
    workersPerAgent: '',
    timeout: '',
    followRedirects: false,
    insecureSkipTLSVerify: false,
    headers: [],
    tags: [],
    thresholds: [
      {
        id: newId(),
        metric: 'http_req_duration',
        stat: 'p95',
        op: '<',
        value: '500',
        abortOnFail: false,
      },
      {
        id: newId(),
        metric: 'http_req_failed',
        stat: 'rate',
        op: '<',
        value: '0.01',
        abortOnFail: false,
      },
    ],
    scenarios: [newScenario()],
  };
}

/**
 * The shape the server renders a configuration into for the builder — the
 * same fields as {@link Draft}, minus the `id` every list row carries here.
 * An id is only ever used as a React key; the server has no reason to invent
 * one, so this type exists to make that gap explicit at the boundary rather
 * than reaching for `any`.
 */
export interface RawDraft {
  name: string;
  baseURL: string;
  executor: Executor;
  vus: string;
  duration: string;
  stages: { duration: string; target: string }[];
  gracefulStop: string;
  maxIterationRate: string;
  iterations: string;
  betweenRequests: string;
  workersPerAgent: string;
  timeout: string;
  followRedirects: boolean;
  insecureSkipTLSVerify: boolean;
  headers: { key: string; value: string }[];
  tags: { key: string; value: string }[];
  thresholds: {
    metric: string;
    stat: string;
    op: string;
    value: string;
    abortOnFail: boolean;
  }[];
  scenarios: {
    name: string;
    weight: string;
    description: string;
    steps: {
      kind: StepKind;
      name: string;
      method: HttpMethod;
      url: string;
      expect: string;
      headers: { key: string; value: string }[];
      query: { key: string; value: string }[];
      bodyKind: BodyKind;
      body: string;
      form: { key: string; value: string }[];
      capture: { key: string; value: string }[];
      betweenRequests: string;
      timeout: string;
      think: string;
    }[];
  }[];
}

/** Turns a server-rendered configuration into an editable draft, assigning
 *  fresh ids for the rows to key on. */
export function draftFromRaw(raw: RawDraft): Draft {
  return {
    ...raw,
    headers: raw.headers.map((pair) => ({ id: newId(), ...pair })),
    tags: raw.tags.map((pair) => ({ id: newId(), ...pair })),
    stages: raw.stages.map((stage) => ({ id: newId(), ...stage })),
    thresholds: raw.thresholds.map((threshold) => ({ id: newId(), ...threshold })),
    scenarios: raw.scenarios.map((scenario) => ({
      id: newId(),
      ...scenario,
      steps: scenario.steps.map((step) => ({
        id: newId(),
        ...step,
        headers: step.headers.map((pair) => ({ id: newId(), ...pair })),
        query: step.query.map((pair) => ({ id: newId(), ...pair })),
        form: step.form.map((pair) => ({ id: newId(), ...pair })),
        capture: step.capture.map((pair) => ({ id: newId(), ...pair })),
      })),
    })),
  };
}

/** Collapses key/value rows into a map, dropping incomplete ones. */
function pairsToMap(pairs: Pair[]): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const pair of pairs) {
    const key = pair.key.trim();
    if (key === '') continue;
    out[key] = pair.value;
  }
  return Object.keys(out).length === 0 ? undefined : out;
}

/** Parses a whole number, or undefined when the field is blank. */
function toInt(text: string): number | undefined {
  const trimmed = text.trim();
  if (trimmed === '') return undefined;
  const parsed = Number.parseInt(trimmed, 10);
  return Number.isFinite(parsed) ? parsed : undefined;
}

/** Parses a decimal, or undefined when the field is blank. */
function toNumber(text: string): number | undefined {
  const trimmed = text.trim();
  if (trimmed === '') return undefined;
  const parsed = Number.parseFloat(trimmed);
  return Number.isFinite(parsed) ? parsed : undefined;
}

/** Parses an expected-status list: "200", "200,201", "200 201". */
function toStatuses(text: string): number[] | undefined {
  const parts = text
    .split(/[,\s]+/)
    .map((part) => part.trim())
    .filter((part) => part !== '');

  const codes: number[] = [];
  for (const part of parts) {
    const code = Number.parseInt(part, 10);
    if (Number.isFinite(code)) codes.push(code);
  }
  return codes.length === 0 ? undefined : codes;
}

/** Blank becomes undefined, so the emitter omits the field entirely. */
function text(value: string): string | undefined {
  const trimmed = value.trim();
  return trimmed === '' ? undefined : trimmed;
}

/** A problem found before the configuration is even sent. */
export interface DraftProblem {
  /** Where it is, for the message: "Scenario 1, step 2". */
  where: string;
  message: string;
}

/**
 * Checks the draft for the things the server cannot explain well.
 *
 * The server is the authority on whether a configuration is valid, and its
 * messages are better than anything restated here — they carry the line and
 * the field. This covers only what would otherwise be reported obscurely: a
 * JSON body that does not parse cannot even be turned into YAML, so it would
 * surface as a puzzling shape error rather than "this JSON is malformed".
 */
export function findProblems(draft: Draft): DraftProblem[] {
  const problems: DraftProblem[] = [];

  draft.scenarios.forEach((scenario, s) => {
    const label = scenario.name.trim() === '' ? `Scenario ${s + 1}` : scenario.name;

    if (scenario.steps.length === 0) {
      problems.push({ where: label, message: 'has no steps' });
    }

    scenario.steps.forEach((step, i) => {
      const at = `${label}, step ${i + 1}`;

      if (step.kind === 'think') {
        if (text(step.think) === undefined) {
          problems.push({ where: at, message: 'a think step needs a duration' });
        }
        return;
      }

      if (text(step.url) === undefined) {
        problems.push({ where: at, message: 'needs a URL' });
      }
      if (step.bodyKind === 'json' && text(step.body) !== undefined) {
        try {
          JSON.parse(step.body);
        } catch (err) {
          problems.push({
            where: at,
            message: `the JSON body is malformed: ${err instanceof Error ? err.message : String(err)}`,
          });
        }
      }
    });
  });

  return problems;
}

/** True when this method should be offered a body by default. */
export function methodTakesBody(method: HttpMethod): boolean {
  return BODY_METHODS.includes(method);
}

/** Renders one step as its configuration entry. */
function stepToConfig(step: Step): YamlValue {
  if (step.kind === 'think') {
    return { think: text(step.think) ?? '1s' };
  }

  // The method shorthand — `get: /path` — rather than method plus url. It is
  // what the examples use and what reads best in a file somebody edits later.
  const shorthand = step.method.toLowerCase();
  const entry: Record<string, YamlValue> = {
    name: text(step.name),
    [shorthand]: text(step.url) ?? '/',
    headers: pairsToMap(step.headers),
    query: pairsToMap(step.query),
    expect: toStatuses(step.expect),
    capture: pairsToMap(step.capture),
    betweenRequests: text(step.betweenRequests),
    timeout: text(step.timeout),
  };

  switch (step.bodyKind) {
    case 'json': {
      const raw = text(step.body);
      if (raw !== undefined) {
        try {
          // Embedded as structure, not as a string, so the YAML reads as a
          // document rather than a quoted blob.
          entry.json = JSON.parse(raw) as YamlValue;
        } catch {
          // findProblems reports this; keeping the text is more useful here
          // than dropping the body silently.
          entry.body = raw;
        }
      }
      break;
    }
    case 'form':
      entry.form = pairsToMap(step.form);
      break;
    case 'raw':
      entry.body = text(step.body);
      break;
    case 'none':
      break;
  }

  return entry;
}

/**
 * Turns the draft into the configuration object the emitter serialises.
 *
 * Field order here is the order they appear in the file, so it is chosen to
 * read top-down the way the examples do: what the test is, then how much load,
 * then how it is paced, then what to assert, then what to do.
 */
export function toConfig(draft: Draft): YamlValue {
  const load: Record<string, YamlValue> = {
    executor: draft.executor,
    gracefulStop: text(draft.gracefulStop),
    maxIterationRate: toInt(draft.maxIterationRate),
    iterations: toInt(draft.iterations),
  };

  if (draft.executor === 'ramping-vus') {
    load.stages = draft.stages.map((stage) => ({
      duration: text(stage.duration) ?? '30s',
      target: toInt(stage.target) ?? 0,
    }));
  } else {
    load.vus = toInt(draft.vus);
    load.duration = text(draft.duration);
  }

  return {
    name: text(draft.name),
    baseURL: text(draft.baseURL),
    load,
    betweenRequests: text(draft.betweenRequests),
    workersPerAgent: toInt(draft.workersPerAgent),
    http: {
      timeout: text(draft.timeout),
      headers: pairsToMap(draft.headers),
      // Emitted only when switched on: false is the default, and a file full
      // of restated defaults is harder to read than one that says only what
      // it changes.
      followRedirects: draft.followRedirects ? true : undefined,
      insecureSkipTLSVerify: draft.insecureSkipTLSVerify ? true : undefined,
    },
    tags: pairsToMap(draft.tags),
    thresholds: draft.thresholds.map((threshold) => ({
      metric: threshold.metric,
      stat: threshold.stat,
      op: threshold.op,
      value: toNumber(threshold.value) ?? 0,
      abortOnFail: threshold.abortOnFail ? true : undefined,
    })),
    scenarios: draft.scenarios.map((scenario) => ({
      name: text(scenario.name) ?? 'scenario',
      weight: toInt(scenario.weight),
      description: text(scenario.description),
      steps: scenario.steps.map(stepToConfig),
    })),
  };
}
