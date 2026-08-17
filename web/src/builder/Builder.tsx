import { Button } from '../components/ui';
import { Field, PairRows, RemoveButton, Section, SelectField, TextField, Toggle } from './fields';
import {
  HTTP_METHODS,
  THRESHOLD_METRICS,
  THRESHOLD_OPS,
  THRESHOLD_STATS,
  methodTakesBody,
  newScenario,
  newStage,
  newStep,
  newThreshold,
  type BodyKind,
  type Draft,
  type Executor,
  type Scenario,
  type Step,
  type Threshold,
} from './model';

/** Replaces one item of a list by id, leaving the rest untouched. */
function replace<T extends { id: string }>(items: T[], id: string, patch: Partial<T>): T[] {
  return items.map((item) => (item.id === id ? { ...item, ...patch } : item));
}

const BODY_KINDS: readonly BodyKind[] = ['none', 'json', 'form', 'raw'];

/**
 * The form half of the scenario builder.
 *
 * Everything here edits the draft and nothing else; producing YAML, validating
 * it and starting the run all happen in the dialog around it. That split is
 * what lets the YAML preview beside the form be derived state rather than
 * something that has to be kept in sync.
 */
export function Builder({ draft, onChange }: { draft: Draft; onChange: (next: Draft) => void }) {
  const patch = (fields: Partial<Draft>) => onChange({ ...draft, ...fields });

  return (
    <div className="flex flex-col gap-3">
      <Section title="Test">
        <div className="grid gap-3 sm:grid-cols-2">
          <TextField
            label="Name"
            value={draft.name}
            onChange={(name) => patch({ name })}
            placeholder="storefront-checkout"
            hint="Shown in the dashboard and in the report."
          />
          <TextField
            label="Base URL"
            value={draft.baseURL}
            onChange={(baseURL) => patch({ baseURL })}
            placeholder="https://staging.example.com"
            hint="Prefixed to every relative path below."
            mono
          />
        </div>
      </Section>

      <Section title="Load" hint={draft.executor}>
        <SelectField
          label="Executor"
          value={draft.executor}
          options={['ramping-vus', 'constant-vus'] as const}
          onChange={(executor: Executor) => patch({ executor })}
          hint={
            draft.executor === 'ramping-vus'
              ? 'Moves between stage targets. Every profile starts from zero.'
              : 'Holds a fixed number of virtual users.'
          }
        />

        {draft.executor === 'constant-vus' ? (
          <div className="grid gap-3 sm:grid-cols-2">
            <TextField
              label="Virtual users"
              value={draft.vus}
              onChange={(vus) => patch({ vus })}
              placeholder="50"
            />
            <TextField
              label="Duration"
              value={draft.duration}
              onChange={(duration) => patch({ duration })}
              placeholder="5m"
            />
          </div>
        ) : (
          <div className="flex flex-col gap-1.5">
            <div className="flex items-center justify-between">
              <span className="text-ink-2 text-xs font-medium">Stages</span>
              <Button
                variant="ghost"
                onClick={() => patch({ stages: [...draft.stages, newStage()] })}
              >
                Add stage
              </Button>
            </div>
            <ul className="flex flex-col gap-1.5">
              {draft.stages.map((stage, index) => (
                <li key={stage.id} className="flex items-end gap-1.5">
                  <TextField
                    label={index === 0 ? 'Over' : ''}
                    value={stage.duration}
                    onChange={(duration) =>
                      patch({ stages: replace(draft.stages, stage.id, { duration }) })
                    }
                    placeholder="30s"
                    className="flex-1"
                  />
                  <TextField
                    label={index === 0 ? 'Reach VUs' : ''}
                    value={stage.target}
                    onChange={(target) =>
                      patch({ stages: replace(draft.stages, stage.id, { target }) })
                    }
                    placeholder="100"
                    className="flex-1"
                  />
                  <RemoveButton
                    label={`Remove stage ${index + 1}`}
                    onClick={() =>
                      patch({ stages: draft.stages.filter((other) => other.id !== stage.id) })
                    }
                  />
                </li>
              ))}
            </ul>
            <p className="text-ink-3 text-xs">
              A ramp up, a hold and a ramp down is the usual shape. The last stage reaching zero is
              what makes the run wind down rather than stop dead.
            </p>
          </div>
        )}

        <div className="grid gap-3 sm:grid-cols-3">
          <TextField
            label="Graceful stop"
            value={draft.gracefulStop}
            onChange={(gracefulStop) => patch({ gracefulStop })}
            placeholder="30s"
            hint="Time for in-flight iterations to finish."
          />
          <TextField
            label="Max iterations / sec"
            value={draft.maxIterationRate}
            onChange={(maxIterationRate) => patch({ maxIterationRate })}
            placeholder="unlimited"
            hint="Caps the arrival rate fleet-wide."
          />
          <TextField
            label="Total iterations"
            value={draft.iterations}
            onChange={(iterations) => patch({ iterations })}
            placeholder="unbounded"
            hint="Stops the run after this many."
          />
        </div>
      </Section>

      <Section title="Pacing" hint={draft.betweenRequests || '1s (default)'}>
        <TextField
          label="Between requests"
          value={draft.betweenRequests}
          onChange={(betweenRequests) => patch({ betweenRequests })}
          placeholder="1s"
          hint={
            'Pause after every request, whatever its outcome — this is what stops a failing ' +
            'endpoint being hammered in a tight loop. Blank uses one second. A range such as ' +
            '"500ms-2s" keeps virtual users from marching in lockstep. Use "0" for a throughput test.'
          }
        />
      </Section>

      <Section
        title="Scenarios"
        action={
          <Button
            variant="ghost"
            onClick={() =>
              onChange({
                ...draft,
                scenarios: [
                  ...draft.scenarios,
                  newScenario(`scenario-${draft.scenarios.length + 1}`),
                ],
              })
            }
          >
            Add scenario
          </Button>
        }
      >
        {draft.scenarios.map((scenario, index) => (
          <ScenarioEditor
            key={scenario.id}
            scenario={scenario}
            index={index}
            removable={draft.scenarios.length > 1}
            onChange={(next) =>
              onChange({ ...draft, scenarios: replace(draft.scenarios, scenario.id, next) })
            }
            onRemove={() =>
              onChange({
                ...draft,
                scenarios: draft.scenarios.filter((other) => other.id !== scenario.id),
              })
            }
          />
        ))}
      </Section>

      <Section
        title="Thresholds"
        action={
          <Button
            variant="ghost"
            onClick={() =>
              onChange({ ...draft, thresholds: [...draft.thresholds, newThreshold()] })
            }
          >
            Add threshold
          </Button>
        }
      >
        {draft.thresholds.length === 0 ? (
          <p className="text-ink-3 text-xs">
            None. Without a threshold the run has no pass or fail criteria, and a CI job cannot tell
            a good result from a bad one.
          </p>
        ) : (
          <ul className="flex flex-col gap-1.5">
            {draft.thresholds.map((threshold, index) => (
              <ThresholdEditor
                key={threshold.id}
                threshold={threshold}
                index={index}
                onChange={(next) =>
                  onChange({
                    ...draft,
                    thresholds: replace(draft.thresholds, threshold.id, next),
                  })
                }
                onRemove={() =>
                  onChange({
                    ...draft,
                    thresholds: draft.thresholds.filter((other) => other.id !== threshold.id),
                  })
                }
              />
            ))}
          </ul>
        )}
        <p className="text-ink-3 text-xs">
          Durations are in milliseconds; rates are fractions of one. A breach exits 2.
        </p>
      </Section>

      <Section title="Advanced" open={false} hint="HTTP, workers, tags">
        <div className="grid gap-3 sm:grid-cols-2">
          <TextField
            label="Request timeout"
            value={draft.timeout}
            onChange={(timeout) => patch({ timeout })}
            placeholder="30s"
          />
          <TextField
            label="Workers per agent"
            value={draft.workersPerAgent}
            onChange={(workersPerAgent) => patch({ workersPerAgent })}
            placeholder="one per core, less one"
            hint="Worker processes, not threads."
          />
        </div>

        <Toggle
          label="Follow redirects"
          checked={draft.followRedirects}
          onChange={(followRedirects) => patch({ followRedirects })}
          hint="Off by default: a load test usually wants to measure the redirect itself."
        />
        <Toggle
          label="Skip TLS verification"
          checked={draft.insecureSkipTLSVerify}
          onChange={(insecureSkipTLSVerify) => patch({ insecureSkipTLSVerify })}
          hint="For environments with self-signed certificates."
        />

        <PairRows
          label="Headers on every request"
          pairs={draft.headers}
          onChange={(headers) => patch({ headers })}
          keyPlaceholder="Authorization"
          valuePlaceholder="Bearer …"
          addLabel="Add header"
        />
        <PairRows
          label="Tags on every metric"
          pairs={draft.tags}
          onChange={(tags) => patch({ tags })}
          keyPlaceholder="env"
          valuePlaceholder="staging"
          hint="Keep the values to a small fixed set: each combination is a time series."
          addLabel="Add tag"
        />
      </Section>
    </div>
  );
}

