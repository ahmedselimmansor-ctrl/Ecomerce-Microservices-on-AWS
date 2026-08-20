'use client';

import { useState } from 'react';
import { Check, Loader2, ShoppingBag } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { QuantityStepper } from './quantity-stepper';
import { useCart } from './cart-provider';
import { canAttemptPurchase } from '@/components/catalog/stock-indicator';

/**
 * The Add to Cart control.
 *
 * The button is enabled whenever availability is *unknown*, and disabled only
 * on a confirmed zero. That is the deliberate reading of an eventually
 * consistent field: `available` is denormalised from inventory-service and the
 * authoritative check happens when the saga reserves. Blocking on a stale null
 * refuses sales of items that are in stock, which costs more than the
 * occasional rejected reservation.
 *
 * After a successful add the button shows a tick for a moment rather than
 * navigating away. Redirecting to the basket after every add is the single
 * most effective way to stop someone buying a second thing.
 */
export function AddToCart({
  sku,
  available,
  className,
}: {
  sku: string;
  available: number | null | undefined;
  className?: string;
}) {
  const { add, loading } = useCart();
  const [quantity, setQuantity] = useState(1);
  const [justAdded, setJustAdded] = useState(false);

  const purchasable = canAttemptPurchase(available);

  // Cap the stepper at what inventory last told us, when it told us anything.
  // Not a guarantee — it is a hint that saves a round trip for the obvious case.
  const max = typeof available === 'number' && available > 0 ? Math.min(available, 99) : 99;

  async function onAdd() {
    const ok = await add(sku, quantity);
    if (!ok) return;

    setJustAdded(true);
    setTimeout(() => setJustAdded(false), 2000);
  }

  return (
    <div className={className}>
      <div className="flex flex-wrap items-center gap-3">
        <QuantityStepper
          value={quantity}
          onCommit={setQuantity}
          max={max}
          disabled={!purchasable || loading}
        />

        <Button
          size="lg"
          className="flex-1 sm:flex-none sm:min-w-48"
          disabled={!purchasable || loading}
          onClick={onAdd}
        >
          {loading ? (
            <Loader2 className="animate-spin" />
          ) : justAdded ? (
            <Check />
          ) : (
            <ShoppingBag />
          )}
          {!purchasable ? 'Out of stock' : justAdded ? 'Added' : 'Add to basket'}
        </Button>
      </div>

      {/*
        The status is announced separately rather than by changing the button's
        own label. A screen reader re-reads a button whose text changes while it
        holds focus, so "Added" would be announced as a new control appearing.
      */}
      <p aria-live="polite" className="sr-only">
        {justAdded ? `${quantity} added to your basket` : ''}
      </p>
    </div>
  );
}
