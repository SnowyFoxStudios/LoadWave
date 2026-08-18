import { useEffect, useRef, useState } from 'react';

/**
 * Whether the operating system has been asked for reduced motion.
 *
 * This is the default input to the dashboard's motion preference rather than
 * the final word on it; see useMotionPreference.
 */
export function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(
    () => window.matchMedia('(prefers-reduced-motion: reduce)').matches,
  );

  useEffect(() => {
    const query = window.matchMedia('(prefers-reduced-motion: reduce)');
    const listener = (event: MediaQueryListEvent) => setReduced(event.matches);
    query.addEventListener('change', listener);
    return () => query.removeEventListener('change', listener);
  }, []);

  return reduced;
}

/**
 * Exponential smoothing toward a target.
 *
 * `rate` is the fraction of the remaining distance covered per second, which
 * makes the result frame-rate independent: the same easing on a 60Hz laptop
 * and a 120Hz display. Linear interpolation by a fixed per-frame fraction —
 * the usual shortcut — animates twice as fast on the latter.
 *
 * Deliberately has no notion of "close enough". An earlier version snapped
 * once the remainder fell below a fraction of the target's magnitude, which is
 * reasonable for a display figure and catastrophic for an absolute timestamp:
 * a ten-thousandth of a Unix epoch is nearly three minutes, so a scrolling
 * chart's edge snapped to its target on the first frame and the smooth scroll
 * did nothing at all. Deciding when a tween has arrived belongs to the caller,
 * which knows what the numbers mean.
 */
export function approach(
  current: number,
  target: number,
  rate: number,
  deltaSeconds: number,
): number {
  if (!Number.isFinite(current)) return target;
  return current + (target - current) * (1 - Math.exp(-rate * deltaSeconds));
}

/** How fast a tweened figure closes on its new value, per second. Brisk
 *  enough to settle well inside one update interval. */
const TWEEN_RATE = 6;

/**
 * Eases a number toward its latest value.
 *
 * Stat tiles change once a second. Snapping between values reads as the page
 * reloading; sliding between them reads as a live measurement. Returns the
 * target unchanged when the viewer prefers reduced motion, or when the jump is
 * the first one — there is nothing to ease from.
 */
export function useTweenedNumber(target: number | null, enabled = true): number | null {
  const animate = enabled && target !== null;

  const [display, setDisplay] = useState<number | null>(target);
  const current = useRef<number | null>(target);

  useEffect(() => {
    if (!animate || target === null) {
      current.current = target;
      return;
    }

    const from = current.current;
    if (from === null || from === target) {
      current.current = target;
      return;
    }

    // Arrival is judged against the distance this tween set out to cover,
    // not against the target's magnitude — the latter makes the tolerance
    // meaningless for large values and impossibly tight for small ones.
    const distance = Math.abs(target - from);
    let frame = 0;
    let last = performance.now();

    // Every state update happens inside the animation callback rather than in
    // the effect body, so this drives a render loop instead of cascading a
    // synchronous re-render on each new target.
    const step = (now: number) => {
      const delta = Math.min((now - last) / 1000, 0.25);
      last = now;

      const next = approach(current.current ?? target, target, TWEEN_RATE, delta);
      const arrived = Math.abs(target - next) <= distance * 0.001;

      current.current = arrived ? target : next;
      setDisplay(current.current);

      if (!arrived) {
        frame = requestAnimationFrame(step);
      }
    };

    frame = requestAnimationFrame(step);
    return () => cancelAnimationFrame(frame);
  }, [target, animate]);

  // Not animating: the target is the answer, with no state in the way.
  return animate ? display : target;
}

/**
 * How quickly a scrolling chart's right edge corrects toward the newest datum,
 * per second. Deliberately gentle: the constant advance is what makes the
 * motion linear, and this only trims the accumulated drift.
 */
const EDGE_DRIFT_RATE = 0.6;

/**
 * How far the edge may fall behind the data before it is snapped rather than
 * eased, in seconds.
 */
const EDGE_RESYNC_SECONDS = 5;

/**
 * Advances a live chart's right edge by one frame.
 *
 * The edge is locked to the newest datum rather than to a fixed offset from
 * now. How far behind real time the data actually runs depends on the bucket
 * width, the coordinator's late-arrival grace and the network — measured at
 * four to five seconds on a local run — and a guessed constant leaves either a
 * permanently empty sliver at the right, where points then pop in mid-plot, or
 * an edge that runs ahead of the data entirely.
 *
 * Between arrivals the edge advances with the wall clock, which is what makes
 * the scroll read as linear rather than easing in and out once a second. The
 * drift term corrects the accumulated error invisibly. A gap too large to
 * close that way — a backgrounded tab, a suspended laptop, a coordinator
 * restart — is snapped instead, because easing back would take exactly as long
 * as the gap.
 *
 * All values are in seconds. An edge of zero means "not yet started".
 */
export function advanceEdge(edge: number, newest: number, deltaSeconds: number): number {
  if (edge === 0 || Math.abs(newest - edge) > EDGE_RESYNC_SECONDS) {
    return newest;
  }
  return approach(edge + deltaSeconds, newest, EDGE_DRIFT_RATE, deltaSeconds);
}

/** How the dashboard should animate. */
export type MotionChoice = 'system' | 'smooth' | 'stepped';

const MOTION_KEY = 'loadwave.motion';

function readMotionChoice(): MotionChoice {
  try {
    const saved = localStorage.getItem(MOTION_KEY);
    if (saved === 'smooth' || saved === 'stepped') return saved;
  } catch {
    /* private browsing blocks storage; fall back to the OS setting */
  }
  return 'system';
}

/**
 * Owns whether charts scroll continuously and figures tween.
 *
 * The default follows the operating system, which is the correct behaviour:
 * continuously moving charts are the kind of thing "reduce motion" exists to
 * suppress. But it must not be a dead end. That setting is often switched on
 * for reasons that have nothing to do with a line chart, and a live dashboard
 * whose only alternative is stepping a whole bucket every second is arguably
 * the more jarring of the two. So the preference is overridable, in both
 * directions, and the control says which way the system resolved it.
 */
export function useMotionPreference(): {
  choice: MotionChoice;
  setChoice: (next: MotionChoice) => void;
  /** Whether to animate. */
  smooth: boolean;
  /** Whether the OS is currently asking for reduced motion. */
  systemReduced: boolean;
} {
  const systemReduced = usePrefersReducedMotion();
  const [choice, setChoiceState] = useState<MotionChoice>(readMotionChoice);

  const setChoice = (next: MotionChoice) => {
    setChoiceState(next);
    try {
      if (next === 'system') localStorage.removeItem(MOTION_KEY);
      else localStorage.setItem(MOTION_KEY, next);
    } catch {
      /* the in-memory choice still applies for this session */
    }
  };

  const smooth = choice === 'system' ? !systemReduced : choice === 'smooth';
  return { choice, setChoice, smooth, systemReduced };
}
