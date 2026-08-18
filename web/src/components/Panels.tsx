import { useState } from 'react';

import type {
  AgentInfo,
  EndpointSummary,
  FailureSummary,
  RunEvent,
  StoreStats,
  ThresholdResult,
} from '../api/types';
import {
  formatAgo,
  formatBytes,
  formatCores,
  formatCount,
  formatMillis,
  formatPercent,
} from '../lib/format';
import { cn } from '../lib/cn';
import { Badge, Empty, Panel } from './ui';

/** Renders a threshold's observed value at a sensible precision. */
function formatActual(result: ThresholdResult): string {
  if (!result.evaluated) return '—';
  if (result.stat === 'rate') return formatPercent(result.actual, 3);
  if (result.stat === 'count') return formatCount(result.actual);
  return formatMillis(result.actual);
}

export function Thresholds({ results }: { results: ThresholdResult[] }) {
  if (results.length === 0) {
    return (
      <Panel title="Thresholds">
        <Empty>
          No thresholds are defined. Add them to the configuration to make this run a pass/fail
          gate.
        </Empty>
      </Panel>
    );
  }

  const failing = results.filter((r) => r.evaluated && !r.passed).length;

  return (
    <Panel
      title="Thresholds"
      action={
        failing > 0 ? (
          <Badge tone="critical" dot>
            {failing} failing
          </Badge>
        ) : (
          <Badge tone="good" dot>
            all passing
          </Badge>
        )
      }
    >
      <ul className="divide-line divide-y">
        {results.map((result) => (
          <li
            key={result.description}
            className="flex items-center justify-between gap-3 px-4 py-2.5"
          >
            <div className="min-w-0">
              <p className="text-ink truncate font-mono text-xs">{result.description}</p>
              {!result.evaluated ? (
                <p className="text-ink-3 mt-0.5 text-xs">
                  never measured — this metric has not been produced
                </p>
              ) : null}
            </div>
            <div className="flex shrink-0 items-center gap-3">
              <span className="tnum text-ink-2 text-xs">{formatActual(result)}</span>
              {/* The word carries the verdict; the colour only reinforces it. */}
              {!result.evaluated ? (
                <Badge tone="neutral">skipped</Badge>
              ) : result.passed ? (
                <Badge tone="good">pass</Badge>
              ) : (
                <Badge tone="critical">fail</Badge>
              )}
            </div>
          </li>
        ))}
      </ul>
    </Panel>
  );
}

type SortKey = 'name' | 'requests' | 'avg' | 'p95' | 'p99' | 'errorRate';

/**
 * The per-endpoint breakdown.
 *
 * This is also the table view the colour guidance requires: every figure the
 * charts encode with hue is readable here as a number, so nothing in the
 * dashboard depends on distinguishing two colours.
 */
