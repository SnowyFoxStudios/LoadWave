import { useEffect, useId, useMemo, useRef, useState } from 'react';

import { startRun, validateConfig, type ValidateResult } from '../api/client';
import { Builder } from '../builder/Builder';
import { findProblems, newDraft, toConfig, type Draft } from '../builder/model';
import { cn } from '../lib/cn';
import { toYaml } from '../lib/yaml';
import { Button } from './ui';

type Mode = 'build' | 'yaml';

/** How long to wait after the last edit before revalidating.
 *
 *  Long enough that typing a URL does not fire a request per character, short
 *  enough that the panel feels like it is keeping up. */
const VALIDATE_DEBOUNCE_MS = 400;

/**
 * The scenario builder.
 *
 * Two ways in, and the form is the primary one: most people writing a first
 * load test do not know the schema, and a blank textarea is a poor place to
 * discover it. The YAML is never hidden though — it is generated beside the
 * form, always visible, and copyable, because the output is meant to end up in
 * a repository rather than only in this dialog.
 *
 * Validation is done by the server rather than reimplemented here. It is the
 * only thing that knows whether a scenario name matches something compiled
 * into this binary, and its parser messages carry the offending line and field.
 */
export function StartRunDialog({
  open,
  onClose,
  onStarted,
}: {
  open: boolean;
  onClose: () => void;
  onStarted: (runId: string) => void;
}) {
  const [mode, setMode] = useState<Mode>('build');
  const [draft, setDraft] = useState<Draft>(newDraft);
  const [raw, setRaw] = useState('');
  // The result is stored together with the configuration it was computed for,
  // so "out of date" is derived rather than cleared. Clearing it would mean a
  // setState on every keystroke, cascading a render before the debounce has
  // even started.
  const [checked, setChecked] = useState<{ config: string; result: ValidateResult } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [copied, setCopied] = useState(false);

  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const labelId = useId();

  // The form's output. Derived, never stored, so the two cannot drift apart.
  const built = useMemo(() => toYaml(toConfig(draft)), [draft]);
  const problems = useMemo(() => (mode === 'build' ? findProblems(draft) : []), [draft, mode]);

  // What would actually be submitted.
  const config = mode === 'build' ? built : raw;

  // <dialog> is driven imperatively; showModal is what gives us the backdrop,
  // the focus trap and Escape-to-close for free.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open && !dialog.open) dialog.showModal();
    if (!open && dialog.open) dialog.close();
  }, [open]);

  const result = checked?.config === config ? checked.result : null;

  // Local problems come first: a malformed JSON body cannot even be turned
  // into YAML, so sending it would get a confusing answer back.
  const wantsCheck = open && config.trim() !== '' && problems.length === 0;
  const checking = wantsCheck && result === null;

  // Validate against the real parser, debounced.
  useEffect(() => {
    if (!wantsCheck || result !== null) return;

    const timer = setTimeout(() => {
      validateConfig(config)
        .then((next) => setChecked({ config, result: next }))
        .catch((err: unknown) =>
          setChecked({
            config,
            result: { valid: false, error: err instanceof Error ? err.message : String(err) },
          }),
        );
    }, VALIDATE_DEBOUNCE_MS);

    return () => clearTimeout(timer);
  }, [config, wantsCheck, result]);

  const blocked = problems.length > 0 || result?.valid !== true;

  const submit = () => {
    setSubmitting(true);
    setError(null);
    startRun(config)
      .then((started) => {
        onStarted(started.runId);
        onClose();
      })
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setSubmitting(false));
  };

  const copy = () => {
    void navigator.clipboard?.writeText(config).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={labelId}
      onClose={onClose}
      className="border-line bg-surface text-ink m-auto flex h-[min(90vh,56rem)] w-[min(84rem,95vw)] flex-col rounded-lg border p-0 backdrop:bg-black/50"
    >
      <header className="border-line flex flex-wrap items-center justify-between gap-2 border-b px-4 py-2.5">
        <h2 id={labelId} className="text-sm font-semibold">
          New run
        </h2>

        <div className="flex items-center gap-2">
          <div className="border-line inline-flex overflow-hidden rounded-md border">
            {(['build', 'yaml'] as const).map((option) => (
              <button
                key={option}
                type="button"
                aria-pressed={mode === option}
                onClick={() => {
                  // Moving to YAML carries the form's output across, so the
                  // form is a starting point you can hand-tune rather than a
                  // separate thing you have to abandon.
                  if (option === 'yaml' && mode === 'build') setRaw(built);
                  setMode(option);
                }}
                className={cn(
                  'px-3 py-1 text-xs font-medium',
                  mode === option ? 'bg-accent-soft text-accent' : 'text-ink-2 hover:bg-surface-2',
                )}
              >
                {option === 'build' ? 'Build' : 'Edit YAML'}
              </button>
            ))}
          </div>
          <Button variant="ghost" onClick={onClose} aria-label="Close">
            ✕
          </Button>
        </div>
      </header>

      <div className="grid min-h-0 flex-1 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
        <div className="min-h-0 overflow-y-auto p-3">
          {mode === 'build' ? (
            <Builder draft={draft} onChange={setDraft} />
          ) : (
            <label className="flex h-full flex-col gap-1.5">
              <span className="text-ink-3 text-xs">
                The same YAML you would pass to <code className="font-mono">loadwave run</code>.
                Switching back to Build discards edits made here.
              </span>
              <textarea
                value={raw}
                onChange={(event) => setRaw(event.target.value)}
                spellCheck={false}
                placeholder={built}
                className="border-line bg-page text-ink min-h-0 flex-1 resize-none rounded-md border px-3 py-2 font-mono text-xs leading-relaxed"
              />
            </label>
          )}
        </div>

        <aside className="border-line flex min-h-0 flex-col border-t lg:border-t-0 lg:border-l">
          <div className="border-line flex items-center justify-between gap-2 border-b px-3 py-2">
            <span className="text-xs font-semibold">
              {mode === 'build' ? 'Generated YAML' : 'Checks'}
            </span>
            <Button variant="ghost" onClick={copy} disabled={config.trim() === ''}>
              {copied ? 'Copied' : 'Copy'}
            </Button>
          </div>

          {mode === 'build' ? (
            <pre className="text-ink-2 min-h-0 flex-1 overflow-auto px-3 py-2 font-mono text-xs leading-relaxed">
              {built || '# Fill in the form to the left.'}
            </pre>
          ) : (
            <div className="min-h-0 flex-1 overflow-auto px-3 py-2">
              <p className="text-ink-3 text-xs">
                Editing YAML directly. The checks below come from the same parser the runner uses.
              </p>
            </div>
          )}

          <ValidationPanel
            checking={checking}
            problems={problems}
            result={result}
            empty={config.trim() === ''}
          />
        </aside>
      </div>

      <footer className="border-line flex flex-wrap items-center justify-between gap-2 border-t px-4 py-3">
        <span className="text-ink-3 text-xs">
          {result?.valid && result.summary
            ? `${result.summary.profile} · peak ${result.summary.peakVUs} VUs`
            : 'The run starts once the configuration checks out.'}
        </span>
        <span className="flex items-center gap-2">
          {error ? (
            <span role="alert" className="text-critical text-xs">
              {error}
            </span>
          ) : null}
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={submit} disabled={submitting || blocked}>
            {submitting ? 'Starting…' : 'Start run'}
          </Button>
        </span>
      </footer>
    </dialog>
  );
}

