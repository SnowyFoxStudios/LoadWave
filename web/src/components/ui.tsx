import type { ButtonHTMLAttributes, ReactNode } from 'react';

import { cn } from '../lib/cn';

export function Panel({
  title,
  action,
  children,
  className,
}: {
  title: string;
  action?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={cn('border-line bg-surface rounded-lg border', className)}>
      <header className="border-line flex items-center justify-between gap-3 border-b px-4 py-2.5">
        <h2 className="text-ink text-sm font-semibold">{title}</h2>
        {action}
      </header>
      {children}
    </section>
  );
}

export type Tone = 'neutral' | 'good' | 'warning' | 'critical' | 'accent';

const TONE_CLASSES: Record<Tone, string> = {
  neutral: 'border-line-strong text-ink-2',
  good: 'border-good/40 text-good',
  warning: 'border-warning/50 text-ink-2',
  critical: 'border-critical/50 text-critical',
  accent: 'border-accent/40 text-accent',
};

/**
 * A small status chip.
 *
 * Always carries a text label. A colored dot alone would put the entire
 * meaning on hue, which fails for colorblind readers and in forced-colors
 * mode; the dot is decoration on top of the word.
 */
export function Badge({
  tone = 'neutral',
  dot = false,
  children,
}: {
  tone?: Tone;
  dot?: boolean;
  children: ReactNode;
}) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium',
        TONE_CLASSES[tone],
      )}
    >
      {dot ? (
        <span aria-hidden="true" className="inline-block size-1.5 rounded-full bg-current" />
      ) : null}
      {children}
    </span>
  );
}

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost';
};

const BUTTON_CLASSES: Record<NonNullable<ButtonProps['variant']>, string> = {
  primary: 'bg-accent text-white hover:opacity-90',
  secondary: 'border border-line-strong bg-surface text-ink hover:bg-surface-2',
  danger: 'border border-critical/60 text-critical hover:bg-critical/10',
  ghost: 'text-ink-2 hover:bg-surface-2 hover:text-ink',
};

export function Button({ variant = 'secondary', className, ...rest }: ButtonProps) {
  return (
    <button
      type="button"
      className={cn(
        'inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium',
        'transition-opacity disabled:cursor-not-allowed disabled:opacity-50',
        BUTTON_CLASSES[variant],
        className,
      )}
      {...rest}
    />
  );
}

/** A short explanation shown where a panel has nothing to show yet. */
export function Empty({ children }: { children: ReactNode }) {
  return <p className="text-ink-3 px-4 py-6 text-center text-sm">{children}</p>;
}
