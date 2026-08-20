import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { Cart } from '@souq/contracts';

import { BffError, call } from '@/lib/bff';
import { CART_COOKIE, getAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../_problem';

/**
 * The current basket.
 *
 * Two identities are possible and they are not interchangeable. A signed-in
 * user's basket is keyed on their id and follows them between devices; a guest
 * basket is keyed on an opaque cookie and does not. Preferring the access token
 * when both are present is what makes signing in adopt the guest basket rather
 * than silently switch to an older one.
 */
export async function GET() {
  try {
    const accessToken = await getAccessToken();
    const cartId = (await cookies()).get(CART_COOKIE)?.value;

    if (!accessToken && !cartId) {
      // No user and no cookie: there is genuinely no basket yet. 404 rather
      // than an empty cart, so the client does not cache "empty" as a fact.
      return NextResponse.json(
        {
          type: 'https://errors.souq.dev/bff/cart-not-found',
          title: 'No basket',
          status: 404,
          code: 'CART_NOT_FOUND',
          instance: '/api/bff/cart',
          requestId: '',
          timestamp: new Date().toISOString(),
        },
        { status: 404, headers: { ...NO_STORE, 'Content-Type': 'application/problem+json' } },
      );
    }

    const cart = await call({
      service: 'cart',
      path: accessToken ? '/v1/carts/mine' : `/v1/carts/${encodeURIComponent(cartId!)}`,
      schema: Cart,
      accessToken: accessToken ?? undefined,
    });

    return NextResponse.json(cart, { headers: NO_STORE });
  } catch (error) {
    if (error instanceof BffError && error.status === 404) {
      return problemResponse(error, '/api/bff/cart');
    }
    return problemResponse(error, '/api/bff/cart');
  }
}
