import { NextResponse } from 'next/server';
import { randomUUID } from 'node:crypto';

import { BffError } from '@/lib/bff';

/**
 * Renders any thrown value as the RFC 9457 envelope from CONTRACTS §2.2.
 *
 * Every Route Handler funnels its catch block through here, so the browser sees
 * exactly the shape it sees from the eleven services — and `api-client.ts` has
 * one parser rather than one per endpoint.
 *
 * A non-`BffError` is always a 500 with fixed text. Reaching that branch means
 * a bug in our own handler, and the message would be an internal one.
 */
export function problemResponse(error: unknown, instance: string): NextResponse {
  const requestId = error instanceof BffError && error.requestId ? error.requestId : randomUUID();

  if (error instanceof BffError) {
    // Status 0 is "could not reach the service at all". HTTP has no such code,
    // and 502 is what it means to the caller.
    const status = error.status === 0 ? 502 : error.status;

    return NextResponse.json(
      {
        // Pass the upstream problem through when there is one, so a field-level
        // validation error from a service survives the hop intact.
        ...(error.problem ?? {
          type: `https://errors.souq.dev/bff/${String(error.code).toLowerCase().replace(/_/g, '-')}`,
          title: 'Request failed',
          detail: error.message,
        }),
        status,
        code: error.code,
        instance,
        requestId,
        timestamp: new Date().toISOString(),
      },
      {
        status,
        headers: {
          'Content-Type': 'application/problem+json',
          'X-Request-Id': requestId,
          'Cache-Control': 'no-store',
        },
      },
    );
  }

  console.error('[bff] unhandled error in a route handler', { instance, requestId, error });

  return NextResponse.json(
    {
      type: 'https://errors.souq.dev/bff/internal-error',
      title: 'Internal error',
      status: 500,
      detail: `Something went wrong. Quote request id ${requestId} if you contact support.`,
      instance,
      code: 'INTERNAL_ERROR',
      requestId,
      timestamp: new Date().toISOString(),
    },
    {
      status: 500,
      headers: {
        'Content-Type': 'application/problem+json',
        'X-Request-Id': requestId,
        'Cache-Control': 'no-store',
      },
    },
  );
}

/** Every BFF response is per-user. None of it may sit in a shared cache. */
export const NO_STORE = { 'Cache-Control': 'private, no-store' } as const;
