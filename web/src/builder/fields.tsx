import type { ReactNode } from 'react';
import { useId } from 'react';

import { cn } from '../lib/cn';
import { Button } from '../components/ui';
import { newPair, type Pair } from './model';

/** A labelled control with optional help text. */
export function Field({
  label,
  hint,
  children,
  className,
}: {
  label: string;
  hint?: string | undefined;
  children: (id: string) => ReactNode;
  className?: string | undefined;
}) {
  const id = useId();
  return (
    <div className={cn('flex flex-col gap-1', className)}>
      <label htmlFor={id} className="text-ink-2 text-xs font-medium">
        {label}
      </label>
      {children(id)}
      {hint ? <p className="text-ink-3 text-xs">{hint}</p> : null}
    </div>
  );
}

const INPUT =
  'border-line bg-page text-ink w-full rounded-md border px-2 py-1.5 text-sm placeholder:text-ink-3';

export function TextField({
  label,
  value,
  onChange,
  placeholder,
  hint,
  mono = false,
  className,
}: {
  label: string;
  value: string;
  onChange: (next: string) => void;
  placeholder?: string | undefined;
  hint?: string | undefined;
  mono?: boolean;
  className?: string | undefined;
}) {
  return (
    <Field label={label} hint={hint} className={className}>
      {(id) => (
        <input
          id={id}
          type="text"
          value={value}
          placeholder={placeholder}
          spellCheck={false}
          onChange={(event) => onChange(event.target.value)}
          className={cn(INPUT, mono && 'font-mono text-xs')}
        />
      )}
    </Field>
  );
}

export function SelectField<T extends string>({
  label,
  value,
  options,
  onChange,
  hint,
  className,
}: {
  label: string;
  value: T;
  options: readonly T[];
  onChange: (next: T) => void;
  hint?: string | undefined;
  className?: string | undefined;
}) {
  return (
    <Field label={label} hint={hint} className={className}>
      {(id) => (
        <select
          id={id}
          value={value}
          onChange={(event) => onChange(event.target.value as T)}
          className={INPUT}
        >
          {options.map((option) => (
            <option key={option} value={option}>
              {option}
            </option>
          ))}
        </select>
      )}
    </Field>
  );
}

export function Toggle({
  label,
  checked,
  onChange,
  hint,
}: {
  label: string;
  checked: boolean;
  onChange: (next: boolean) => void;
  hint?: string | undefined;
}) {
  const id = useId();
  return (
    <div className="flex flex-col gap-0.5">
      <label htmlFor={id} className="flex items-center gap-2 text-sm">
        <input
          id={id}
          type="checkbox"
          checked={checked}
          onChange={(event) => onChange(event.target.checked)}
          className="accent-accent size-3.5"
        />
        <span className="text-ink-2">{label}</span>
      </label>
      {hint ? <p className="text-ink-3 pl-6 text-xs">{hint}</p> : null}
    </div>
  );
}

/** A collapsible group of fields. */
export function Section({
  title,
  hint,
  children,
  action,
  open = true,
}: {
  title: string;
  hint?: string | undefined;
  children: ReactNode;
  action?: ReactNode | undefined;
  open?: boolean;
}) {
  return (
    <details open={open} className="border-line bg-surface rounded-lg border">
      <summary className="flex cursor-pointer items-center justify-between gap-2 px-3 py-2">
        <span className="text-sm font-semibold">{title}</span>
        {action ?? (hint ? <span className="text-ink-3 text-xs">{hint}</span> : null)}
      </summary>
      <div className="border-line flex flex-col gap-3 border-t px-3 py-3">{children}</div>
    </details>
  );
}

/**
 * Editable key/value rows.
 *
 * There is deliberately no "add" button beside every row: rows are appended at
 * the end and removed individually, which keeps the order stable while
 * somebody is typing into one of them.
 */
export function PairRows({
  label,
  pairs,
  onChange,
  keyPlaceholder = 'name',
  valuePlaceholder = 'value',
  hint,
  addLabel = 'Add',
}: {
  label: string;
  pairs: Pair[];
  onChange: (next: Pair[]) => void;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
  hint?: string | undefined;
  addLabel?: string;
}) {
  const update = (id: string, patch: Partial<Pair>) =>
    onChange(pairs.map((pair) => (pair.id === id ? { ...pair, ...patch } : pair)));

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between">
        <span className="text-ink-2 text-xs font-medium">{label}</span>
        <Button variant="ghost" onClick={() => onChange([...pairs, newPair()])}>
          {addLabel}
        </Button>
      </div>

      {pairs.length === 0 ? (
        <p className="text-ink-3 text-xs">{hint ?? 'None.'}</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {pairs.map((pair) => (
            <li key={pair.id} className="flex items-center gap-1.5">
              <input
                type="text"
                value={pair.key}
                placeholder={keyPlaceholder}
                spellCheck={false}
                onChange={(event) => update(pair.id, { key: event.target.value })}
                className={cn(INPUT, 'font-mono text-xs')}
                aria-label={`${label} name`}
              />
              <input
                type="text"
                value={pair.value}
                placeholder={valuePlaceholder}
                spellCheck={false}
                onChange={(event) => update(pair.id, { value: event.target.value })}
                className={cn(INPUT, 'font-mono text-xs')}
                aria-label={`${label} value`}
              />
              <RemoveButton
                label={`Remove ${label.toLowerCase()} ${pair.key || 'row'}`}
                onClick={() => onChange(pairs.filter((other) => other.id !== pair.id))}
              />
            </li>
          ))}
        </ul>
      )}
      {pairs.length > 0 && hint ? <p className="text-ink-3 text-xs">{hint}</p> : null}
    </div>
  );
}

/** A compact remove control, always with an accessible name. */
export function RemoveButton({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className="text-ink-3 hover:text-critical shrink-0 rounded px-1.5 py-1 text-sm"
    >
      ✕
    </button>
  );
}