function ScenarioEditor({
  scenario,
  index,
  removable,
  onChange,
  onRemove,
}: {
  scenario: Scenario;
  index: number;
  removable: boolean;
  onChange: (patch: Partial<Scenario>) => void;
  onRemove: () => void;
}) {
  return (
    <div className="border-line bg-page flex flex-col gap-3 rounded-md border p-3">
      <div className="flex items-end gap-1.5">
        <TextField
          label="Scenario name"
          value={scenario.name}
          onChange={(name) => onChange({ name })}
          placeholder="browse"
          className="flex-1"
        />
        <TextField
          label="Weight"
          value={scenario.weight}
          onChange={(weight) => onChange({ weight })}
          placeholder="1"
          className="w-20"
        />
        {removable ? (
          <RemoveButton label={`Remove scenario ${index + 1}`} onClick={onRemove} />
        ) : null}
      </div>

      <TextField
        label="Description"
        value={scenario.description}
        onChange={(description) => onChange({ description })}
        placeholder="What this simulated user is doing"
      />

      <div className="flex items-center justify-between">
        <span className="text-ink-2 text-xs font-medium">Steps</span>
        <span className="flex gap-1">
          <Button
            variant="ghost"
            onClick={() => onChange({ steps: [...scenario.steps, newStep('request')] })}
          >
            Add request
          </Button>
          <Button
            variant="ghost"
            onClick={() => onChange({ steps: [...scenario.steps, newStep('think')] })}
          >
            Add think
          </Button>
        </span>
      </div>

      {scenario.steps.length === 0 ? (
        <p className="text-ink-3 text-xs">No steps yet.</p>
      ) : (
        <ol className="flex flex-col gap-2">
          {scenario.steps.map((step, stepIndex) => (
            <StepEditor
              key={step.id}
              step={step}
              index={stepIndex}
              total={scenario.steps.length}
              onChange={(patch) => onChange({ steps: replace(scenario.steps, step.id, patch) })}
              onRemove={() =>
                onChange({ steps: scenario.steps.filter((other) => other.id !== step.id) })
              }
              onMove={(delta) => {
                const next = [...scenario.steps];
                const to = stepIndex + delta;
                if (to < 0 || to >= next.length) return;
                [next[stepIndex], next[to]] = [next[to]!, next[stepIndex]!];
                onChange({ steps: next });
              }}
            />
          ))}
        </ol>
      )}
    </div>
  );
}

