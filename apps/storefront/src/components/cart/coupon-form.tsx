'use client';

import { useState } from 'react';
import { Loader2, Tag, X } from 'lucide-react';

import { formatMoney, type Cart } from '@souq/contracts';

import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { useCart } from './cart-provider';

/**
 * Coupon entry.
 *
 * `rejectedCoupons` is rendered with the same weight as accepted ones. A code
 * that silently does nothing is the worst outcome here: the customer completes
 * checkout believing a discount applied, and finds out from the receipt.
 */
export function CouponForm({ cart, locale = 'en-GB' }: { cart: Cart; locale?: string }) {
  const { applyCoupon, loading } = useCart();
  const [code, setCode] = useState('');
  const [submitting, setSubmitting] = useState(false);

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    if (!code.trim()) return;

    setSubmitting(true);
    const ok = await applyCoupon(code.trim());
    setSubmitting(false);
    // Clear only on success, so a typo stays in the box to be corrected.
    if (ok) setCode('');
  }

  return (
    <div className="space-y-3">
      {cart.appliedCoupons.length > 0 && (
        <ul className="space-y-1.5">
          {cart.appliedCoupons.map((coupon) => (
            <li key={coupon.code} className="flex items-center justify-between gap-2 text-sm">
              <Badge variant="success" className="gap-1">
                <Tag className="h-3 w-3" />
                {coupon.code}
              </Badge>
              <span className="tabular text-success">
                −{formatMoney({ ...coupon.discount, amount: Math.abs(coupon.discount.amount) }, locale)}
              </span>
            </li>
          ))}
        </ul>
      )}

      {cart.rejectedCoupons.length > 0 && (
        <ul className="space-y-1.5">
          {cart.rejectedCoupons.map((coupon) => (
            <li key={coupon.code} className="flex items-center gap-2 text-xs text-destructive">
              <X className="h-3.5 w-3.5 shrink-0" />
              <span>
                <span className="font-medium">{coupon.code}</span> — {rejectionCopy(coupon.reasonCode)}
              </span>
            </li>
          ))}
        </ul>
      )}

      <form onSubmit={onSubmit} className="flex gap-2">
        <label htmlFor="coupon" className="sr-only">
          Discount code
        </label>
        <Input
          id="coupon"
          value={code}
          onChange={(e) => setCode(e.target.value.toUpperCase())}
          placeholder="Discount code"
          autoComplete="off"
          spellCheck={false}
          className="h-10"
        />
        <Button type="submit" variant="outline" size="sm" className="h-10" disabled={submitting || loading}>
          {submitting ? <Loader2 className="animate-spin" /> : 'Apply'}
        </Button>
      </form>
    </div>
  );
}

/** Keyed on the machine code, never on server prose. */
function rejectionCopy(reasonCode: string): string {
  switch (reasonCode) {
    case 'EXPIRED':          return 'this code has expired';
    case 'NOT_FOUND':        return 'we do not recognise this code';
    case 'MINIMUM_NOT_MET':  return 'your basket is below the minimum for this code';
    case 'ALREADY_USED':     return 'this code has already been used';
    case 'NOT_ELIGIBLE':     return 'this code does not apply to anything in your basket';
    case 'USAGE_LIMIT':      return 'this code has reached its usage limit';
    default:                 return 'this code cannot be applied';
  }
}
