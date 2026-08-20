'use client';

import { useEffect, useState } from 'react';
import Link from 'next/link';
import Image from 'next/image';
import { Lock, Package } from 'lucide-react';

import { OrderPage, formatMoney, isTerminal, type Order, type OrderStatus } from '@souq/contracts';

import { ApiError, apiFetch } from '@/lib/api-client';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { useSession } from '@/components/auth/session-provider';

/**
 * Order history.
 *
 * The status mapping is the interesting part. The saga has ten states and the
 * customer should see four. `STOCK_RESERVED`, `PAID` and `STOCK_COMMITTED` are
 * all "we are working on it" from outside — exposing them would invite
 * questions nobody at support can usefully answer, and `COMPENSATING` in
 * particular reads as a technical failure when it is the system correctly
 * undoing itself.
 *
 * The raw value is still available on the status page, because support and the
 * admin saga inspector genuinely need it.
 */
export function OrderHistory() {
  const { user, loading: sessionLoading } = useSession();
  const [orders, setOrders] = useState<Order[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!user) return;

    let cancelled = false;

    apiFetch('/api/bff/orders/history', { schema: OrderPage })
      .then((page) => {
        if (!cancelled) setOrders(page.items);
      })
      .catch((err) => {
        if (cancelled) return;
        setError(err instanceof ApiError ? err.userMessage : 'Could not load your orders.');
      });

    return () => {
      cancelled = true;
    };
  }, [user]);

  if (sessionLoading) return <ListSkeleton />;

  if (!user) {
    return (
      <Alert>
        <Lock className="h-4 w-4" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>Sign in to see your orders.</span>
          <Button size="sm" asChild>
            <Link href="/login?next=/account/orders">Sign in</Link>
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  if (error) {
    return (
      <Alert variant="destructive">
        <AlertDescription>{error}</AlertDescription>
      </Alert>
    );
  }

  if (orders === null) return <ListSkeleton />;

  if (orders.length === 0) {
    return (
      <div className="flex flex-col items-center py-16 text-center">
        <Package className="h-10 w-10 text-muted-foreground" aria-hidden="true" />
        <h2 className="mt-4 text-lg font-semibold">No orders yet</h2>
        <Button className="mt-6" asChild>
          <Link href="/search">Start browsing</Link>
        </Button>
      </div>
    );
  }

  return (
    <ul className="space-y-4">
      {orders.map((order) => {
        const view = customerView(order.status);

        return (
          <li key={order.id}>
            <Card>
              <CardContent className="p-5">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div>
                    <p className="text-sm font-medium">
                      <Link href={`/orders/${order.id}`} className="hover:underline">
                        Order {order.id}
                      </Link>
                    </p>
                    <time
                      dateTime={order.placedAt}
                      className="text-xs text-muted-foreground"
                    >
                      {new Date(order.placedAt).toLocaleDateString('en-GB', {
                        year: 'numeric', month: 'long', day: 'numeric',
                      })}
                    </time>
                  </div>

                  <div className="flex items-center gap-3">
                    <Badge variant={view.tone}>{view.label}</Badge>
                    <span className="tabular font-semibold">{formatMoney(order.total)}</span>
                  </div>
                </div>

                <ul className="mt-4 flex flex-wrap gap-2">
                  {order.lines.slice(0, 5).map((line) => (
                    <li
                      key={line.sku}
                      className="relative h-14 w-14 overflow-hidden rounded-md bg-muted"
                    >
                      {line.image ? (
                        <Image src={line.image} alt={line.title} fill sizes="56px" className="object-cover" />
                      ) : null}
                    </li>
                  ))}
                  {order.lines.length > 5 && (
                    <li className="tabular flex h-14 w-14 items-center justify-center rounded-md bg-muted text-xs text-muted-foreground">
                      +{order.lines.length - 5}
                    </li>
                  )}
                </ul>

                {/*
                  Only offered while it is genuinely possible. docs/DESIGN-INVARIANTS.md §1:
                  past PAID there is no compensating transition, so a cancel
                  button there is a button that cannot work — and offering one
                  is worse than not offering it.
                */}
                {!isTerminal(order.status) && order.status === 'PENDING' && (
                  <Button variant="outline" size="sm" className="mt-4" asChild>
                    <Link href={`/orders/${order.id}`}>Track or cancel</Link>
                  </Button>
                )}
              </CardContent>
            </Card>
          </li>
        );
      })}
    </ul>
  );
}

/** Ten saga states collapsed to the four a customer can act on. */
function customerView(status: OrderStatus): {
  label: string;
  tone: 'default' | 'secondary' | 'success' | 'destructive' | 'warning';
} {
  switch (status) {
    case 'PENDING':
    case 'STOCK_RESERVED':
    case 'PAID':
    case 'STOCK_COMMITTED':
    case 'COMPENSATING':
      return { label: 'Processing', tone: 'secondary' };
    case 'CONFIRMED':
      return { label: 'Confirmed', tone: 'success' };
    case 'SHIPPED':
      return { label: 'Shipped', tone: 'default' };
    case 'DELIVERED':
      return { label: 'Delivered', tone: 'success' };
    case 'CANCELLED':
      return { label: 'Cancelled', tone: 'destructive' };
    case 'REFUNDED':
      return { label: 'Refunded', tone: 'warning' };
  }
}

function ListSkeleton() {
  return (
    <div className="space-y-4">
      {[0, 1, 2].map((i) => (
        <Card key={i}>
          <CardContent className="space-y-3 p-5">
            <Skeleton className="h-4 w-48" />
            <Skeleton className="h-3 w-32" />
            <div className="flex gap-2">
              {[0, 1, 2].map((j) => (
                <Skeleton key={j} className="h-14 w-14" />
              ))}
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
