import { describe, expect, it } from 'vitest';

import {
  formatBytes,
  formatCompact,
  formatCount,
  formatElapsed,
  formatMillis,
  formatPercent,
  formatRate,
} from './format';

describe('formatMillis', () => {
  it('picks a unit that keeps the number readable', () => {
    expect(formatMillis(0.4)).toBe('400µs');
    expect(formatMillis(4.25)).toBe('4.3ms');
    expect(formatMillis(250)).toBe('250ms');
    expect(formatMillis(1500)).toBe('1.50s');
  });

  it('renders absent values as a dash rather than a misleading zero', () => {
    // A metric with no observations is not "0ms"; saying so would read as an
    // instantaneous response.
    expect(formatMillis(0)).toBe('—');
    expect(formatMillis(undefined)).toBe('—');
    expect(formatMillis(null)).toBe('—');
    expect(formatMillis(Number.NaN)).toBe('—');
  });
});

describe('formatPercent', () => {
  it('formats ratios', () => {
    expect(formatPercent(0)).toBe('0%');
    expect(formatPercent(0.0159)).toBe('1.59%');
    expect(formatPercent(1)).toBe('100.00%');
    expect(formatPercent(0.985, 1)).toBe('98.5%');
  });

  it('does not round a small but non-zero error rate down to zero', () => {
    // "0%" when requests are genuinely failing is the worst thing this
    // function could say.
    expect(formatPercent(0.00001)).toBe('<0.01%');
  });
});

describe('formatRate', () => {
  it('scales with magnitude', () => {
    expect(formatRate(0)).toBe('0');
    expect(formatRate(4.27)).toBe('4.3');
    expect(formatRate(1240)).toBe('1240');
    expect(formatRate(48_000)).toBe('48.0k');
  });
});

describe('formatCount and formatCompact', () => {
  it('separates thousands exactly', () => {
    expect(formatCount(0)).toBe('0');
    expect(formatCount(1002)).toBe('1,002');
    expect(formatCount(1_234_567)).toBe('1,234,567');
  });

  it('shortens only once a number stops being readable in full', () => {
    expect(formatCompact(999)).toBe('999');
    expect(formatCompact(9999)).toBe('9,999');
    expect(formatCompact(12_900)).toBe('12.9K');
    expect(formatCompact(4_200_000)).toBe('4.2M');
  });
});

describe('formatElapsed', () => {
  it('renders mm:ss, and h:mm:ss past an hour', () => {
    expect(formatElapsed(0)).toBe('00:00');
    expect(formatElapsed(9)).toBe('00:09');
    expect(formatElapsed(605)).toBe('10:05');
    expect(formatElapsed(3671)).toBe('1:01:11');
  });

  it('survives a missing or negative value', () => {
    expect(formatElapsed(undefined)).toBe('00:00');
    expect(formatElapsed(-5)).toBe('00:00');
  });
});

describe('formatBytes', () => {
  it('uses binary units', () => {
    expect(formatBytes(512)).toBe('512 B');
    expect(formatBytes(45_600)).toBe('44.5 KiB');
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MiB');
    expect(formatBytes(0)).toBe('—');
  });
});