function StepEditor({
  step,
  index,
  total,
  onChange,
  onRemove,
  onMove,
}: {
  step: Step;
  index: number;
  total: number;
  onChange: (patch: Partial<Step>) => void;
  onRemove: () => void;
  onMove: (delta: number) => void;
}) {
  return (
    <li className="border-line bg-surface rounded-md border p-2.5">
      <div className="mb-2 flex items-center justify-between gap-2">
        <span className="text-ink-3 text-xs font-medium">
          {index + 1}. {step.kind === 'think' ? 'Think' : 'Request'}
        </span>
        <span className="flex items-center gap-0.5">
          <button
            type="button"
            onClick={() => onMove(-1)}
            disabled={index === 0}
            aria-label={`Move step ${index + 1} earlier`}
            className="text-ink-3 hover:text-ink rounded px-1 disabled:opacity-30"
          >
            ↑
          </button>
          <button
            type="button"
            onClick={() => onMove(1)}
            disabled={index === total - 1}
            aria-label={`Move step ${index + 1} later`}
            className="text-ink-3 hover:text-ink rounded px-1 disabled:opacity-30"
          >
            ↓
          </button>
          <RemoveButton label={`Remove step ${index + 1}`} onClick={onRemove} />
        </span>
      </div>

      {step.kind === 'think' ? (
        <TextField
          label="Pause for"
          value={step.think}
          onChange={(think) => onChange({ think })}
          placeholder="1s-3s"
          hint="A range rather than a fixed value, so users do not move in lockstep."
        />
      ) : (
        <div className="flex flex-col gap-2.5">
          <div className="flex items-end gap-1.5">
            <SelectField
              label="Method"
              value={step.method}
              options={HTTP_METHODS}
              onChange={(method) => onChange({ method })}
              className="w-28"
            />
            <TextField
              label="Path or URL"
              value={step.url}
              onChange={(url) => onChange({ url })}
              placeholder="/api/products/${id}"
              className="flex-1"
              mono
            />
          </div>

          <div className="grid gap-2.5 sm:grid-cols-3">
            <TextField
              label="Expect status"
              value={step.expect}
              onChange={(expect) => onChange({ expect })}
              placeholder="200"
            />
            <TextField
              label="Metric name"
              value={step.name}
              onChange={(name) => onChange({ name })}
              placeholder="derived from the path"
              hint="Set it when the path has ids in it."
            />
            <TextField
              label="Pause after"
              value={step.betweenRequests}
              onChange={(betweenRequests) => onChange({ betweenRequests })}
              placeholder="run default"
              hint={'"0" for none.'}
            />
          </div>

          {methodTakesBody(step.method) || step.bodyKind !== 'none' ? (
            <div className="flex flex-col gap-2">
              <SelectField
                label="Body"
                value={step.bodyKind}
                options={BODY_KINDS}
                onChange={(bodyKind) => onChange({ bodyKind })}
                className="w-32"
              />
              {step.bodyKind === 'json' || step.bodyKind === 'raw' ? (
                <Field
                  label={step.bodyKind === 'json' ? 'JSON body' : 'Raw body'}
                  hint={
                    step.bodyKind === 'json'
                      ? 'Parsed and embedded as structure. ${placeholders} work inside strings.'
                      : 'Sent verbatim.'
                  }
                >
                  {(id) => (
                    <textarea
                      id={id}
                      value={step.body}
                      onChange={(event) => onChange({ body: event.target.value })}
                      spellCheck={false}
                      rows={4}
                      placeholder={
                        step.bodyKind === 'json' ? '{ "productId": 4711, "quantity": 1 }' : ''
                      }
                      className="border-line bg-page text-ink w-full resize-y rounded-md border px-2 py-1.5 font-mono text-xs"
                    />
                  )}
                </Field>
              ) : null}
              {step.bodyKind === 'form' ? (
                <PairRows
                  label="Form fields"
                  pairs={step.form}
                  onChange={(form) => onChange({ form })}
                  addLabel="Add field"
                />
              ) : null}
            </div>
          ) : null}

          <PairRows
            label="Capture from the response"
            pairs={step.capture}
            onChange={(capture) => onChange({ capture })}
            keyPlaceholder="productId"
            valuePlaceholder="items.0.id"
            hint="Use the captured name later as ${productId}."
            addLabel="Add capture"
          />

          <details className="text-xs">
            <summary className="text-ink-3 cursor-pointer">Headers, query, timeout</summary>
            <div className="mt-2 flex flex-col gap-2.5">
              <PairRows
                label="Headers"
                pairs={step.headers}
                onChange={(headers) => onChange({ headers })}
                addLabel="Add header"
              />
              <PairRows
                label="Query parameters"
                pairs={step.query}
                onChange={(query) => onChange({ query })}
                keyPlaceholder="page"
                valuePlaceholder="1"
                addLabel="Add parameter"
              />
              <TextField
                label="Timeout"
                value={step.timeout}
                onChange={(timeout) => onChange({ timeout })}
                placeholder="run default"
              />
            </div>
          </details>
        </div>
      )}
    </li>
  );
}

