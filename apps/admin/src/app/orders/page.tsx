import Link from 'next/link';
import { Suspense } from 'react';

import { OrderPage, formatMoney, isTerminal, type OrderStatus } from '@souq/contracts';

import { adminCall, gatherAdmin } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin } from '@/lib/session';
import { PanelUnavailable, Unauthorised } from '@/components/layout/unauthorised';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';

export const metadata = { title: 'Orders' };
export const dynamic = 'force-dynamic';

/**
 * Order search.
 *
 * Shows the **raw saga state**, not the customer-facing collapse into four
 * labels. That is the whole reason this screen exists separately from the
 * storefront's order history: when someone asks "why has this order not
 * shipped", `STOCK_RESERVED` and `COMPENSATING` are completely different
 * answers and the customer view calls both "Processing".
 */
export default async function OrdersPage({
  searchParams,
}: {
  searchParams: Promise<{ status?: string; q?: string }>;
}) {
  try {
    await requireAdmin();
  } catch (error) {
    if (error instanceof NotAuthorised) return <Unauthorised reason={error.reason} />;
    throw error;
  }

  const params = await searchParams;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-bold tracking-tight">Orders</h1>
        <p className="text-sm text-muted-foreground">
          Raw saga states. Open one to see which participant has not replied.
        </p>
      </div>

      <Suspense key={JSON.stringify(params)} fallback={<TableSkeleton />}>
        <OrderTable status={params.status} query={params.q} />
      </Suspense>
    </div>
  );
}

async function OrderTable({ status, query }: { status?: string; query?: string }) {
  const { accessToken } = await requireAdmin();

  const search = new URLSearchParams({ limit: '50' });
  if (status) search.set('status', status);
  if (query) search.set('q', query);

  const { page } = await gatherAdmin({
    page: adminCall({
      service: 'order',
      path: `/v1/admin/orders?${search.toString()}`,
      schema: OrderPage,
      accessToken,
    }),
  });

  if (!page) return <PanelUnavailable name="Orders" />;

  if (page.items.length === 0) {
    return <p className="py-12 text-center text-sm text-muted-foreground">No orders match.</p>;
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Placed</TableHead>
          <TableHead>Order</TableHead>
          <TableHead>Saga state</TableHead>
          <TableHead className="text-right">Total</TableHead>
          <TableHead>Payment</TableHead>
          <TableHead>Reservation</TableHead>
        </TableRow>
      </TableHeader>

      <TableBody>
        {page.items.map((order) => (
          <TableRow key={order.id}>
            <TableCell className="whitespace-nowrap text-xs">
              <time dateTime={order.placedAt}>
                {new Date(order.placedAt).toLocaleString('en-GB', {
                  year: 'numeric', month: 'short', day: '2-digit',
                  hour: '2-digit', minute: '2-digit', hour12: false,
                })}
              </time>
            </TableCell>

            <TableCell>
              <Link href={`/orders/${order.id}`} className="font-mono text-xs hover:underline">
                {order.id}
              </Link>
            </TableCell>

            <TableCell>
              <Badge variant={stateTone(order.status)} className="font-mono text-[10px]">
                {order.status}
              </Badge>
              {/*
                docs/DESIGN-INVARIANTS.md §1. Marked in the list, not just on the
                detail page, so nobody opens an order intending to cancel it and
                only then discovers they cannot.
              */}
              {isPastNoReturn(order.status) && (
                <span className="ml-2 text-[10px] uppercase tracking-wide text-muted-foreground">
                  no rollback
                </span>
              )}
            </TableCell>

            <TableCell className="tabular text-right">{formatMoney(order.total)}</TableCell>

            <TableCell className="font-mono text-[10px] text-muted-foreground">
              {order.paymentId ?? '—'}
            </TableCell>

            <TableCell className="font-mono text-[10px] text-muted-foreground">
              {order.reservationId ?? '—'}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

/** Mirrors saga.RollbackForbidden in the Go implementation. */
function isPastNoReturn(status: OrderStatus): boolean {
  return status === 'PAID' || status === 'STOCK_COMMITTED' || status === 'CONFIRMED';
}

function stateTone(status: OrderStatus): 'default' | 'secondary' | 'success' | 'destructive' | 'warning' {
  if (status === 'CANCELLED') return 'destructive';
  if (status === 'COMPENSATING') return 'warning';
  if (status === 'REFUNDED') return 'warning';
  if (status === 'CONFIRMED' || status === 'DELIVERED') return 'success';
  if (isTerminal(status)) return 'default';
  return 'secondary';
}

function TableSkeleton() {
  return (
    <div className="space-y-2">
      {Array.from({ length: 8 }, (_, i) => (
        <Skeleton key={i} className="h-11 w-full" />
      ))}
    </div>
  );
}
