import { formatMoney, type Money } from '@souq/contracts';

import { cn, discountPercent } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';

/**
 * A price, with its reference price when there is a genuine saving.
 *
 * Three details are legal rather than cosmetic in most markets we sell in.
 *
 * The reference price only renders when it is strictly higher — the schema
 * already refuses to store one that is not (`list_price_not_below_price`), and
 * this is the second place that rule is enforced, on the side the customer
 * actually sees.
 *
 * The saving is rounded **down**. Claiming 30% off when the real figure is
 * 29.6% is a small lie that a consumer authority treats as a large one.
 *
 * The struck-through price is wrapped in a `<s>` with visually-hidden text.
 * `line-through` is a CSS property, so a screen reader announces two prices
 * with no indication that one of them is not what you pay.
 */
export function Price({
  price,
  listPrice,
  locale = 'en-GB',
  size = 'default',
  showSaving = true,
  className,
}: {
  price: Money;
  listPrice?: Money | null;
  locale?: string;
  size?: 'sm' | 'default' | 'lg';
  showSaving?: boolean;
  className?: string;
}) {
  const saving = discountPercent(price, listPrice);
  const hasReference = saving !== null && listPrice;

  const sizes = {
    sm: 'text-sm',
    default: 'text-lg',
    lg: 'text-2xl',
  } as const;

  return (
    <div className={cn('flex flex-wrap items-baseline gap-x-2 gap-y-1', className)}>
      <span
        className={cn('tabular font-semibold', sizes[size], hasReference && 'text-destructive')}
      >
        {formatMoney(price, locale)}
      </span>

      {hasReference && (
        <s className="tabular text-sm text-muted-foreground">
          <span className="sr-only">Previous price: </span>
          {formatMoney(listPrice, locale)}
        </s>
      )}

      {hasReference && showSaving && (
        <Badge variant="destructive" className="tabular">
          −{saving}%
        </Badge>
      )}
    </div>
  );
}

/**
 * A per-unit price for a cart line, shown only when the quantity is above one.
 *
 * At quantity 1 the unit price and the line total are the same number, and
 * printing it twice reads as a rendering bug.
 */
export function UnitPrice({
  unitPrice,
  quantity,
  locale = 'en-GB',
}: {
  unitPrice: Money;
  quantity: number;
  locale?: string;
}) {
  if (quantity <= 1) return null;

  return (
    <p className="tabular text-xs text-muted-foreground">
      {formatMoney(unitPrice, locale)} each
    </p>
  );
}
