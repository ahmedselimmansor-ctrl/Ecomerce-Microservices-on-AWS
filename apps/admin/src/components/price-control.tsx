'use client';

import { useState, useTransition } from 'react';
import { useRouter } from 'next/navigation';
import { Loader2, Pencil } from 'lucide-react';

import { formatMoney, type Money } from '@souq/contracts';

import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';

/**
 * Change a price.
 *
 * A **reason is required**, not optional. It is written to `price_history` by a
 * database trigger, and finance asks for it after the fact — "the price changed"
 * without "because we matched a competitor" is not the evidence a consumer
 * authority accepts.
 *
 * Input is in major units because that is how a merchandiser thinks, and
 * converted to minor units here. The conversion rounds rather than truncating:
 * `Math.round(12.99 * 100)` is 1299, whereas `Math.trunc` gives 1298 for some
 * values because 12.99 is not exactly representable in binary floating point.
 * A penny off every price is a reconciliation problem nobody enjoys.
 */
export function PriceControl({
  productId,
  sku,
  price,
  listPrice,
}: {
  productId: string;
  sku: string;
  price: Money;
  listPrice: Money | null;
}) {
  const router = useRouter();
  const [, startTransition] = useTransition();

  const [editing, setEditing] = useState(false);
  const [amount, setAmount] = useState((price.amount / 100).toFixed(2));
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);

  async function save() {
    const parsed = Number.parseFloat(amount);
    if (!Number.isFinite(parsed) || parsed < 0) return;

    const minorUnits = Math.round(parsed * 100);

    // The schema also refuses this (`list_price_not_below_price`), but catching
    // it here means a field error instead of a 409 from a constraint violation.
    if (listPrice && listPrice.amount < minorUnits) {
      alert(
        'The new price is above the reference price. Lower the reference price first, '
        + 'or a struck-through "was" figure would be misleading.',
      );
      return;
    }

    setBusy(true);
    try {
      const response = await fetch(
        `/api/admin/catalog/${encodeURIComponent(productId)}/price`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            sku,
            price: minorUnits,
            listPrice: listPrice?.amount ?? null,
            currency: price.currency,
            reason,
          }),
        },
      );

      if (!response.ok) throw new Error(await response.text());

      setEditing(false);
      setReason('');
      startTransition(() => router.refresh());
    } catch (error) {
      console.error('[catalog] price change failed', error);
      alert('Could not change the price. See the console for detail.');
    } finally {
      setBusy(false);
    }
  }

  if (!editing) {
    return (
      <button
        type="button"
        onClick={() => setEditing(true)}
        className="tabular group inline-flex items-center gap-1.5 text-sm hover:underline"
      >
        {formatMoney(price)}
        <Pencil className="h-3 w-3 opacity-0 transition-opacity group-hover:opacity-60" />
        <span className="sr-only">Change the price of {sku}</span>
      </button>
    );
  }

  return (
    <div className="flex items-center justify-end gap-1.5">
      <Input
        value={amount}
        onChange={(e) => setAmount(e.target.value)}
        inputMode="decimal"
        aria-label={`New price for ${sku}`}
        className="tabular h-8 w-24 text-right"
        autoFocus
      />
      <Input
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        placeholder="Reason"
        aria-label="Reason for the price change"
        className="h-8 w-32"
      />
      <Button
        size="sm"
        // The reason is enforced here as well as recorded downstream. A blank
        // audit column is the same as no audit column.
        disabled={busy || reason.trim().length < 3}
        onClick={() => void save()}
      >
        {busy ? <Loader2 className="animate-spin" /> : 'Save'}
      </Button>
      <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
        Cancel
      </Button>
    </div>
  );
}
