/** Presentation helpers. Kept in one place so the dashboard, the tooltips and
 *  the tables all render a duration or a rate the same way. */

const counts = new Intl.NumberFormat(undefined);

/** A whole number with thousands separators. */
export function formatCount(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '—';
  return counts.format(Math.round(n));
}

/** A large number shortened for a stat tile: 1,284 · 12.9K · 4.2M. */
export function formatCompact(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '—';
  if (Math.abs(n) < 10_000) return counts.format(Math.round(n));
  if (Math.abs(n) < 1_000_000) return `${(n / 1_000).toFixed(1)}K`;
  if (Math.abs(n) < 1_000_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  return `${(n / 1_000_000_000).toFixed(1)}B`;
}

/** A millisecond figure, with the unit that keeps it readable. */
export function formatMillis(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms) || ms <= 0) return '—';
  if (ms < 1) return `${Math.round(ms * 1000)}µs`;
  if (ms < 1000) return `${ms < 10 ? ms.toFixed(1) : Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(2)}s`;
}

/** Requests per second. */
export function formatRate(rps: number | null | undefined): string {
  if (rps === null || rps === undefined || !Number.isFinite(rps) || rps <= 0) return '0';
  if (rps < 10) return rps.toFixed(1);
  if (rps < 10_000) return Math.round(rps).toString();
  return `${(rps / 1000).toFixed(1)}k`;
}

/** A ratio rendered as a percentage. */
export function formatPercent(ratio: number | null | undefined, digits = 2): string {
  if (ratio === null || ratio === undefined || !Number.isFinite(ratio)) return '—';
  if (ratio === 0) return '0%';
  if (ratio < 0.0001) return '<0.01%';
  return `${(ratio * 100).toFixed(digits)}%`;
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'] as const;

/** A byte count in binary units. */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes) || bytes <= 0) return '—';
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${unit === 0 ? Math.round(value) : value.toFixed(1)} ${BYTE_UNITS[unit]}`;
}

/** An elapsed duration as mm:ss, or h:mm:ss past an hour. */
export function formatElapsed(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds < 0) {
    return '00:00';
  }
  const total = Math.floor(seconds);
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600);
  const pad = (n: number) => n.toString().padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}

/** A wall-clock time for an axis tick or a log line. */
export function formatTime(epochMillis: number): string {
  return new Date(epochMillis).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
}

/** How long ago an ISO timestamp was, in words. */
export function formatAgo(iso: string | undefined): string {
  if (!iso) return '—';
  const then = new Date(iso).getTime();
  if (!Number.isFinite(then)) return '—';

  const seconds = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (seconds < 5) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  return `${Math.floor(seconds / 3600)}h ago`;
}
