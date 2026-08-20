import { cn } from '@/lib/utils';

/**
 * Availability, with "unknown" as a first-class state.
 *
 * `available` is denormalised from inventory-service and is **eventually
 * consistent** — docs/CONTRACTS.md is explicit that it is a display hint. The
 * authoritative check happens when the saga reserves.
 *
 * `null` means "we have not heard from inventory", and it is not the same as
 * `0`. Rendering "Out of stock" when the truth is "unknown" suppresses sales of
 * items that are actually in stock, so the unknown case says nothing at all and
 * lets the Add to Cart button stay enabled. The reservation will reject it if
 * there is genuinely none left, and a rejected reservation is a far cheaper
 * mistake than a sale never offered.
 *
 * The low-stock threshold is a nudge, and an honest one: it only appears when
 * the number really is small. Showing "Only 3 left!" on a product with four
 * hundred in a warehouse is the kind of thing that gets a retailer fined.
 */
const LOW_STOCK_THRESHOLD = 5;

export function StockIndicator({
  available,
  className,
}: {
  available: number | null | undefined;
  className?: string;
}) {
  if (available === null || available === undefined) {
    // Deliberately silent. See above.
    return null;
  }

  if (available <= 0) {
    return (
      <p className={cn('text-sm font-medium text-muted-foreground', className)}>Out of stock</p>
    );
  }

  if (available <= LOW_STOCK_THRESHOLD) {
    return (
      <p className={cn('text-sm font-medium text-warning', className)}>
        Only {available} left
      </p>
    );
  }

  return <p className={cn('text-sm font-medium text-success', className)}>In stock</p>;
}

/**
 * Whether the UI should let the customer try.
 *
 * Only a confirmed zero blocks the attempt. Unknown availability is treated as
 * "worth trying", for the reason above.
 */
export function canAttemptPurchase(available: number | null | undefined): boolean {
  return available === null || available === undefined || available > 0;
}
