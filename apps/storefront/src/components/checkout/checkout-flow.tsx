'use client';

import { useMemo, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import Link from 'next/link';
import { AlertTriangle, Loader2, Lock } from 'lucide-react';

import { Address, CreateOrderResponse, formatMoney } from '@souq/contracts';

import { ApiError, apiFetch, newIdempotencyKey } from '@/lib/api-client';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Separator } from '@/components/ui/separator';
import { useCart } from '@/components/cart/cart-provider';
import { useSession } from '@/components/auth/session-provider';

/**
 * The checkout form.
 *
 * Three decisions here are the ones that matter.
 *
 * **The idempotency key is minted once, when the component mounts**, and reused
 * across every retry of this submission. That is the entire mechanism: a
 * customer who clicks Place Order, sees nothing happen, and clicks again sends
 * the same key, and order-service recognises the second as a replay. A key
 * generated per click would make every impatient double-click a second order —
 * the most expensive duplicate in the system.
 *
 * **`expectedTotal` and `cartVersion` are sent with the order.** order-service
 * rejects the request if either has moved. Without them a price change landing
 * between the basket being displayed and the button being pressed charges a
 * number the customer never agreed to.
 *
 * **On 202 we navigate to the status page, not to a confirmation.** The saga has
 * started; it has not finished.
 */
