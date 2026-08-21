import { describe, expect, it } from 'vitest';

import type { EndpointTick, SeriesSummary } from '../api/types';
import { scenariosByEndpoint, stepLines, sumScenarioAverages, withinPalette } from './latency';

function endpoint(avg: number): EndpointTick {
  return { avg, requests: 1, errorRate: 0 };
}

function duration(name: string, scenario: string): SeriesSummary {
  return {
    metric: 'http_req_duration',
    kind: 'trend',
    tags: { name, scenario, status: '200' },
    count: 1,
    sum: 1,
    min: 1,
    max: 1,
    avg: 1,
    rate: 0,
  };
}

describe('stepLines', () => {
  const owners = scenariosByEndpoint([
    duration('login', 'browse'),
    duration('login', 'checkout'),
    duration('search', 'browse'),
    duration('pay', 'checkout'),
  ]);

  it('groups the steps under their scenario, keeping traffic order within one', () => {
    // Busiest-first in, grouped by scenario out — so the legend reads as the
    // scenarios it came from rather than as one flat ranking.
    const lines = stepLines(['pay', 'login', 'search'], owners);
    expect(lines.map((line) => line.label)).toEqual([
      'browse · login',
      'browse · search',
      'checkout · pay',
      'checkout · login',
    ]);
  });

  it('gives a shared step a line under each scenario that runs it', () => {
    const lines = stepLines(['login'], owners);
    expect(lines).toHaveLength(2);
    // Both read the same request out of a tick: the store merged them by name.
    expect(new Set(lines.map((line) => line.name))).toEqual(new Set(['login']));
    expect(new Set(lines.map((line) => line.key)).size).toBe(2);
  });

  it('keeps a request no scenario claims, and sorts it last', () => {
    const lines = stepLines(['mystery', 'search'], owners);
    expect(lines.map((line) => line.label)).toEqual(['browse · search', 'mystery']);
  });
});

describe('scenariosByEndpoint', () => {
  it('lists every scenario that issues a request name', () => {
    // login is shared: both scenarios pay it on each pass, so both must own it.
    const owners = scenariosByEndpoint([
      duration('login', 'browse'),
      duration('login', 'checkout'),
      duration('search', 'browse'),
    ]);
    expect(owners.get('login')).toEqual(['browse', 'checkout']);
    expect(owners.get('search')).toEqual(['browse']);
  });

  it('ignores series that are not request durations, and untagged ones', () => {
    const owners = scenariosByEndpoint([
      {
        metric: 'http_reqs',
        kind: 'counter',
        tags: { name: 'login', scenario: 'browse' },
        count: 1,
        sum: 1,
        min: 0,
        max: 1,
        avg: 1,
        rate: 0,
      },
      duration('', 'browse'),
    ]);
    expect(owners.size).toBe(0);
  });
});

describe('sumScenarioAverages', () => {
  const owners = scenariosByEndpoint([
    duration('login', 'browse'),
    duration('login', 'checkout'),
    duration('search', 'browse'),
    duration('pay', 'checkout'),
  ]);

  it('sums only the requests that scenario issues', () => {
    const tick = { login: endpoint(100), search: endpoint(80), pay: endpoint(250) };
    expect(sumScenarioAverages(tick, 'browse', owners)).toBe(180);
    expect(sumScenarioAverages(tick, 'checkout', owners)).toBe(350);
  });

  it('leaves out requests whose scenario is unknown', () => {
    // A name absent from the cumulative series cannot be attributed, and
    // guessing would inflate whichever scenario it landed on.
    expect(sumScenarioAverages({ mystery: endpoint(900) }, 'browse', owners)).toBe(0);
  });
});

describe('withinPalette', () => {
  const busiest = ['a', 'b', 'c', 'd'];

  it('leaves a list that already fits alone', () => {
    expect(withinPalette(busiest, 4, () => false)).toEqual(busiest);
  });

  it('keeps a highlighted entry that traffic alone would have cut', () => {
    // Highlighting the quietest endpoint has to chart it, or the click looks
    // like it did nothing.
    expect(withinPalette(busiest, 2, (name) => name === 'd')).toEqual(['a', 'd']);
  });

  it('preserves the original order rather than floating the highlight to the top', () => {
    expect(withinPalette(busiest, 3, (name) => name === 'c' || name === 'd')).toEqual([
      'a',
      'c',
      'd',
    ]);
  });
});
