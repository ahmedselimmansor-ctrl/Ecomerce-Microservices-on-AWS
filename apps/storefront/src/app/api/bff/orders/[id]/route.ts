import { NextResponse } from 'next/server';

import { OrderStatusResponse } from '@souq/contracts';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

/**
 * Order status, for the checkout poller.
 *
 * Authenticated, and the ownership check happens in order-service rather than
 * here. An order id is guessable enough that "knows the id" must not be
 * sufficient — the BFF has no way to verify ownership without the order, so it
 * forwards the token and lets the owner of the data decide.
 */
export async function GET(
  _request: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  try {
    const accessToken = await requireAccessToken();

    const status = await call({
      service: 'order',
      path: `/v1/orders/${encodeURIComponent(id)}/status`,
      schema: OrderStatusResponse,
      accessToken,
      // No retries. The client is already polling; a retry here just makes each
      // poll take three times as long to report the same thing.
      attempts: 1,
    });

    return NextResponse.json(status, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, `/api/bff/orders/${id}`);
  }
}