export function CheckoutFlow() {
  const router = useRouter();
  const { cart, loading } = useCart();
  const { user } = useSession();

  // Minted once per mount. See above — this is load-bearing.
  const idempotencyKey = useRef(newIdempotencyKey());

  const [address, setAddress] = useState({
    recipient: user?.fullName ?? '',
    line1: '',
    line2: '',
    city: '',
    region: '',
    postalCode: '',
    countryCode: 'EG',
    phone: '',
  });

  const [error, setError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  const canSubmit = useMemo(
    () => Boolean(cart && cart.lines.length > 0 && !loading && !submitting),
    [cart, loading, submitting],
  );

  if (!user) {
    return (
      <Alert className="mt-6">
        <Lock className="h-4 w-4" />
        <AlertTitle>Sign in to check out</AlertTitle>
        <AlertDescription className="space-y-3">
          <p>We need an account to send your order confirmation and let you track it.</p>
          <Button size="sm" asChild>
            <Link href="/login?next=/checkout">Sign in</Link>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (!cart || cart.lines.length === 0) {
    return (
      <Alert className="mt-6">
        <AlertTitle>Your basket is empty</AlertTitle>
        <AlertDescription className="space-y-3">
          <Button size="sm" variant="outline" asChild>
            <Link href="/search">Find something</Link>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault();
    if (!cart) return;

    setError(null);
    setFieldErrors({});

    const parsed = Address.safeParse({
      ...address,
      line2: address.line2 || undefined,
      region: address.region || undefined,
      phone: address.phone || undefined,
    });

    if (!parsed.success) {
      // Validated client-side first purely to save a round trip. The server
      // validates the same schema and its answer is the one that counts.
      setFieldErrors(
        Object.fromEntries(
          parsed.error.issues.map((issue) => [String(issue.path[0]), issue.message]),
        ),
      );
      return;
    }

    setSubmitting(true);

    try {
      const result = await apiFetch('/api/bff/orders', {
        schema: CreateOrderResponse,
        method: 'POST',
        idempotencyKey: idempotencyKey.current,
        body: {
          cartId: cart.id,
          cartVersion: cart.version,
          shippingAddress: parsed.data,
          // A placeholder token. A real integration collects this from Paymob's
          // hosted fields so a card number never touches this origin — which is
          // the difference between SAQ A and a full PCI DSS assessment.
          paymentMethodToken: 'tok_test_visa',
          expectedTotal: cart.total,
        },
      });

      // 202. The saga has started, not finished — so this goes to the status
      // page, which subscribes to the outcome.
      router.push(`/orders/${result.orderId}`);
    } catch (err) {
      if (!(err instanceof ApiError)) throw err;

      if (err.code === 'CART_STALE') {
        setError('Your basket changed while you were checking out. Please review it and try again.');
      } else {
        setError(err.userMessage);
      }

      if (err.fieldErrors) {
        setFieldErrors(
          Object.fromEntries(err.fieldErrors.map((f) => [f.field.split('.').pop() ?? f.field, f.message])),
        );
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form onSubmit={onSubmit} className="mt-6 grid gap-8 lg:grid-cols-[1fr_20rem]">
      <div className="space-y-6">
        {error && (
          <Alert variant="destructive">
            <AlertTriangle className="h-4 w-4" />
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}

        <section>
          <h2 className="font-semibold">Delivery address</h2>

          <div className="mt-4 grid gap-4 sm:grid-cols-2">
            <Field
              id="recipient" label="Recipient" className="sm:col-span-2"
              value={address.recipient} error={fieldErrors.recipient}
              autoComplete="name"
              onChange={(v) => setAddress((a) => ({ ...a, recipient: v }))}
            />
            <Field
              id="line1" label="Address" className="sm:col-span-2"
              value={address.line1} error={fieldErrors.line1}
              autoComplete="address-line1"
              onChange={(v) => setAddress((a) => ({ ...a, line1: v }))}
            />
            <Field
              id="line2" label="Apartment, floor (optional)" className="sm:col-span-2"
              value={address.line2} error={fieldErrors.line2}
              autoComplete="address-line2" required={false}
              onChange={(v) => setAddress((a) => ({ ...a, line2: v }))}
            />
            <Field
              id="city" label="City" value={address.city} error={fieldErrors.city}
              autoComplete="address-level2"
              onChange={(v) => setAddress((a) => ({ ...a, city: v }))}
            />
            <Field
              id="region" label="Governorate (optional)" value={address.region}
              error={fieldErrors.region} autoComplete="address-level1" required={false}
              onChange={(v) => setAddress((a) => ({ ...a, region: v }))}
            />
            <Field
              id="postalCode" label="Postcode" value={address.postalCode}
              error={fieldErrors.postalCode} autoComplete="postal-code"
              onChange={(v) => setAddress((a) => ({ ...a, postalCode: v }))}
            />
            <Field
              id="phone" label="Phone (optional)" value={address.phone} error={fieldErrors.phone}
              autoComplete="tel" required={false} inputMode="tel"
              placeholder="+201234567890"
              onChange={(v) => setAddress((a) => ({ ...a, phone: v }))}
            />
          </div>
        </section>

        <Separator />

        <section>
          <h2 className="font-semibold">Payment</h2>
          <Alert className="mt-3">
            <Lock className="h-4 w-4" />
            <AlertDescription className="text-xs">
              Card details are collected by Paymob in a hosted field and never reach this site.
              This build submits a test token.
            </AlertDescription>
          </Alert>
        </section>
      </div>

      <div className="lg:sticky lg:top-24 lg:self-start">
        <Card>
          <CardContent className="space-y-4 p-6">
            <h2 className="font-semibold">Your order</h2>

            <ul className="space-y-2 text-sm">
              {cart.lines.map((line) => (
                <li key={line.sku} className="flex justify-between gap-3">
                  <span className="min-w-0 truncate">
                    <span className="tabular text-muted-foreground">{line.quantity}×</span>{' '}
                    {line.title}
                  </span>
                  <span className="tabular shrink-0">{formatMoney(line.lineTotal)}</span>
                </li>
              ))}
            </ul>

            <Separator />

            <div className="flex items-baseline justify-between">
              <span className="font-semibold">Total</span>
              <span className="tabular text-xl font-bold">{formatMoney(cart.total)}</span>
            </div>

            <Button type="submit" size="lg" className="w-full" disabled={!canSubmit}>
              {submitting && <Loader2 className="animate-spin" />}
              Place order
            </Button>

            <p className="text-center text-xs text-muted-foreground">
              Stock is confirmed after you place the order. We will tell you within a few seconds.
            </p>
          </CardContent>
        </Card>
      </div>
    </form>
  );
}

function Field({
  id, label, value, onChange, error, className, required = true, ...rest
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  error?: string;
  className?: string;
  required?: boolean;
} & Omit<React.InputHTMLAttributes<HTMLInputElement>, 'onChange' | 'value' | 'id'>) {
  return (
    <div className={className}>
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        value={value}
        required={required}
        onChange={(e) => onChange(e.target.value)}
        // Both, not just one. `aria-invalid` styles the field and announces the
        // state; `aria-describedby` is what actually reads the message out.
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? `${id}-error` : undefined}
        className="mt-1.5"
        {...rest}
      />
      {error && (
        <p id={`${id}-error`} className="mt-1 text-xs text-destructive">
          {error}
        </p>
      )}
    </div>
  );
}
