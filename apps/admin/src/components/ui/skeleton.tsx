import { cn } from '@/lib/utils';

/**
 * A loading placeholder.
 *
 * `aria-hidden` and no text. A screen reader announcing "loading" for each of
 * twenty-four skeleton cards is worse than silence; the page's own live region
 * announces the result count once the data arrives.
 *
 * Callers should size these to the real content. A skeleton that is a
 * different height from what replaces it produces a layout shift at exactly
 * the moment the user starts reading.
 */
function Skeleton({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      aria-hidden="true"
      className={cn('animate-pulse rounded-md bg-muted', className)}
      {...props}
    />
  );
}

export { Skeleton };