function ThresholdEditor({
  threshold,
  index,
  onChange,
  onRemove,
}: {
  threshold: Threshold;
  index: number;
  onChange: (patch: Partial<Threshold>) => void;
  onRemove: () => void;
}) {
  return (
    <li className="flex flex-wrap items-end gap-1.5">
      <SelectField
        label={index === 0 ? 'Metric' : ''}
        value={threshold.metric}
        options={THRESHOLD_METRICS}
        onChange={(metric) => onChange({ metric })}
        className="min-w-44 flex-1"
      />
      <SelectField
        label={index === 0 ? 'Stat' : ''}
        value={threshold.stat}
        options={THRESHOLD_STATS}
        onChange={(stat) => onChange({ stat })}
        className="w-24"
      />
      <SelectField
        label={index === 0 ? 'Is' : ''}
        value={threshold.op}
        options={THRESHOLD_OPS}
        onChange={(op) => onChange({ op })}
        className="w-20"
      />
      <TextField
        label={index === 0 ? 'Than' : ''}
        value={threshold.value}
        onChange={(value) => onChange({ value })}
        placeholder="500"
        className="w-24"
      />
      <label className="flex items-center gap-1.5 pb-2 text-xs">
        <input
          type="checkbox"
          checked={threshold.abortOnFail}
          onChange={(event) => onChange({ abortOnFail: event.target.checked })}
          className="accent-accent size-3.5"
        />
        <span className="text-ink-3">abort</span>
      </label>
      <span className="pb-1">
        <RemoveButton label={`Remove threshold ${index + 1}`} onClick={onRemove} />
      </span>
    </li>
  );
}
