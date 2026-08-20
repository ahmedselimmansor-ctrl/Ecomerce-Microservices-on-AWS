import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { ApplyCouponRequest, Cart } from '@souq/contracts';

import { call } from '@/lib/bff';
import { CART_COOKIE, getAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

/**
 * Apply a discount code.
 *
 * A rejected code comes back as part of the cart in `rejectedCoupons`, not as
 * an error. That is deliberate on cart-service's side and this handler
 * preserves it: the request succeeded, the basket is valid, and one code did
 * not apply. Turning it into a 4xx would lose the recomputed totals that came
 * back with it.
 */
export async function POST(request: Request) {
  try {
    const body = ApplyCouponRequest.parse(await request.json());

    const accessToken = await getAccessToken();
    const cartId = (await cookies()).get(CART_COOKIE)?.value;

    const cart = await call({
      service: 'cart',
      path: accessToken
        ? '/v1/carts/mine/coupons'
        : `/v1/carts/${encodeURIComponent(cartId ?? '')}/coupons`,
      schema: Cart,
      method: 'POST',
      body,
      accessToken: accessToken ?? undefined,
    });

    return NextResponse.json(cart, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/cart/coupons');
  }
}
