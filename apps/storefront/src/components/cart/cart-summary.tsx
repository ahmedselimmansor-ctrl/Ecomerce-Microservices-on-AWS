'use client';

import Link from 'next/link';
import { Info } from 'lucide-react';

import { formatMoney, type Cart } from '@souq/contracts';

import { Alert, AlertDescription } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Separator } from '@/components/ui/separator';
import { CouponForm } from './coupon-form';

/**
 * The totals panel.
 *
 * `pricingDegraded` gets a visible notice. When pricing-engine is unreachable
 * the cart falls back to list price and is still chargeable at those numbers —
 * but promotional messaging is hidden rather than shown against totals we
 * cannot justify. Selling at list price is a bad day; charging a promotional
 * price we did not compute is a refund.
 */
export function CartSummary({ cart, locale = 'en-GB' }: { cart: Cart; locale?: string }) {
  return (
    <Card>
      <CardContent className="space-y-4 p-6">
        <h2 className="font-semibold">Order summary</h2>

        {cart.pricingDegraded && (
          <Alert variant="warning">
            <Info className="h-4 w-4" />
            <AlertDescription className="text-xs">
              Promotions are temporarily unavailable. These are list prices, and they are what you
              will be charged.
            </AlertDescription>
          </Alert>
        )}

        <dl className="space-y-2 text-sm">
          <Row label="Subtotal" value={formatMoney(cart.subtotal, locale)} />

          {cart.discountTotal.amount !== 0 && (
            <Row
              label="Discount"
              value={formatMoney(cart.discountTotal, locale)}
              className="text-success"
            />
          )}

          <Row
            label="Delivery"
            value={
              cart.shippingTotal.amount === 0
                ? 'Free'
                : formatMoney(cart.shippingTotal, locale)
            }
          />

          {cart.taxTotal.amount !== 0 && (
            <Row label="VAT" value={formatMoney(cart.taxTotal, locale)} />
          )}
        </dl>

        <Separator />

        <div className="flex items-baseline justify-between">
          <span className="font-semibold">Total</span>
          <span className="tabular text-xl font-bold">{formatMoney(cart.total, locale)}</span>
        </div>

        {!cart.pricingDegraded && <CouponForm cart={cart} locale={locale} />}

        <Button size="lg" className="w-full" asChild disabled={cart.lines.length === 0}>
          <Link href="/checkout">Checkout</Link>
        </Button>

        <p className="text-center text-xs text-muted-foreground">
          Stock is confirmed when you place your order, not before.
        </p>
      </CardContent>
    </Card>
  );
}

function Row({ label, value, className }: { label: string; value: string; className?: string }) {
  return (
    <div className="flex justify-between">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={`tabular ${className ?? ''}`}>{value}</dd>
    </div>
  );
}
