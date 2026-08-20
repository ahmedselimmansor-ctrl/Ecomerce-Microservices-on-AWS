'use client';

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from 'react';

import { Cart } from '@souq/contracts';

import { ApiError, apiFetch, newIdempotencyKey } from '@/lib/api-client';
import { toast } from '@/hooks/use-toast';

/**
 * The basket, shared across the app.
 *
 * The interesting part is `version`. The cart is optimistically concurrent: a
 * mutation carries the version the user was looking at, and cart-service
 * rejects it with `CART_STALE` if the cart moved on. That happens more often
 * than it sounds — two tabs, or a phone and a laptop, are ordinary.
 *
 * On `CART_STALE` this refetches and tells the user, rather than retrying
 * blindly. Retrying with the new version would silently reapply an action
 * against a basket the user never saw, which is exactly what the version check
 * exists to prevent.
 */

interface CartContextValue {
  cart: Cart | null;
  loading: boolean;
  /** Count of items, not of lines: three of one SKU is three. */
  itemCount: number;
  add: (sku: string, quantity: number) => Promise<boolean>;
  setQuantity: (sku: string, quantity: number) => Promise<boolean>;
  remove: (sku: string) => Promise<boolean>;
  applyCoupon: (code: string) => Promise<boolean>;
  refresh: () => Promise<void>;
}

const CartContext = createContext<CartContextValue | null>(null);

export function CartProvider({
  children,
  initialCart = null,
}: {
  children: React.ReactNode;
  initialCart?: Cart | null;
}) {
  const [cart, setCart] = useState<Cart | null>(initialCart);
  const [loading, setLoading] = useState(false);

  // Guards against a double-click firing two mutations whose responses arrive
  // out of order and leave the UI showing the earlier one.
  const inFlight = useRef(0);

  const refresh = useCallback(async () => {
    try {
      const next = await apiFetch('/api/bff/cart', { schema: Cart });
      setCart(next);
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        // No cart yet is the normal state for a new visitor, not an error.
        setCart(null);
        return;
      }
      console.error('[cart] refresh failed', err);
    }
  }, []);

  useEffect(() => {
    if (initialCart === null) void refresh();
  }, [initialCart, refresh]);

  const mutate = useCallback(
    async (
      run: (version: number) => Promise<Cart>,
      { successMessage }: { successMessage?: string } = {},
    ): Promise<boolean> => {
      const ticket = ++inFlight.current;
      setLoading(true);

      try {
        const next = await run(cart?.version ?? 0);

        // Drop a stale response. Without this a slow "add" resolving after a
        // fast "remove" resurrects the removed line.
        if (ticket === inFlight.current) {
          setCart(next);
          if (successMessage) toast({ title: successMessage, variant: 'success' });
        }
        return true;
      } catch (err) {
        if (!(err instanceof ApiError)) throw err;

        if (err.code === 'CART_STALE') {
          await refresh();
          toast({
            title: 'Your basket changed',
            description: 'We have refreshed it — please check and try again.',
          });
          return false;
        }

        toast({
          title: 'That did not work',
          description: err.userMessage,
          variant: 'destructive',
        });
        return false;
      } finally {
        if (ticket === inFlight.current) setLoading(false);
      }
    },
    [cart?.version, refresh],
  );

  const value = useMemo<CartContextValue>(
    () => ({
      cart,
      loading,
      itemCount: cart?.lines.reduce((sum, line) => sum + line.quantity, 0) ?? 0,

      add: (sku, quantity) =>
        mutate(
          () =>
            apiFetch('/api/bff/cart/lines', {
              schema: Cart,
              method: 'POST',
              body: { sku, quantity },
              // Adding to a basket moves stock intent, so it carries a key.
              // Without one a retried POST after a lost response adds twice.
              idempotencyKey: newIdempotencyKey(),
            }),
          { successMessage: 'Added to your basket' },
        ),

      setQuantity: (sku, quantity) =>
        mutate((version) =>
          apiFetch(`/api/bff/cart/lines/${encodeURIComponent(sku)}`, {
            schema: Cart,
            method: 'PATCH',
            body: { quantity, version },
          }),
        ),

      // Quantity 0 removes the line — one endpoint, one code path, so a
      // "remove" and a "set to zero" cannot diverge.
      remove: (sku) =>
        mutate((version) =>
          apiFetch(`/api/bff/cart/lines/${encodeURIComponent(sku)}`, {
            schema: Cart,
            method: 'PATCH',
            body: { quantity: 0, version },
          }),
        ),

      applyCoupon: (code) =>
        mutate(
          () =>
            apiFetch('/api/bff/cart/coupons', {
              schema: Cart,
              method: 'POST',
              body: { code },
            }),
        ),

      refresh,
    }),
    [cart, loading, mutate, refresh],
  );

  return <CartContext.Provider value={value}>{children}</CartContext.Provider>;
}

export function useCart(): CartContextValue {
  const context = useContext(CartContext);
  if (!context) {
    throw new Error('useCart must be used inside a CartProvider');
  }
  return context;
}
