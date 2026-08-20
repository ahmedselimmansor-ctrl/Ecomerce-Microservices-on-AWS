import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { Cart, UpdateCartLineRequest } from '@souq/contracts';

import { call } from '@/lib/bff';
import { CART_COOKIE, getAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../../_problem';

/**
 * Change or remove a line.
 *
 * Quantity 0 removes it — one endpoint rather than a separate DELETE, so
 * "set to zero" and "remove" cannot drift apart in behaviour.
 *
 * The body carries `version`. cart-service rejects a mismatch with `CART_STALE`,
 * which is what stops a second tab's stale view from overwriting a change made
 * in the first. This handler passes it straight through and does **not** retry
 * on conflict: retrying with a refreshed version would reapply the action
 * against a basket the user never saw.
 */
export async function PATCH(
  request: Request,
  { params }: { params: Promise<{ sku: string }> },
) {
  const { sku } = await params;

  try {
    const body = UpdateCartLineRequest.parse(await request.json());

    const accessToken = await getAccessToken();
    const cartId = (await cookies()).get(CART_COOKIE)?.value;

    const cart = await call({
      service: 'cart',
      path: accessToken
        ? `/v1/carts/mine/lines/${encodeURIComponent(sku)}`
        : `/v1/carts/${encodeURIComponent(cartId ?? '')}/lines/${encodeURIComponent(sku)}`,
      schema: Cart,
      method: 'PATCH',
      body,
      accessToken: accessToken ?? undefined,
    });

    return NextResponse.json(cart, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, `/api/bff/cart/lines/${sku}`);
  }
}
