import { NextResponse } from 'next/server';

import { OrderPage } from '@souq/contracts';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

/**
 * The signed-in user's orders.
 *
 * No user id in the path or the query. order-service derives it from the token,
 * which removes the entire class of bug where an id is accepted from the client
 * and the ownership check is forgotten on one endpoint out of twelve.
 */
export async function GET(request: Request) {
  try {
    const accessToken = await requireAccessToken();
    const cursor = new URL(request.url).searchParams.get('cursor');

    const page = await call({
      service: 'order',
      path: `/v1/orders/mine?limit=20${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`,
      schema: OrderPage,
      accessToken,
    });

    return NextResponse.json(page, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/orders/history');
  }
}
