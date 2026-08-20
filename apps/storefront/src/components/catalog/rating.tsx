import { Star } from 'lucide-react';

import { cn } from '@/lib/utils';

/**
 * A star rating.
 *
 * The visual is five stars with a clipped overlay, which is the only way to
 * show 4.3 honestly — rounding to the nearest half star turns 4.26 and 4.74
 * into the same picture, and those are meaningfully different products.
 *
 * The accessible name carries the real number and the count. A screen-reader
 * user hearing "four point three out of five, two hundred and eleven reviews"
 * has strictly more information than a sighted user squinting at partial
 * stars, which is the right way round.
 */
export function Rating({
  value,
  count,
  size = 'default',
  showCount = true,
  className,
}: {
  value: number | null;
  count?: number;
  size?: 'sm' | 'default';
  showCount?: boolean;
  className?: string;
}) {
  // No rating is not the same as a rating of zero. A new product with no
  // reviews must not render five empty stars, which reads as "everyone hated
  // it" rather than "nobody has said yet".
  if (value === null || value === undefined) {
    return showCount ? (
      <span className={cn('text-xs text-muted-foreground', className)}>No reviews yet</span>
    ) : null;
  }

  const clamped = Math.max(0, Math.min(5, value));
  const starSize = size === 'sm' ? 'h-3.5 w-3.5' : 'h-4 w-4';

  return (
    <div
      className={cn('flex items-center gap-1.5', className)}
      role="img"
      aria-label={
        count === undefined
          ? `Rated ${clamped.toFixed(1)} out of 5`
          : `Rated ${clamped.toFixed(1)} out of 5 from ${count} review${count === 1 ? '' : 's'}`
      }
    >
      <span className="relative inline-flex" aria-hidden="true">
        <span className="flex">
          {[0, 1, 2, 3, 4].map((i) => (
            <Star key={i} className={cn(starSize, 'text-muted-foreground/30')} fill="currentColor" />
          ))}
        </span>
        {/* Clipped overlay — a fractional width, not a rounded star count. */}
        <span
          className="absolute inset-0 flex overflow-hidden"
          style={{ width: `${(clamped / 5) * 100}%` }}
        >
          {[0, 1, 2, 3, 4].map((i) => (
            <Star key={i} className={cn(starSize, 'shrink-0 text-warning')} fill="currentColor" />
          ))}
        </span>
      </span>

      {showCount && count !== undefined && (
        <span className="tabular text-xs text-muted-foreground" aria-hidden="true">
          {clamped.toFixed(1)} ({count.toLocaleString()})
        </span>
      )}
    </div>
  );
}
