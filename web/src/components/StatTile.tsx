import type { ReactNode } from 'react';

import { cn } from '../lib/cn';
import { useTweenedNumber } from '../lib/motion';

export interface StatTileProps {
  label: string;
  /**
   * The figure, as a number so it can be eased between updates. Null renders
   * as an em dash — a metric with no observations is not zero.
   */
  value: number | null;
  /** Renders the value. Called on every animation frame, so keep it cheap. */
  format: (value: number) => string;
  /** A secondary figure under the value: a total, a cap, a denominator. */
  detail?: string;
  /** Tints the value only when the number itself carries a warning. */
  tone?: 'default' | 'good' | 'warning' | 'critical';
  /** A swatch tying this tile to the series of the same colour. */
  accent?: string;
  icon?: ReactNode;
  /** Ease between values. The caller honours the motion preference. */
  animate?: boolean;
}

const TONE_TEXT: Record<NonNullable<StatTileProps['tone']>, string> = {
  default: 'text-ink',
  good: 'text-good',
  warning: 'text-ink',
  critical: 'text-critical',
};

/**
 * One headline figure.
 *
 * A single number is a stat tile, never a one-bar chart. The value uses the
 * font's proportional figures — `tabular-nums` gives every digit the width of
 * a zero, which makes a number like 121 look gappy at this size — while the
 * smaller detail line, which sits in a column with its neighbours, does use
 * tabular figures.
 */
export function StatTile({
  label,
  value,
  format,
  detail,
  tone = 'default',
  accent,
  icon,
  animate = true,
}: StatTileProps) {
  // Values change once a second. Snapping between them reads as the page
  // reloading; sliding reads as a live measurement.
  const eased = useTweenedNumber(value, animate);

  return (
    <div className="border-line bg-surface flex flex-col gap-1 rounded-lg border px-4 py-3">
      <div className="flex items-center gap-1.5">
        {accent ? (
          <span
            aria-hidden="true"
            className="inline-block size-2 shrink-0 rounded-full"
            style={{ background: accent }}
          />
        ) : null}
        <span className="text-ink-3 text-xs font-medium tracking-wide uppercase">{label}</span>
        {icon}
      </div>

      <span className={cn('text-2xl leading-tight font-semibold', TONE_TEXT[tone])}>
        {eased === null ? '—' : format(eased)}
      </span>

      {detail ? <span className="tnum text-ink-3 text-xs">{detail}</span> : null}
    </div>
  );
}
