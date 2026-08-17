import { describe, expect, it } from 'vitest';

import { toYaml } from '../lib/yaml';
import {
  findProblems,
  newDraft,
  newPair,
  newScenario,
  newStep,
  toConfig,
  type Draft,
} from './model';

/** The draft a fresh builder starts from, with its scenario replaced. */
function draftWith(mutate: (draft: Draft) => void): Draft {
  const draft = newDraft();
  mutate(draft);
  return draft;
}

describe('toConfig', () => {
  it('emits a runnable configuration from the default draft', () => {
    const yaml = toYaml(toConfig(newDraft()));

    // The shape the server expects, and the shape a person would have written.
    expect(yaml).toContain('name: my-test');
    expect(yaml).toContain('executor: ramping-vus');
    expect(yaml).toContain('stages:');
    expect(yaml).toContain('- { duration: 30s, target: 25 }');
    expect(yaml).toContain('scenarios:');
    expect(yaml).toContain('- name: browse');
    expect(yaml).toContain('get: /');
  });

  it('uses the method shorthand rather than method plus url', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.method = 'POST';
      step.url = '/api/orders';
      d.scenarios = [{ ...newScenario('checkout'), steps: [step] }];
    });

    const yaml = toYaml(toConfig(draft));
    expect(yaml).toContain('post: /api/orders');
    expect(yaml).not.toContain('method:');
  });

  it('embeds a JSON body as structure, not as a quoted blob', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.method = 'POST';
      step.url = '/api/orders';
      step.bodyKind = 'json';
      step.body = '{"productId": 4711, "quantity": 1}';
      d.scenarios = [{ ...newScenario('checkout'), steps: [step] }];
    });

    const yaml = toYaml(toConfig(draft));
    expect(yaml).toContain('json:');
    expect(yaml).toContain('productId: 4711');
    // Not a string containing braces.
    expect(yaml).not.toContain('json: "{');
  });

  it('switches between the executors cleanly', () => {
    const ramping = toYaml(toConfig(draftWith((d) => (d.executor = 'ramping-vus'))));
    expect(ramping).toContain('stages:');
    expect(ramping).not.toContain('vus:');

    const constant = toYaml(toConfig(draftWith((d) => (d.executor = 'constant-vus'))));
    expect(constant).toContain('vus: 50');
    expect(constant).toContain('duration: 5m');
    expect(constant).not.toContain('stages:');
  });

  it('omits empty fields rather than emitting them blank', () => {
    const yaml = toYaml(toConfig(newDraft()));

    // Unset by default; a file restating every default is harder to read.
    expect(yaml).not.toContain('betweenRequests:');
    expect(yaml).not.toContain('workersPerAgent:');
    expect(yaml).not.toContain('followRedirects:');
    expect(yaml).not.toContain('tags:');
    expect(yaml).not.toContain('description:');
  });

  it('drops key/value rows with no key', () => {
    const draft = draftWith((d) => {
      d.headers = [newPair('Accept', 'application/json'), newPair('', 'orphaned')];
      d.tags = [newPair('', '')];
    });

    const yaml = toYaml(toConfig(draft));
    expect(yaml).toContain('Accept: application/json');
    expect(yaml).not.toContain('orphaned');
    expect(yaml).not.toContain('tags:');
  });

  it('parses expected statuses however they are typed', () => {
    for (const typed of ['200,201', '200 201', ' 200 , 201 ']) {
      const draft = draftWith((d) => {
        const step = newStep();
        step.expect = typed;
        d.scenarios = [{ ...newScenario(), steps: [step] }];
      });
      expect(toYaml(toConfig(draft))).toContain('expect: [200, 201]');
    }
  });

  it('quotes a template placeholder so it survives the emitter', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.url = '/api/products/${id}';
      d.scenarios = [{ ...newScenario(), steps: [step] }];
    });

    expect(toYaml(toConfig(draft))).toContain('get: "/api/products/${id}"');
  });

  it('keeps a captured value as a path, not a quoted number', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.capture = [newPair('id', 'items.0.id')];
      d.scenarios = [{ ...newScenario(), steps: [step] }];
    });

    expect(toYaml(toConfig(draft))).toContain('id: items.0.id');
  });

  it('renders a think step as a think step', () => {
    const draft = draftWith((d) => {
      const think = newStep('think');
      think.think = '500ms-2s';
      d.scenarios = [{ ...newScenario(), steps: [newStep(), think] }];
    });

    expect(toYaml(toConfig(draft))).toContain('- think: 500ms-2s');
  });
});

describe('findProblems', () => {
  it('accepts the default draft', () => {
    expect(findProblems(newDraft())).toEqual([]);
  });

  it('reports a malformed JSON body, which the emitter cannot explain', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.bodyKind = 'json';
      step.body = '{ not json';
      d.scenarios = [{ ...newScenario('checkout'), steps: [step] }];
    });

    const problems = findProblems(draft);
    expect(problems).toHaveLength(1);
    expect(problems[0]!.where).toBe('checkout, step 1');
    expect(problems[0]!.message).toContain('JSON body is malformed');
  });

  it('reports a request with no URL and a scenario with no steps', () => {
    const draft = draftWith((d) => {
      const step = newStep();
      step.url = '   ';
      d.scenarios = [
        { ...newScenario('empty'), steps: [] },
        { ...newScenario('urlless'), steps: [step] },
      ];
    });

    const messages = findProblems(draft).map((p) => `${p.where}: ${p.message}`);
    expect(messages).toContain('empty: has no steps');
    expect(messages).toContain('urlless, step 1: needs a URL');
  });

  it('names an unnamed scenario by position', () => {
    const draft = draftWith((d) => {
      d.scenarios = [{ ...newScenario(''), steps: [] }];
    });
    expect(findProblems(draft)[0]!.where).toBe('Scenario 1');
  });
});