/**
 * What the server made of the configuration.
 *
 * Echoing the profile back in words — "30s to 100 VUs, then 5m0s to 100 VUs" —
 * is a far stronger confirmation than a green tick: it proves the runner read
 * the form the same way the person filling it in did.
 */
function ValidationPanel({
  checking,
  problems,
  result,
  empty,
}: {
  checking: boolean;
  problems: { where: string; message: string }[];
  result: ValidateResult | null;
  empty: boolean;
}) {
  if (empty) {
    return (
      <div className="border-line text-ink-3 border-t px-3 py-2 text-xs">Nothing to check yet.</div>
    );
  }

  if (problems.length > 0) {
    return (
      <div className="border-line border-t px-3 py-2">
        <p className="text-critical text-xs font-semibold">
          {problems.length === 1 ? '1 problem' : `${problems.length} problems`}
        </p>
        <ul className="mt-1 flex flex-col gap-0.5">
          {problems.map((problem, index) => (
            <li key={index} className="text-ink-2 text-xs">
              <span className="text-ink-3">{problem.where}:</span> {problem.message}
            </li>
          ))}
        </ul>
      </div>
    );
  }

  if (checking || !result) {
    return <div className="border-line text-ink-3 border-t px-3 py-2 text-xs">Checking…</div>;
  }

  if (!result.valid) {
    return (
      <div className="border-line border-t px-3 py-2">
        <p className="text-critical text-xs font-semibold">Rejected</p>
        {/* The parser's own message, verbatim: it names the line and the field,
            which is more use than anything reworded here. */}
        <pre className="text-ink-2 mt-1 overflow-auto font-mono text-xs whitespace-pre-wrap">
          {result.error}
        </pre>
      </div>
    );
  }

  const summary = result.summary;
  if (!summary) return null;

  return (
    <div className="border-line border-t px-3 py-2">
      <p className="text-good text-xs font-semibold">Valid</p>
      <dl className="mt-1 grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 text-xs">
        <dt className="text-ink-3">Profile</dt>
        <dd className="text-ink-2">{summary.profile}</dd>
        <dt className="text-ink-3">Peak VUs</dt>
        <dd className="text-ink-2 tnum">{summary.peakVUs}</dd>
        {summary.durationSeconds > 0 ? (
          <>
            <dt className="text-ink-3">Duration</dt>
            <dd className="text-ink-2 tnum">{summary.durationSeconds}s</dd>
          </>
        ) : null}
        <dt className="text-ink-3">Pacing</dt>
        <dd className="text-ink-2">
          {summary.betweenRequests === '0s' ? 'none — flat out' : summary.betweenRequests}
          {summary.pacingDefaulted ? ' (default)' : ''}
        </dd>
        <dt className="text-ink-3">Scenarios</dt>
        <dd className="text-ink-2">
          {summary.scenarios
            .map((s) => `${s.name} ×${s.weight}${s.source === 'go' ? ' (Go)' : ''}`)
            .join(', ')}
        </dd>
        {summary.thresholds?.length ? (
          <>
            <dt className="text-ink-3">Thresholds</dt>
            <dd className="text-ink-2 font-mono">{summary.thresholds.join(' · ')}</dd>
          </>
        ) : null}
      </dl>
      {summary.thresholds?.length ? null : (
        <p className="text-warning mt-1.5 text-xs">
          No thresholds: this run will pass whatever the results are.
        </p>
      )}
    </div>
  );
}
