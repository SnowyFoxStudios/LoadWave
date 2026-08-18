/** Joins class names, dropping anything falsy.
 *
 *  Lives apart from the components so that editing it does not invalidate
 *  React Fast Refresh for every component module that imports it.
 */
export function cn(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ');
}
