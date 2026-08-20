import { NextResponse } from 'next/server';

import { z } from 'zod';

import { call } from '@/lib/bff';
import { NO_STORE, problemResponse } from '../../_problem';

const ForgotPasswordRequest = z.object({ email: z.string().email().max(254) }).strict();
const AcceptedResponse = z.object({ status: z.string(), message: z.string() }).passthrough();

/**
 * Request a reset link.
 *
 * Passes the 202 through unchanged. identity-service answers identically
 * whether or not the address has an account, and this is one of the few
 * endpoints an unauthenticated caller can use to probe for one — so nothing
 * here may vary by outcome.
 */
export async function POST(request: Request) {
  try {
    const body = ForgotPasswordRequest.parse(await request.json());

    const result = await call({
      service: 'identity',
      path: '/v1/auth/forgot-password',
      schema: AcceptedResponse,
      method: 'POST',
      body,
    });

    return NextResponse.json(result, { status: 202, headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/auth/forgot-password');
  }
}
