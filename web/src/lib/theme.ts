import { useEffect, useState } from 'react';

export type ThemeChoice = 'light' | 'dark' | 'system';

const STORAGE_KEY = 'loadwave.theme';

/** Reads the stored choice, tolerating storage being unavailable. */
function readChoice(): ThemeChoice {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === 'light' || saved === 'dark') return saved;
  } catch {
    /* private browsing blocks storage; fall back to the OS setting */
  }
  return 'system';
}

/** Stamps or clears the attribute the CSS tokens key off. */
function applyChoice(choice: ThemeChoice): void {
  if (choice === 'system') {
    delete document.documentElement.dataset.theme;
    return;
  }
  document.documentElement.dataset.theme = choice;
}

/**
 * Owns the light/dark choice.
 *
 * Three states, not two: an explicit choice stamps `data-theme`, and "system"
 * clears it so `prefers-color-scheme` governs. `resolved` is what is actually
 * on screen, which is what the charts need — they read concrete hex values
 * out of the stylesheet and must be rebuilt when those change.
 */
export function useTheme(): {
  choice: ThemeChoice;
  resolved: 'light' | 'dark';
  setChoice: (next: ThemeChoice) => void;
  toggle: () => void;
} {
  const [choice, setChoiceState] = useState<ThemeChoice>(readChoice);
  const [systemDark, setSystemDark] = useState(
    () => window.matchMedia('(prefers-color-scheme: dark)').matches,
  );

  useEffect(() => {
    const query = window.matchMedia('(prefers-color-scheme: dark)');
    const listener = (event: MediaQueryListEvent) => setSystemDark(event.matches);
    query.addEventListener('change', listener);
    return () => query.removeEventListener('change', listener);
  }, []);

  useEffect(() => {
    applyChoice(choice);
  }, [choice]);

  const setChoice = (next: ThemeChoice) => {
    setChoiceState(next);
    try {
      if (next === 'system') localStorage.removeItem(STORAGE_KEY);
      else localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* the in-memory choice still applies for this session */
    }
  };

  const resolved: 'light' | 'dark' = choice === 'system' ? (systemDark ? 'dark' : 'light') : choice;

  return {
    choice,
    resolved,
    setChoice,
    toggle: () => setChoice(resolved === 'dark' ? 'light' : 'dark'),
  };
}

/** Design tokens the charts need as concrete colors. */
export interface ChartColors {
  surface: string;
  grid: string;
  axis: string;
  text: string;
  textMuted: string;
  vus: string;
  rps: string;
  error: string;
  latency: [string, string, string, string];
  status: Record<string, string>;
  /** Fixed-order categorical slots. Never cycled past the eighth. */
  categorical: string[];
}

/** Reads one custom property off the document root. */
function token(styles: CSSStyleDeclaration, name: string): string {
  return styles.getPropertyValue(name).trim();
}

/**
 * Resolves the chart tokens to concrete colors for the current theme.
 *
 * uPlot draws to a canvas and cannot follow a CSS variable, so the values have
 * to be read out of the stylesheet and handed over. Recomputing on `resolved`
 * is what makes the charts actually change colour when the theme does.
 */
export function useChartColors(resolved: 'light' | 'dark'): ChartColors {
  const [colors, setColors] = useState<ChartColors>(() => readChartColors());

  useEffect(() => {
    // A frame's delay lets the attribute change settle into computed styles
    // before they are read back.
    const id = requestAnimationFrame(() => setColors(readChartColors()));
    return () => cancelAnimationFrame(id);
  }, [resolved]);

  return colors;
}

function readChartColors(): ChartColors {
  const styles = getComputedStyle(document.documentElement);
  return {
    surface: token(styles, '--surface-1') || '#fcfcfb',
    grid: token(styles, '--grid') || '#eceae5',
    axis: token(styles, '--axis') || '#b9b7b0',
    text: token(styles, '--text-primary') || '#0b0b0b',
    textMuted: token(styles, '--text-muted') || '#7a7873',
    vus: token(styles, '--series-vus') || '#2a78d6',
    rps: token(styles, '--series-rps') || '#1baf7a',
    error: token(styles, '--series-error') || '#d03b3b',
    // Ordered p50 → p99. The stylesheet decides which end is brightest,
    // because that flips between light and dark surfaces.
    latency: [
      token(styles, '--lat-p50') || '#86b6ef',
      token(styles, '--lat-p90') || '#5598e7',
      token(styles, '--lat-p95') || '#2a78d6',
      token(styles, '--lat-p99') || '#184f95',
    ],
    categorical: [
      token(styles, '--cat-1') || '#2a78d6',
      token(styles, '--cat-2') || '#eb6834',
      token(styles, '--cat-3') || '#1baf7a',
      token(styles, '--cat-4') || '#eda100',
      token(styles, '--cat-5') || '#e87ba4',
      token(styles, '--cat-6') || '#008300',
      token(styles, '--cat-7') || '#4a3aa7',
      token(styles, '--cat-8') || '#e34948',
    ],
    status: {
      '2xx': token(styles, '--status-2xx') || '#0ca30c',
      '3xx': token(styles, '--status-3xx') || '#2a78d6',
      '4xx': token(styles, '--status-4xx') || '#eda100',
      '5xx': token(styles, '--status-5xx') || '#d03b3b',
      error: token(styles, '--status-err') || '#4a3aa7',
      '1xx': token(styles, '--text-muted') || '#7a7873',
      other: token(styles, '--text-muted') || '#7a7873',
    },
  };
}
