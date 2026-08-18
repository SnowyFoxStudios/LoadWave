/**
 * A small YAML emitter for the shapes the scenario builder produces.
 *
 * Hand-rolled rather than pulled in as a dependency, because the builder emits
 * exactly one schema — nested maps, lists, and scalar leaves — and the whole
 * value of the output is that a human can read it, copy it into a repository,
 * and hand-edit it afterwards. A general emitter brings anchors, tags and flow
 * styles that would only make that harder.
 *
 * The one thing this must get right is quoting. A header value of
 * `Bearer x: y`, a URL with a `#`, a version string like `1.0` meant as text —
 * each will silently change meaning if emitted bare, and the resulting config
 * would be wrong in a way nobody notices until the run measures the wrong
 * thing. Anything not provably safe is double-quoted.
 */

/** A value the emitter can render. */
export type YamlValue =
  string | number | boolean | null | undefined | YamlValue[] | { [key: string]: YamlValue };

/**
 * Scalars that are safe to emit unquoted.
 *
 * Conservative but not paranoid. A leading digit or slash is fine — `30s` and
 * `/api/products` are both plain strings to YAML — so quoting them would only
 * make the output noisier than what a person would have written. What must not
 * appear first is an indicator character: `-`, `#`, `*`, `&`, `!`, `%`, `?`,
 * `:`, `,`, `[`, `{`, `>`, `|`, `@`, a backtick or a quote. None are in this
 * class, so anything containing one is quoted.
 */
const SAFE_PLAIN = /^[A-Za-z0-9_/][A-Za-z0-9_./-]*$/;

/** Strings YAML reads as something other than text. */
const RESERVED = new Set(['true', 'false', 'yes', 'no', 'on', 'off', 'null', 'nil', '~', 'y', 'n']);

/** Anything that looks like a number must be quoted to stay a string. */
const NUMERIC = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;

/** Renders a string, quoting it unless it is provably safe bare. */
export function yamlString(value: string): string {
  if (value === '') return '""';

  const plain =
    SAFE_PLAIN.test(value) && !RESERVED.has(value.toLowerCase()) && !NUMERIC.test(value);

  if (plain) return value;

  // Double quotes with JSON escaping. JSON string syntax is a subset of YAML's
  // double-quoted style, so this is both correct and familiar to read.
  return JSON.stringify(value);
}

/** Renders a map key. */
function yamlKey(key: string): string {
  return SAFE_PLAIN.test(key) ? key : JSON.stringify(key);
}

/** True for values that should be omitted rather than emitted as empty. */
function isAbsent(value: YamlValue): boolean {
  if (value === undefined || value === null) return true;
  if (typeof value === 'string') return value === '';
  if (Array.isArray(value)) return value.length === 0;
  if (typeof value === 'object') return Object.keys(value).every((k) => isAbsent(value[k]));
  return false;
}

/** True for a value that renders on one line. */
function isScalar(value: YamlValue): boolean {
  return value === null || ['string', 'number', 'boolean'].includes(typeof value);
}

/** Renders a scalar. */
function scalar(value: YamlValue): string {
  if (value === null) return 'null';
  if (typeof value === 'string') return yamlString(value);
  if (typeof value === 'boolean') return value ? 'true' : 'false';
  if (typeof value === 'number') {
    if (!Number.isFinite(value)) return '0';
    return String(value);
  }
  return '""';
}

/** Largest map still worth inlining as a list item. A threshold has four
 *  fields and reads far better on one line than on four. */
const MAX_INLINE_ENTRIES = 5;

/** Renders a list or map inline, for short leaf collections. */
function inline(value: YamlValue): string {
  if (Array.isArray(value)) {
    return `[${value.map((item) => scalar(item)).join(', ')}]`;
  }
  const entries = Object.entries(value as Record<string, YamlValue>).filter(
    ([, v]) => !isAbsent(v),
  );
  return `{ ${entries.map(([k, v]) => `${yamlKey(k)}: ${scalar(v)}`).join(', ')} }`;
}

/** True for a scalar list, which always reads better on one line. */
function isScalarList(value: YamlValue): boolean {
  return Array.isArray(value) && value.length > 0 && value.every(isScalar);
}

/**
 * True when a map is short and flat enough to read better on one line.
 *
 * Only ever applied to list items. A map that is the value of a key — `load:`,
 * `http:`, `capture:` — stays in block style however short it is, because
 * that is how these files are written by hand and how they stay readable as
 * fields are added to them later.
 */
function preferInlineItem(value: YamlValue): boolean {
  if (isScalarList(value)) return true;
  if (Array.isArray(value) || typeof value !== 'object' || value === null) return false;

  const entries = Object.entries(value).filter(([, v]) => !isAbsent(v));
  return (
    // A single field reads better bare — `- think: 1s-3s`, not
    // `- { think: 1s-3s }` — so braces are earned from two fields up.
    entries.length >= 2 &&
    entries.length <= MAX_INLINE_ENTRIES &&
    entries.every(([, v]) => isScalar(v)) &&
    inline(value).length <= 76
  );
}

/** Renders a value at the given indent, as the body of a key or list item. */
function render(value: YamlValue, indent: number, lines: string[]): void {
  const pad = '  '.repeat(indent);

  if (Array.isArray(value)) {
    for (const item of value) {
      if (isScalar(item)) {
        lines.push(`${pad}- ${scalar(item)}`);
      } else if (preferInlineItem(item)) {
        lines.push(`${pad}- ${inline(item)}`);
      } else {
        // Render the item, then fold its first line onto the dash so the
        // result reads the way a person would have written it.
        const nested: string[] = [];
        render(item, indent + 1, nested);
        if (nested.length === 0) continue;
        lines.push(`${pad}- ${nested[0]!.trimStart()}`);
        lines.push(...nested.slice(1));
      }
    }
    return;
  }

  for (const [key, child] of Object.entries(value as Record<string, YamlValue>)) {
    if (isAbsent(child)) continue;

    if (isScalar(child)) {
      lines.push(`${pad}${yamlKey(key)}: ${scalar(child)}`);
    } else if (isScalarList(child)) {
      // `expect: [200, 201]` — a list of scalars never earns four lines.
      lines.push(`${pad}${yamlKey(key)}: ${inline(child)}`);
    } else {
      lines.push(`${pad}${yamlKey(key)}:`);
      render(child, indent + 1, lines);
    }
  }
}

/**
 * Serialises a value as YAML.
 *
 * Absent fields — undefined, null, empty strings and empty collections — are
 * omitted rather than emitted blank, so the output is the configuration
 * somebody would have written by hand rather than a filled-in schema dump.
 */
export function toYaml(value: YamlValue): string {
  if (isAbsent(value)) return '';

  const lines: string[] = [];
  render(value, 0, lines);
  return lines.length === 0 ? '' : `${lines.join('\n')}\n`;
}

/** Prefixes each line of a comment block with `# `. */
export function yamlComment(text: string): string {
  return text
    .split('\n')
    .map((line) => (line ? `# ${line}` : '#'))
    .join('\n');
}
