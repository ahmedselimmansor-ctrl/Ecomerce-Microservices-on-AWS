'use client';

import { useCart } from './cart-provider';

/**
 * The badge on the basket icon.
 *
 * `aria-live="polite"` so a screen-reader user hears the basket change without
 * being interrupted mid-sentence — a sighted user sees the number move, and
 * this is the equivalent. Polite rather than assertive: adding to a basket is
 * not an emergency.
 */
export function CartCount() {
  const { itemCount } = useCart();

  return (
    <>
      {itemCount > 0 && (
        <span
          aria-hidden="true"
          className="tabular absolute -right-0.5 -top-0.5 flex h-5 min-w-5 items-center justify-center rounded-full bg-primary px-1 text-[10px] font-bold text-primary-foreground"
        >
          {itemCount > 99 ? '99+' : itemCount}
        </span>
      )}
      <span aria-live="polite" className="sr-only">
        {itemCount === 0
          ? 'Basket is empty'
          : `${itemCount} item${itemCount === 1 ? '' : 's'} in your basket`}
      </span>
    </>
  );
}
