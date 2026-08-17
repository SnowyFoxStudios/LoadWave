import { describe, expect, it } from 'vitest';

import { advanceEdge, approach } from './motion';

describe('approach', () => {
  it('closes on the target without overshooting', () => {
    let value = 0;
    for (let i = 0; i < 200; i += 1) value = approach(value, 100, 4, 1 / 60);

    // Asymptotic by design: deciding when a tween has arrived belongs to the
    // caller, which knows whether these are milliseconds or a Unix epoch.
    expect(value).toBeCloseTo(100, 3);
    expect(value).toBeLessThanOrEqual(100);
  });

  it('is frame-rate independent', () => {
    // The same elapsed time must produce the same result whether it arrives
    // as sixty frames or a hundred and twenty. A fixed per-frame fraction —
    // the usual shortcut — animates twice as fast on a 120Hz display.
    let slow = 0;
    for (let i = 0; i < 60; i += 1) slow = approach(slow, 100, 3, 1 / 60);

    let fast = 0;
    for (let i = 0; i < 120; i += 1) fast = approach(fast, 100, 3, 1 / 120);

    expect(Math.abs(slow - fast)).toBeLessThan(0.5);
  });

  it('starts from the target when the current value is not a number', () => {
    expect(approach(Number.NaN, 42, 4, 0.016)).toBe(42);
  });
});

describe('advanceEdge', () => {
  it('seeds from the data on the first frame', () => {
    expect(advanceEdge(0, 1000, 0.016)).toBe(1000);
  });

  it('snaps rather than crawling when far adrift', () => {
    // A backgrounded tab or a suspended laptop leaves the edge minutes
    // behind; easing back would take exactly as long as the gap.
    expect(advanceEdge(1000, 1120, 0.016)).toBe(1120);
    expect(advanceEdge(1120, 1000, 0.016)).toBe(1000);
  });

  it('advances at about wall-clock rate between data arrivals', () => {
    // No new data for a second: the edge must keep moving, or the chart
    // freezes and then jumps when the next bucket lands.
    const newest = 1000;
    let edge = 1000;
    for (let i = 0; i < 60; i += 1) edge = advanceEdge(edge, newest, 1 / 60);

    // Advanced, but reined in by the drift correction toward the stale datum.
    expect(edge).toBeGreaterThan(1000.3);
    expect(edge).toBeLessThan(1001);
  });

  it('tracks steadily advancing data without drifting away', () => {
    // The realistic case: a point a second, arriving about four seconds
    // behind real time. The edge must neither race ahead into empty space nor
    // fall progressively further behind.
    let edge = 0;
    let newest = 1000;

    for (let second = 0; second < 120; second += 1) {
      for (let frame = 0; frame < 60; frame += 1) {
        edge = advanceEdge(edge, newest, 1 / 60);
      }
      newest += 1;
    }

    // After two minutes it should still be sitting on the data, not adrift.
    expect(Math.abs(edge - newest)).toBeLessThan(2);
  });

  it('moves forward on every frame while tracking', () => {
    // Any frame that fails to advance is a visible stutter.
    let edge = 1000;
    const newest = 1002;
    for (let i = 0; i < 30; i += 1) {
      const next = advanceEdge(edge, newest, 1 / 60);
      expect(next).toBeGreaterThan(edge);
      edge = next;
    }
  });
});
