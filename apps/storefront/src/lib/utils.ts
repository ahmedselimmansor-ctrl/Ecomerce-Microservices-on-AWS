import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

/**
 * Merge class names, letting later Tailwind classes win.
 *
 * `clsx` alone concatenates, so `cn('p-2', 'p-4')` produces `"p-2 p-4"` and the
 * winner depends on stylesheet order rather than on argument order. `twMerge`
 * understands that `p-2` and `p-4` are the same property and drops the first.
 * Without it, every component's `className` override is a coin flip.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/**
 * Percentage saved against the reference price, or null.
 *
 * Returns null rather than 0 when there is no saving, so the caller renders
 * nothing instead of a "0% off" badge. Rounds down: claiming 30% off when the
 * real figure is 29.6% is a small lie that a consumer authority treats as a
 * large one.
 */
export function discountPercent(
  price: { amount: number },
  listPrice: { amount: number } | null | undefined,
): number | null {
  if (!listPrice || listPrice.amount <= price.amount) return null;
  const saved = Math.floor(((listPrice.amount - price.amount) / listPrice.amount) * 100);
  return saved > 0 ? saved : null;
}

/**
 * A relative time string, in whole units.
 *
 * `Intl.RelativeTimeFormat` rather than a hand-rolled table, so it is correct
 * in every locale we ship. Computed on the client only — rendering it on the
 * server bakes the server's "now" into the HTML, and a cached page then shows
 * "2 minutes ago" for an hour.
 */
export function relativeTime(iso: string, locale = 'en-GB'): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '';

  const seconds = Math.round((then - Date.now()) / 1000);
  const format = new Intl.RelativeTimeFormat(locale, { numeric: 'auto' });

  const units: [Intl.RelativeTimeFormatUnit, number][] = [
    ['year', 31_536_000],
    ['month', 2_592_000],
    ['day', 86_400],
    ['hour', 3_600],
    ['minute', 60],
  ];

  for (const [unit, size] of units) {
    if (Math.abs(seconds) >= size) {
      return format.format(Math.round(seconds / size), unit);
    }
  }
  return format.format(seconds, 'second');
}

/**
 * Build a query string, dropping empty values.
 *
 * Arrays repeat the key (`?colour=red&colour=blue`) rather than joining with a
 * comma, because that is what `URLSearchParams.getAll` reads back and what
 * search-service's `filters` map expects. Comma-joining silently breaks any
 * facet value that legitimately contains a comma.
 */
export function toQuery(params: Record<string, string | number | boolean | string[] | undefined | null>): string {
  const search = new URLSearchParams();

  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;

    if (Array.isArray(value)) {
      for (const entry of value) {
        if (entry !== '') search.append(key, entry);
      }
    } else {
      search.set(key, String(value));
    }
  }

  const query = search.toString();
  return query ? `?${query}` : '';
}