export function EndpointTable({
  endpoints,
  selected,
  onSelect,
}: {
  endpoints: EndpointSummary[];
  /** The endpoint the response-time chart is isolated to, if any. */
  selected?: string | null;
  /** Called with a row's name when it is clicked. */
  onSelect?: (name: string) => void;
}) {
  const [sort, setSort] = useState<SortKey>('p95');
  const [ascending, setAscending] = useState(false);

  if (endpoints.length === 0) {
    return (
      <Panel title="Requests">
        <Empty>No requests have been recorded yet.</Empty>
      </Panel>
    );
  }

  const sorted = [...endpoints].sort((a, b) => {
    const pick = (row: EndpointSummary): number | string => {
      switch (sort) {
        case 'name':
          return row.name;
        case 'requests':
          return row.requests;
        case 'avg':
          return row.avg;
        case 'p99':
          return row.percentiles?.p99 ?? 0;
        case 'errorRate':
          return row.errorRate;
        default:
          return row.percentiles?.p95 ?? 0;
      }
    };
    const left = pick(a);
    const right = pick(b);
    const order =
      typeof left === 'string' && typeof right === 'string'
        ? left.localeCompare(right)
        : Number(left) - Number(right);
    return ascending ? order : -order;
  });

  const header = (key: SortKey, label: string, align: 'left' | 'right' = 'right') => (
    <th
      scope="col"
      className={cn(
        'text-ink-3 px-3 py-2 text-xs font-medium',
        align === 'left' ? 'text-left' : 'text-right',
      )}
    >
      <button
        type="button"
        className="hover:text-ink"
        onClick={() => {
          if (sort === key) setAscending((prev) => !prev);
          else {
            setSort(key);
            setAscending(key === 'name');
          }
        }}
        aria-sort={sort === key ? (ascending ? 'ascending' : 'descending') : 'none'}
      >
        {label}
        {sort === key ? <span aria-hidden="true">{ascending ? ' ↑' : ' ↓'}</span> : null}
      </button>
    </th>
  );

  return (
    <Panel
      title="Requests"
      action={
        <span className="text-ink-3 text-xs">
          {onSelect ? 'click a request name to chart it alone' : 'by name'}
        </span>
      }
    >
      <div className="overflow-x-auto">
        <table className="w-full min-w-[52rem] border-collapse text-sm">
          <thead className="border-line border-b">
            <tr>
              {header('name', 'Request', 'left')}
              {header('requests', 'Count')}
              {header('avg', 'Avg')}
              {header('p95', 'p95')}
              {header('p99', 'p99')}
              <th scope="col" className="text-ink-3 px-3 py-2 text-right text-xs font-medium">
                Max
              </th>
              {header('errorRate', 'Errors')}
              <th scope="col" className="text-ink-3 px-3 py-2 text-right text-xs font-medium">
                Received
              </th>
            </tr>
          </thead>
          <tbody className="divide-line divide-y">
            {sorted.map((row) => (
              <tr
                key={row.name}
                aria-selected={selected === row.name}
                className={cn(selected === row.name ? 'bg-accent-soft' : 'hover:bg-surface-2')}
              >
                <th scope="row" className="max-w-[22rem] truncate px-3 py-2 text-left font-normal">
                  {onSelect ? (
                    <button
                      type="button"
                      onClick={() => onSelect(row.name)}
                      className="text-ink hover:text-accent cursor-pointer font-mono text-xs"
                      title={
                        selected === row.name
                          ? 'Show every endpoint on the chart again'
                          : `Chart only ${row.name}`
                      }
                    >
                      {row.name}
                    </button>
                  ) : (
                    <span className="text-ink font-mono text-xs">{row.name}</span>
                  )}
                </th>
                <td className="tnum text-ink-2 px-3 py-2 text-right">
                  {formatCount(row.requests)}
                </td>
                <td className="tnum text-ink-2 px-3 py-2 text-right">{formatMillis(row.avg)}</td>
                <td className="tnum text-ink px-3 py-2 text-right font-medium">
                  {formatMillis(row.percentiles?.p95)}
                </td>
                <td className="tnum text-ink-2 px-3 py-2 text-right">
                  {formatMillis(row.percentiles?.p99)}
                </td>
                <td className="tnum text-ink-2 px-3 py-2 text-right">{formatMillis(row.max)}</td>
                <td
                  className={cn(
                    'tnum px-3 py-2 text-right',
                    row.errorRate > 0 ? 'text-critical font-medium' : 'text-ink-2',
                  )}
                >
                  {formatPercent(row.errorRate)}
                </td>
                <td className="tnum text-ink-3 px-3 py-2 text-right">{formatBytes(row.bytesIn)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Panel>
  );
}

/** Renders the status column: an HTTP code, or the transport failure class. */
function failureCode(failure: FailureSummary): string {
  if (failure.status > 0) return String(failure.status);
  return failure.errorClass ? failure.errorClass.replace(/_/g, ' ') : 'no response';
}

/** A failure with no response at all is a different kind of problem from a
 *  server that answered with an error, and reads differently. */
function failureTone(failure: FailureSummary): 'critical' | 'warning' {
  if (failure.status >= 400 && failure.status < 500) return 'warning';
  return 'critical';
}

/**
 * What went wrong, and why.
 *
 * The metrics can say that 3% of requests failed; only this can say that they
 * were all `502 payment declined` on one endpoint. Rows are aggregated by
 * status, endpoint and transport error class, with a short excerpt of what the
 * server actually said — captured the first time each kind was seen.
 */
export function Failures({ failures, stats }: { failures: FailureSummary[]; stats?: StoreStats }) {
  if (failures.length === 0) {
    return (
      <Panel title="Failed requests">
        <Empty>No failures recorded. Every request has been judged successful.</Empty>
      </Panel>
    );
  }

  const total = failures.reduce((sum, failure) => sum + failure.count, 0);

  return (
    <Panel
      title="Failed requests"
      action={
        <span className="tnum text-ink-3 text-xs">
          {formatCount(total)} across {failures.length} {failures.length === 1 ? 'kind' : 'kinds'}
        </span>
      }
    >
      <div className="overflow-x-auto">
        <table className="w-full min-w-[52rem] border-collapse text-sm">
          <thead className="border-line border-b">
            <tr>
              <th scope="col" className="text-ink-3 px-3 py-2 text-left text-xs font-medium">
                Request
              </th>
              <th scope="col" className="text-ink-3 px-3 py-2 text-left text-xs font-medium">
                Code
              </th>
              <th scope="col" className="text-ink-3 px-3 py-2 text-right text-xs font-medium">
                Count
              </th>
              <th scope="col" className="text-ink-3 px-3 py-2 text-left text-xs font-medium">
                Message
              </th>
              <th scope="col" className="text-ink-3 px-3 py-2 text-right text-xs font-medium">
                Last seen
              </th>
            </tr>
          </thead>
          <tbody className="divide-line divide-y">
            {failures.map((failure) => (
              <tr
                key={`${failure.method}|${failure.name}|${failure.status}|${failure.errorClass ?? ''}`}
                className="hover:bg-surface-2"
              >
                <th scope="row" className="max-w-[18rem] truncate px-3 py-2 text-left font-normal">
                  <span className="text-ink font-mono text-xs">{failure.name}</span>
                </th>
                <td className="px-3 py-2">
                  {/* The code is spelled out, so the colour only reinforces it. */}
                  <Badge tone={failureTone(failure)}>{failureCode(failure)}</Badge>
                </td>
                <td className="tnum text-ink px-3 py-2 text-right font-medium">
                  {formatCount(failure.count)}
                </td>
                <td className="text-ink-2 max-w-[28rem] px-3 py-2">
                  {failure.message ? (
                    <span className="line-clamp-2 font-mono text-xs break-words">
                      {failure.message}
                    </span>
                  ) : (
                    <span className="text-ink-3 text-xs">—</span>
                  )}
                </td>
                <td className="tnum text-ink-3 px-3 py-2 text-right text-xs">
                  {formatAgo(failure.lastSeen)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {stats && stats.droppedFailureKinds > 0 ? (
        <p className="border-line text-warning border-t px-4 py-2 text-xs">
          {formatCount(stats.droppedFailureKinds)} further kinds of failure were not recorded — the
          run hit its cap. The counts above are complete for the kinds shown.
        </p>
      ) : null}
    </Panel>
  );
}

export function Agents({ agents }: { agents: AgentInfo[] }) {
  if (agents.length === 0) {
    return (
      <Panel title="Agents">
        <Empty>
          No agents are connected. Start one with <code className="font-mono">loadwave agent</code>.
        </Empty>
      </Panel>
    );
  }

  const totalVUs = agents.reduce((sum, agent) => sum + agent.activeVUs, 0);

  return (
    <Panel
      title="Agents"
      action={
        <span className="tnum text-ink-3 text-xs">
          {agents.length} connected · {formatCount(totalVUs)} VUs
        </span>
      }
    >
      <ul className="divide-line divide-y">
        {agents.map((agent) => (
          <li key={agent.id} className="px-4 py-2.5">
            <div className="flex items-center justify-between gap-3">
              <div className="min-w-0">
                <p className="text-ink truncate text-sm font-medium">{agent.id}</p>
                <p className="text-ink-3 truncate text-xs">
                  {agent.hostname} · {agent.cores} cores · {agent.healthyWorkers} workers ·{' '}
                  {agent.remoteAddr}
                </p>
              </div>
              <div className="flex shrink-0 items-center gap-2">
                <span className="tnum text-ink-2 text-xs">
                  {formatCount(agent.activeVUs)}
                  <span className="text-ink-3"> / {formatCount(agent.maxVUs)} VUs</span>
                </span>
                <Badge tone={agent.healthy ? 'good' : 'critical'} dot>
                  {agent.healthy ? 'healthy' : 'unreachable'}
                </Badge>
              </div>
            </div>
            {agent.labels && Object.keys(agent.labels).length > 0 ? (
              <ul className="mt-1.5 flex flex-wrap gap-1">
                {Object.entries(agent.labels).map(([key, value]) => (
                  <li
                    key={key}
                    className="bg-surface-2 text-ink-3 rounded px-1.5 py-0.5 font-mono text-[11px]"
                  >
                    {key}={value}
                  </li>
                ))}
              </ul>
            ) : null}

            {agent.workers.length > 0 ? (
              <ul className="border-line mt-2 flex flex-col gap-1 border-t pt-2">
                {agent.workers.map((worker) => (
                  <li key={worker.id} className="flex items-center justify-between gap-3 text-xs">
                    <span className="text-ink-3 min-w-0 truncate font-mono">{worker.id}</span>
                    <span className="tnum text-ink-2 flex shrink-0 items-center gap-3">
                      <span title="Virtual users">{formatCount(worker.activeVUs)} VUs</span>
                      <span title="CPU, as a share of one core">
                        {formatCores(worker.cpuPercent)}
                      </span>
                      <span title="Resident memory">{formatBytes(worker.memBytes)}</span>
                    </span>
                  </li>
                ))}
              </ul>
            ) : null}
          </li>
        ))}
      </ul>
    </Panel>
  );
}

const LEVEL_TONE: Record<RunEvent['level'], string> = {
  debug: 'text-ink-3',
  info: 'text-ink-2',
  warn: 'text-warning',
  error: 'text-critical',
};

export function EventLog({ events }: { events: RunEvent[] }) {
  if (events.length === 0) {
    return (
      <Panel title="Events">
        <Empty>Nothing has happened yet.</Empty>
      </Panel>
    );
  }

  // Newest first: during an incident the latest line is the one being read.
  const newest = [...events].reverse().slice(0, 60);

  return (
    <Panel title="Events" action={<span className="text-ink-3 text-xs">newest first</span>}>
      <ul className="divide-line max-h-80 divide-y overflow-y-auto">
        {newest.map((event, index) => (
          <li key={`${event.time}-${index}`} className="flex gap-3 px-4 py-2 text-xs">
            <span className="tnum text-ink-3 shrink-0">{formatAgo(event.time)}</span>
            <span className={cn('shrink-0 font-medium uppercase', LEVEL_TONE[event.level])}>
              {event.level}
            </span>
            <span className="text-ink-2 min-w-0 flex-1">
              {event.message}
              {event.source ? <span className="text-ink-3"> · {event.source}</span> : null}
            </span>
          </li>
        ))}
      </ul>
    </Panel>
  );
}
