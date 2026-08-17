import { describe, expect, it } from 'vitest';

import { toYaml, yamlString } from './yaml';

describe('yamlString', () => {
  it('leaves provably safe scalars bare', () => {
    expect(yamlString('browse')).toBe('browse');
    expect(yamlString('list-products')).toBe('list-products');
    expect(yamlString('/api/products')).toBe('/api/products');
    expect(yamlString('500ms-2s')).toBe('500ms-2s');
  });

  it('quotes anything that would change meaning', () => {
    // Each of these silently becomes something other than a string if emitted
    // bare, and the resulting config is wrong in a way nobody notices.
    expect(yamlString('Bearer abc: def')).toBe('"Bearer abc: def"');
    expect(yamlString('value # not a comment')).toBe('"value # not a comment"');
    expect(yamlString('0')).toBe('"0"');
    expect(yamlString('1.0')).toBe('"1.0"');
    expect(yamlString('true')).toBe('"true"');
    expect(yamlString('no')).toBe('"no"');
    expect(yamlString('~')).toBe('"~"');
    expect(yamlString('')).toBe('""');
    expect(yamlString('- leading dash')).toBe('"- leading dash"');
    expect(yamlString('has "quotes"')).toBe('"has \\"quotes\\""');
    expect(yamlString('line\nbreak')).toBe('"line\\nbreak"');
  });

  it('quotes template placeholders, which start with an indicator', () => {
    expect(yamlString('${productId}')).toBe('"${productId}"');
    expect(yamlString('/api/products/${id}')).toBe('"/api/products/${id}"');
  });
});

describe('toYaml', () => {
  it('renders nested maps', () => {
    expect(
      toYaml({
        name: 'storefront',
        load: { executor: 'ramping-vus' },
      }),
    ).toBe('name: storefront\nload:\n  executor: ramping-vus\n');
  });

  it('renders short flat maps inline, the way a person would write them', () => {
    expect(toYaml({ stages: [{ duration: '30s', target: 100 }] })).toBe(
      'stages:\n  - { duration: 30s, target: 100 }\n',
    );
  });

  it('renders short scalar lists inline', () => {
    expect(toYaml({ expect: [200, 201] })).toBe('expect: [200, 201]\n');
  });

  it('leaves a single-field list item bare rather than braced', () => {
    // `- think: 1s-3s` is how a person writes it; braces are earned from two
    // fields up.
    expect(toYaml({ steps: [{ think: '1s-3s' }] })).toBe('steps:\n  - think: 1s-3s\n');
    expect(toYaml({ steps: [{ get: '/a', expect: [200] }] })).toBe(
      'steps:\n  - get: /a\n    expect: [200]\n',
    );
  });

  it('folds a list item onto its dash', () => {
    const yaml = toYaml({
      scenarios: [{ name: 'browse', steps: [{ get: '/a', expect: [200] }] }],
    });
    // The step has a list value, so it is not flat enough to inline; its first
    // field folds onto the dash instead.
    expect(yaml).toBe(
      'scenarios:\n  - name: browse\n    steps:\n      - get: /a\n        expect: [200]\n',
    );
  });

  it('keeps a map value in block style however short it is', () => {
    // `load:` with one field today may have four tomorrow, and block style is
    // how these files are written and edited by hand.
    expect(toYaml({ capture: { id: 'items.0.id' } })).toBe('capture:\n  id: items.0.id\n');
  });

  it('omits absent fields rather than emitting them blank', () => {
    // The output should be the configuration somebody would have written, not
    // a filled-in schema dump.
    expect(
      toYaml({
        name: 'x',
        baseURL: '',
        weight: undefined,
        tags: {},
        thresholds: [],
        nested: { all: '', empty: null },
      }),
    ).toBe('name: x\n');
  });

  it('keeps false and zero, which are values rather than absences', () => {
    expect(toYaml({ followRedirects: false, target: 0 })).toBe(
      'followRedirects: false\ntarget: 0\n',
    );
  });

  it('returns an empty string for nothing at all', () => {
    expect(toYaml({})).toBe('');
    expect(toYaml(null)).toBe('');
  });

  it('leaves ordinary header names bare and quotes the awkward ones', () => {
    expect(toYaml({ headers: { 'X-Request-Id': 'abc' } })).toBe('headers:\n  X-Request-Id: abc\n');
    expect(toYaml({ headers: { 'odd: name': 'v' } })).toBe('headers:\n  "odd: name": v\n');
  });

  it('emits a realistic configuration', () => {
    const yaml = toYaml({
      name: 'storefront',
      baseURL: 'https://staging.example.com',
      betweenRequests: '500ms-2s',
      load: {
        executor: 'ramping-vus',
        stages: [
          { duration: '30s', target: 100 },
          { duration: '5m', target: 100 },
        ],
      },
      http: { headers: { Authorization: 'Bearer secret: value' } },
      thresholds: [{ metric: 'http_req_duration', stat: 'p95', op: '<', value: 500 }],
      scenarios: [
        {
          name: 'browse',
          weight: 3,
          steps: [
            { get: '/api/products', expect: [200], capture: { id: 'items.0.id' } },
            { think: '1s-3s' },
            { get: '/api/products/${id}', expect: [200] },
          ],
        },
      ],
    });

    expect(yaml).toBe(
      [
        'name: storefront',
        'baseURL: "https://staging.example.com"',
        'betweenRequests: 500ms-2s',
        'load:',
        '  executor: ramping-vus',
        '  stages:',
        '    - { duration: 30s, target: 100 }',
        '    - { duration: 5m, target: 100 }',
        'http:',
        '  headers:',
        '    Authorization: "Bearer secret: value"',
        'thresholds:',
        '  - { metric: http_req_duration, stat: p95, op: "<", value: 500 }',
        'scenarios:',
        '  - name: browse',
        '    weight: 3',
        '    steps:',
        '      - get: /api/products',
        '        expect: [200]',
        '        capture:',
        '          id: items.0.id',
        '      - think: 1s-3s',
        '      - get: "/api/products/${id}"',
        '        expect: [200]',
        '',
      ].join('\n'),
    );
  });
});
