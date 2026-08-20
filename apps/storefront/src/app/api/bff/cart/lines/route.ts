import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { AddToCartRequest, Cart } from '@souq/contracts';

import { call } from '@/lib/bff';
import { CART_COOKIE, getAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

/** A guest basket outlives a session but not forever. Thirty days matches cart-service's TTL. */
const CART_COOKIE_MAX_AGE = 60 * 60 * 24 * 30;

/**
 * Add a line.
 *
 * Creates the basket on first use and persists its id, so an anonymous visitor
 * who adds something and closes the tab still has it tomorrow.
 *
 * The client's `Idempotency-Key` is forwarded rather than regenerated. That is
 * the whole mechanism: a retried POST after a lost response must be recognised
 * by cart-service as the same logical add, and minting a fresh key here would
 * make every retry a second item.
 */
export async function POST(request: Request) {
  try {
    const body = AddToCartRequest.parse(await request.json());

    const accessToken = await getAccessToken();
    const jar = await cookies();
    const cartId = jar.get(CART_COOKIE)?.value;

    const cart = await call({
      service: 'cart',
      path: accessToken
        ? '/v1/carts/mine/lines'
        : cartId
          ? `/v1/carts/${encodeURIComponent(cartId)}/lines`
          : '/v1/carts/lines',
      schema: Cart,
      method: 'POST',
      body,
      accessToken: accessToken ?? undefined,
      idempotencyKey: request.headers.get('Idempotency-Key') ?? undefined,
    });

    const response = NextResponse.json(cart, { headers: NO_STORE });

    // Persist the id whenever it is new — including the case where the user is
    // signed in, so the basket survives a sign-out on the same device.
    if (cart.id !== cartId) {
      response.cookies.set({
        name: CART_COOKIE,
        value: cart.id,
        // Not HttpOnly: this is an opaque lookup key with no authority, and
        // cart-service treats it as such. Marking it HttpOnly would imply a
        // protection it does not provide.
        httpOnly: false,
        secure: process.env.NODE_ENV === 'production',
        sameSite: 'lax',
        path: '/',
        maxAge: CART_COOKIE_MAX_AGE,
      });
    }

    return response;
  } catch (error) {
    return problemResponse(error, '/api/bff/cart/lines');
  }
}
