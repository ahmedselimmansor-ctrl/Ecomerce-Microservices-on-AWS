import { NextResponse } from 'next/server';

import { CreateOrderRequest, CreateOrderResponse } from '@souq/contracts';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../_problem';

/**
 * Place an order.
 *
 * Returns **202**, and passing that through unchanged is the single most
 * important thing this handler does. Checkout is a saga across three
 * independent databases with no two-phase commit; at the moment this responds,
 * stock has not been reserved and nothing has been charged. Converting it to a
 * 201 here would make the client show a success page for an order that fails
 * roughly one time in ten.
 *
 * `Idempotency-Key` is required, not optional. Without one a retry after a lost
 * response places a second order — the most expensive duplicate in the system.
 */
export async function POST(request: Request) {
  try {
    const idempotencyKey = request.headers.get('Idempotency-Key');

    if (!idempotencyKey) {
      return NextResponse.json(
        {
          type: 'https://errors.souq.dev/bff/validation-failed',
          title: 'Missing idempotency key',
          status: 400,
          detail: 'Placing an order requires an Idempotency-Key header.',
          code: 'VALIDATION_FAILED',
          instance: '/api/bff/orders',
          requestId: '',
          timestamp: new Date().toISOString(),
        },
        { status: 400, headers: { ...NO_STORE, 'Content-Type': 'application/problem+json' } },
      );
    }

    const accessToken = await requireAccessToken();
    const body = CreateOrderRequest.parse(await request.json());

    const result = await call({
      service: 'order',
      path: '/v1/orders',
      schema: CreateOrderResponse,
      method: 'POST',
      body,
      accessToken,
      idempotencyKey,
    });

    return NextResponse.json(result, { status: 202, headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/orders');
  }
}
