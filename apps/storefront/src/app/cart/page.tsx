'use client';

import Link from 'next/link';
import { Loader2, ShoppingBag } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { CartLineRow } from '@/components/cart/cart-line-row';
import { CartSummary } from '@/components/cart/cart-summary';
import { useCart } from '@/components/cart/cart-provider';

/**
 * The basket.
 *
 * A client component, unlike the rest of the catalogue. The basket is
 * per-session, mutates constantly and must never be cached — rendering it on
 * the server would mean either a dynamic render on every request or, far worse,
 * one shopper's basket served to another from a CDN edge.
 */
export default function CartPage() {
  const { cart, loading } = useCart();

  if (!cart) {
    return loading ? <CartSkeleton /> : <EmptyCart />;
  }

  if (cart.lines.length === 0) {
    return <EmptyCart />;
  }

  return (
    <div className="container py-8">
      <h1 className="text-2xl font-bold tracking-tight">Your basket</h1>

      <div className="mt-6 grid gap-8 lg:grid-cols-[1fr_22rem]">
        <div className="relative">
          {/*
            The list dims while a mutation is in flight instead of being
            replaced by a spinner. Losing the layout on every quantity change
            makes the page feel like it is reloading, and the user loses their
            place in a long basket.
          */}
          {loading && (
            <div className="absolute inset-0 z-10 flex items-start justify-center pt-8">
              <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          )}

          <ul className={`divide-y transition-opacity ${loading ? 'opacity-50' : ''}`}>
            {cart.lines.map((line) => (
              <CartLineRow key={line.sku} line={line} />
            ))}
          </ul>
        </div>

        <div className="lg:sticky lg:top-24 lg:self-start">
          <CartSummary cart={cart} />
        </div>
      </div>
    </div>
  );
}

function EmptyCart() {
  return (
    <div className="container flex flex-col items-center py-24 text-center">
      <ShoppingBag className="h-10 w-10 text-muted-foreground" aria-hidden="true" />
      <h1 className="mt-4 text-xl font-semibold">Your basket is empty</h1>
      <p className="mt-2 text-sm text-muted-foreground">
        Once you add something, it will show up here.
      </p>
      <Button className="mt-6" asChild>
        <Link href="/search">Start browsing</Link>
      </Button>
    </div>
  );
}

function CartSkeleton() {
  return (
    <div className="container py-8">
      <Skeleton className="h-8 w-40" />
      <div className="mt-6 grid gap-8 lg:grid-cols-[1fr_22rem]">
        <div className="space-y-5">
          {[0, 1, 2].map((i) => (
            <div key={i} className="flex gap-4">
              <Skeleton className="h-24 w-24 shrink-0" />
              <div className="flex-1 space-y-2">
                <Skeleton className="h-4 w-2/3" />
                <Skeleton className="h-3 w-24" />
                <Skeleton className="h-10 w-32" />
              </div>
            </div>
          ))}
        </div>
        <Card>
          <CardContent className="space-y-3 p-6">
            <Skeleton className="h-5 w-32" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-11 w-full" />
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
