/**
 * How the response-time chart rolls per-request averages up.
 *
 * A tick carries one average per request name and nothing that says which
 * scenario issued it, so the hierarchy the chart offers — request, step,
 * scenario — has to be reassembled here. Kept out of the component because
 * the arithmetic is the part worth testing: a mean and a sum answer different
 * questions, and picking the wrong one is invisible on the chart.
 */

import type { EndpointTick, SeriesSummary } from '../api/types';
import { Metric } from '../api/types';

/** The aggregation the response-time chart is showing. */
export type LatencyView = 'total' | 'individual' | 'step' | 'scenario';

/** One step's line on the chart: which request it measures, and the scenario
 *  it belongs to. */
export interface StepLine {
  /** Stable identity, unique even when two scenarios share a step name. */
  key: string;
  /** What the legend prints. */
  label: string;
  /** The request name to read out of a tick. */
  name: string;
  /** Empty when no scenario claims the request. */
  scenario: string;
}

/**
 * Maps each request name to the scenarios that issue it.
 *
 * The per-endpoint timeline in a tick is keyed by request name alone — the
 * store deliberately does not split it per scenario — so the association comes
 * from the cumulative series, which carry both labels.
 *
 * A name used by two scenarios is listed under both rather than being assigned
 * to the busier one: each of those scenarios really does pay that request on
 * every pass, so each one's total should include it.
 */
export function scenariosByEndpoint(series: SeriesSummary[]): Map<string, string[]> {
  const owners = new Map<string, string[]>();

  for (const entry of series) {
    if (entry.metric !== Metric.httpReqDuration) continue;

    const name = entry.tags?.name;
    const scenario = entry.tags?.scenario;
    if (!name || !scenario) continue;

    const listed = owners.get(name);
    if (listed === undefined) owners.set(name, [scenario]);
    else if (!listed.includes(scenario)) listed.push(scenario);
  }
  return owners;
}

/**
 * What one pass through a scenario's own steps costs.
 *
 * A sum, not a mean. A scenario that issues three requests waits for all
 * three, so a pass costs their averages added together; the mean of the three
 * answers "how slow is a typical request", which is what the Total view is
 * for. Each scenario line is therefore the sum of its own step lines.
 */
export function sumScenarioAverages(
  endpoints: Record<string, EndpointTick> | undefined,
  scenario: string,
  owners: Map<string, string[]>,
): number {
  let total = 0;
  for (const [name, point] of Object.entries(endpoints ?? {})) {
    if (!Number.isFinite(point.avg)) continue;
    if (owners.get(name)?.includes(scenario)) total += point.avg;
  }
  return total;
}

/**
 * One line per step, grouped under the scenario that runs it.
 *
 * A step is a request, so these are the same measurements the per-request view
 * draws; what the step view adds is which scenario each one belongs to, which
 * a tick's flat map of request names cannot say. A step two scenarios share
 * gets a line under each, because each of those passes really does run it —
 * the two carry identical numbers, the store having merged them by name.
 *
 * `names` is expected busiest-first; within a scenario that ordering is kept,
 * and the scenarios themselves are ordered by name so the legend groups.
 */
export function stepLines(names: string[], owners: Map<string, string[]>): StepLine[] {
  const lines: StepLine[] = [];

  for (const name of names) {
    const scenarios = owners.get(name);
    if (scenarios === undefined || scenarios.length === 0) {
      lines.push({ key: name, label: name, name, scenario: '' });
      continue;
    }
    for (const scenario of scenarios) {
      lines.push({
        key: `${scenario}\u0000${name}`,
        label: `${scenario} · ${name}`,
        name,
        scenario,
      });
    }
  }

  // Unattributed steps sort last: they are the odd case, and leading with them
  // would push the scenario groups down the legend.
  return lines.sort((a, b) => {
    if (a.scenario !== b.scenario) {
      if (a.scenario === '') return 1;
      if (b.scenario === '') return -1;
      return a.scenario.localeCompare(b.scenario);
    }
    return names.indexOf(a.name) - names.indexOf(b.name);
  });
}

/**
 * Trims a list to the palette's capacity without dropping what a reader asked
 * to see.
 *
 * Highlighted entries take the slots first, then the rest; the survivors come
 * back in the original order, so highlighting something does not reshuffle the
 * legend around it.
 */
export function withinPalette<T>(
  items: readonly T[],
  limit: number,
  highlighted: (item: T) => boolean,
): T[] {
  if (items.length <= limit) return [...items];

  const kept = new Set(
    [...items.filter(highlighted), ...items.filter((i) => !highlighted(i))].slice(0, limit),
  );
  return items.filter((item) => kept.has(item));
}
