import { NextResponse } from 'next/server';
import { randomUUID } from 'node:crypto';

import { AdminApiError } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin, type AdminSession } from '@/lib/session';

/**
 * Every mutating admin route starts here.
 *
 * The check is server-side and per request. Hiding a button in the UI is a
 * courtesy; this is the control. A route that skipped it would be reachable by
 * anyone who can guess the URL, and these routes cancel orders and discard
 * events.
 */
export async function withAdmin<T>(
  instance: string,
  handler: (session: AdminSession) => Promise<T>,
): Promise<NextResponse> {
  try {
    const session = await requireAdmin();
    const result = await handler(session);

    return NextResponse.json(result ?? { ok: true }, {
      headers: { 'Cache-Control': 'no-store' },
    });
  } catch (error) {
    return adminProblem(error, instance);
  }
}

export function adminProblem(error: unknown, instance: string): NextResponse {
  const requestId =
    error instanceof AdminApiError && error.requestId ? error.requestId : randomUUID();

  if (error instanceof NotAuthorised) {
    const status = error.reason === 'unauthenticated' ? 401 : 403;
    return NextResponse.json(
      {
        type: `https://errors.souq.dev/admin/${error.reason}`,
        title: 'Not permitted',
        status,
        detail: error.reason === 'no-mfa'
          ? 'The admin API requires a session with multi-factor authentication.'
          : 'The admin API requires the ADMIN or OPS role.',
        code: status === 401 ? 'UNAUTHENTICATED' : 'FORBIDDEN',
        instance,
        requestId,
        timestamp: new Date().toISOString(),
      },
      { status, headers: { 'Content-Type': 'application/problem+json', 'Cache-Control': 'no-store' } },
    );
  }

  if (error instanceof AdminApiError) {
    return NextResponse.json(
      {
        type: `https://errors.souq.dev/admin/${error.code.toLowerCase().replace(/_/g, '-')}`,
        title: 'Upstream rejected the request',
        status: error.status,
        detail: error.message,
        code: error.code,
        instance,
        requestId,
        timestamp: new Date().toISOString(),
      },
      {
        status: error.status,
        headers: { 'Content-Type': 'application/problem+json', 'Cache-Control': 'no-store' },
      },
    );
  }

  console.error('[admin-api] unhandled error', { instance, requestId, error });

  return NextResponse.json(
    {
      type: 'https://errors.souq.dev/admin/internal-error',
      title: 'Internal error',
      status: 500,
      detail: `Request ${requestId} failed.`,
      code: 'INTERNAL_ERROR',
      instance,
      requestId,
      timestamp: new Date().toISOString(),
    },
    { status: 500, headers: { 'Content-Type': 'application/problem+json', 'Cache-Control': 'no-store' } },
  );
}
