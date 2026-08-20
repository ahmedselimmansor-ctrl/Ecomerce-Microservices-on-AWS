'use client';

import { useEffect, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import { CheckCircle2, Circle, Loader2, XCircle } from 'lucide-react';

import { OrderStatusResponse, isTerminal, type OrderStatus } from '@souq/contracts';
import { cn } from '@/lib/utils';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';

/**
 * The checkout progress view.
 *
 * This component exists because checkout is asynchronous and the UI has to be
 * honest about that. `POST /v1/orders` returns 202: the saga has started, not
 * finished. Showing a success page at that moment would be a lie roughly one
 * time in ten, when the card is declined a second later.
 *
 * Two transport mechanisms, in order of preference:
 *
 *   SSE      the server pushes each transition. A saga usually settles in
 *            under two seconds, and four poll round-trips to discover that is
 *            wasted work at scale.
 *   polling  the fallback. Corporate proxies and some mobile networks still
 *            break EventSource, and a checkout that hangs behind a proxy is
 *            not an acceptable failure mode.
 */

const STEPS: { status: OrderStatus; label: string; detail: string }[] = [
  { status: 'PENDING',         label: 'Order received',   detail: 'Checking availability' },
  { status: 'STOCK_RESERVED',  label: 'Items reserved',   detail: 'Authorising payment' },
  { status: 'PAID',            label: 'Payment approved', detail: 'Confirming your items' },
  { status: 'STOCK_COMMITTED', label: 'Items confirmed',  detail: 'Completing payment' },
  { status: 'CONFIRMED',       label: 'Order confirmed',  detail: 'All done' },
];

const stepIndex = (s: OrderStatus): number => {
  const i = STEPS.findIndex((step) => step.status === s);
  // COMPENSATING and CANCELLED are not on the happy path; the UI shows the
  // failure panel instead of a position on this track.
  return i === -1 ? 0 : i;
};

/**
 * Copy for each way an order can fail. Keyed on the machine-readable
 * reasonCode, never on the human-readable detail — the detail is written for
 * an engineer reading a log, and it changes.
 */
const FAILURE_COPY: Record<string, { title: string; body: string; retry: boolean }> = {
  INSUFFICIENT_STOCK: {
    title: 'Some items sold out',
    body: 'They went while you were checking out. Nothing has been charged.',
    retry: false,
  },
  PAYMENT_DECLINED: {
    title: 'Your payment was declined',
    body: 'Your bank turned down the payment. Nothing has been charged — try another card.',
    retry: true,
  },
  PAYMENT_TIMEOUT: {
    title: 'Payment took too long',
    body: 'We could not reach your bank in time and released your items. Nothing has been charged.',
    retry: true,
  },
  RESERVATION_TIMEOUT: {
    title: 'Checkout timed out',
    body: 'Your items were released. Nothing has been charged.',
    retry: true,
  },
  CUSTOMER_CANCELLED: {
    title: 'Order cancelled',
    body: 'You cancelled this order. Nothing has been charged.',
    retry: true,
  },
  FRAUD_REJECTED: {
    // Deliberately vague. Telling someone precisely why a fraud check fired
    // tells them how to get around it next time.
    title: 'We could not complete this order',
    body: 'Please contact support if you think this is a mistake.',
    retry: false,
  },
};

interface Props {
  orderId: string;
  /**
   * The status the server already knew, when it knew one.
   *
   * Optional, because there are two entries to this component and they differ.
   * Checkout arrives holding the 202's `status` and should render it without a
   * flash. A direct navigation to /orders/{id} — a bookmark, a link from an
   * email — has nothing, and defaulting to PENDING there is correct: it is the
   * one state from which every other is reachable, so the first poll can only
   * move the UI forwards. Defaulting to a terminal state would instead mark the
   * order settled and never poll at all.
   */
  initialStatus?: OrderStatus;
}

export function OrderProgress({ orderId, initialStatus = 'PENDING' }: Props) {
  const router = useRouter();
  const [state, setState] = useState<{
    status: OrderStatus;
    reason: string | null;
    unavailable: { sku: string; requested: number; available: number }[];
  }>({ status: initialStatus, reason: null, unavailable: [] });

  const [stalled, setStalled] = useState(false);
  const settled = useRef(isTerminal(initialStatus));

  useEffect(() => {
    if (settled.current) return;

    let cancelled = false;
    let source: EventSource | null = null;
    let pollTimer: ReturnType<typeof setTimeout> | null = null;

    const apply = (raw: unknown) => {
      // Parsed here too, not just in the BFF. This payload arrives over SSE
      // straight from a Route Handler, and an unvalidated `status` string
      // would render as an unknown step rather than failing visibly.
      const parsed = OrderStatusResponse.safeParse(raw);
      if (!parsed.success || cancelled) return;

      setState({
        status: parsed.data.status,
        reason: parsed.data.cancellationReason,
        unavailable: parsed.data.unavailable ?? [],
      });

      if (parsed.data.terminal) {
        settled.current = true;
        source?.close();
        if (pollTimer) clearTimeout(pollTimer);

        if (parsed.data.status === 'CONFIRMED') {
          // Refresh so the server component re-renders with the confirmed
          // order rather than us reconstructing it on the client.
          router.refresh();
        }
      }
    };

    // Poll as the fallback. Backs off from 500ms so an order that takes longer
    // than usual does not generate hundreds of requests while it settles.
    let delay = 500;
    const poll = async () => {
      if (cancelled || settled.current) return;
      try {
        const res = await fetch(`/api/bff/orders/${orderId}/status`, { cache: 'no-store' });
        if (res.ok) apply(await res.json());
      } catch {
        // Network blip. The next tick retries; no need to surface it.
      }
      if (!cancelled && !settled.current) {
        delay = Math.min(delay * 1.5, 5_000);
        pollTimer = setTimeout(poll, delay);
      }
    };

    if (typeof EventSource !== 'undefined') {
      source = new EventSource(`/api/bff/orders/${orderId}/stream`);
      source.addEventListener('status', (e) => {
        try {
          apply(JSON.parse((e as MessageEvent).data));
        } catch {
          /* malformed frame; the poller is still running */
        }
      });
      source.onerror = () => {
        // Proxy or network killed the stream. Fall back rather than leaving
        // the customer staring at a spinner.
        source?.close();
        source = null;
        if (!settled.current) poll();
      };
    }

    // Belt and braces: run the poller alongside SSE at a slow cadence, so a
    // stream that silently stops delivering (rather than erroring) still
    // resolves.
    pollTimer = setTimeout(poll, 2_000);

    // If the saga has not settled in 90 seconds, stop implying it is about to.
    // Something is genuinely wrong and the customer deserves to be told what
    // to do rather than watching an animation.
    const stallTimer = setTimeout(() => {
      if (!settled.current && !cancelled) setStalled(true);
    }, 90_000);

    return () => {
      cancelled = true;
      source?.close();
      if (pollTimer) clearTimeout(pollTimer);
      clearTimeout(stallTimer);
    };
  }, [orderId, router]);

  // ---------------------------------------------------------------- failure

  if (state.status === 'CANCELLED') {
    const copy = FAILURE_COPY[state.reason ?? ''] ?? {
      title: 'We could not complete your order',
      body: 'Nothing has been charged.',
      retry: true,
    };

    return (
      <div className="mx-auto w-full max-w-lg space-y-6">
        <Alert variant="destructive">
          <XCircle className="size-5" aria-hidden />
          <AlertTitle>{copy.title}</AlertTitle>
          <AlertDescription className="space-y-3">
            <p>{copy.body}</p>

            {state.unavailable.length > 0 && (
              <ul className="space-y-1 text-sm">
                {state.unavailable.map((u) => (
                  <li key={u.sku}>
                    <span className="font-mono text-xs">{u.sku}</span>
                    {' — '}
                    {u.available === 0
                      ? 'out of stock'
                      : `only ${u.available} left, you wanted ${u.requested}`}
                  </li>
                ))}
              </ul>
            )}
          </AlertDescription>
        </Alert>

        <div className="flex gap-3">
          {copy.retry && (
            <Button onClick={() => router.push('/checkout')} className="flex-1">
              Try again
            </Button>
          )}
          <Button variant="outline" onClick={() => router.push('/cart')} className="flex-1">
            Back to cart
          </Button>
        </div>

        {/* The order id is the only thing support can act on. Always visible
            on a failure, never buried in a details panel. */}
        <p className="text-center text-xs text-muted-foreground">
          Reference <span className="font-mono">{orderId}</span>
        </p>
      </div>
    );
  }

  // ---------------------------------------------------------------- success

  if (state.status === 'CONFIRMED') {
    return (
      <div className="mx-auto w-full max-w-lg space-y-6 text-center">
        <CheckCircle2 className="mx-auto size-14 text-emerald-600" aria-hidden />
        <div className="space-y-1">
          <h1 className="text-2xl font-semibold tracking-tight">Order confirmed</h1>
          <p className="text-muted-foreground">
            We have emailed your receipt. Reference{' '}
            <span className="font-mono text-sm">{orderId}</span>
          </p>
        </div>
        <Button onClick={() => router.push(`/orders/${orderId}`)} className="w-full">
          View your order
        </Button>
      </div>
    );
  }

  // ---------------------------------------------------------------- pending

  const current = stepIndex(state.status);
  const compensating = state.status === 'COMPENSATING';

  return (
    <div
      className="mx-auto w-full max-w-lg space-y-8"
      // Screen readers get told about each transition without the whole
      // subtree being re-announced.
      aria-live="polite"
      aria-busy={!settled.current}
    >
      <div className="space-y-1 text-center">
        <h1 className="text-2xl font-semibold tracking-tight">
          {compensating ? 'Releasing your items' : 'Placing your order'}
        </h1>
        <p className="text-sm text-muted-foreground">
          {compensating
            ? 'Something went wrong. Undoing everything now — nothing will be charged.'
            : 'This usually takes a couple of seconds. Please do not close this page.'}
        </p>
      </div>

      <ol className="space-y-1">
        {STEPS.map((step, i) => {
          const done = i < current;
          const active = i === current && !compensating;

          return (
            <li
              key={step.status}
              className={cn(
                'flex items-start gap-3 rounded-lg px-3 py-2.5 transition-colors',
                active && 'bg-muted',
              )}
            >
              <span className="mt-0.5 shrink-0" aria-hidden>
                {done ? (
                  <CheckCircle2 className="size-5 text-emerald-600" />
                ) : active ? (
                  <Loader2 className="size-5 animate-spin text-primary" />
                ) : (
                  <Circle className="size-5 text-muted-foreground/40" />
                )}
              </span>

              <span className="min-w-0">
                <span
                  className={cn(
                    'block text-sm font-medium',
                    !done && !active && 'text-muted-foreground/60',
                  )}
                >
                  {step.label}
                </span>
                {active && (
                  <span className="block text-xs text-muted-foreground">{step.detail}</span>
                )}
              </span>

              {/* Screen-reader-only status, so the visual iconography is not
                  the only way to know where the order is. */}
              <span className="sr-only">
                {done ? 'completed' : active ? 'in progress' : 'not started'}
              </span>
            </li>
          );
        })}
      </ol>

      {stalled && (
        <Alert>
          <AlertTitle>This is taking longer than usual</AlertTitle>
          <AlertDescription className="space-y-2 text-sm">
            <p>
              Your order is still being processed and it is safe to leave this page — we will
              email you as soon as it settles. You have not been charged twice.
            </p>
            <p>
              Reference <span className="font-mono">{orderId}</span>
            </p>
          </AlertDescription>
        </Alert>
      )}
    </div>
  );
}
