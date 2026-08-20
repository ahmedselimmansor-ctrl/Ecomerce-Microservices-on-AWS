import { NextResponse } from 'next/server';

import { RegisterRequest } from '@souq/contracts';
import { z } from 'zod';

import { call } from '@/lib/bff';
import { NO_STORE, problemResponse } from '../../_problem';

const AcceptedResponse = z.object({ status: z.string(), message: z.string() }).passthrough();

/**
 * Create an account.
 *
 * Passes identity-service's 202 through unchanged, including the deliberately
 * uninformative body. The service returns the same response whether the account
 * was created or already existed; adding anything here that distinguishes the
 * two — a different status, a different message, even a measurably different
 * latency — hands back the account-enumeration oracle it exists to close.
 */
export async function POST(request: Request) {
  try {
    const body = RegisterRequest.parse(await request.json());

    const result = await call({
      service: 'identity',
      path: '/v1/auth/register',
      schema: AcceptedResponse,
      method: 'POST',
      body,
    });

    return NextResponse.json(result, { status: 202, headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/auth/register');
  }
}
