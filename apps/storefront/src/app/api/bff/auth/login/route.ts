import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { LoginRequest, LoginResponse } from '@souq/contracts';

import { call } from '@/lib/bff';
import { refreshCookieOptions } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

/** 30 days, matching the refresh TTL in docs/CONTRACTS.md §7. */
const REFRESH_MAX_AGE = 60 * 60 * 24 * 30;

/**
 * Sign in.
 *
 * The one thing this handler exists to do is **strip the refresh token out of
 * the response body and put it in an HttpOnly cookie**. identity-service
 * returns it in the body because native app clients have no cookie jar; a
 * browser must never see it, because a 30-day credential readable by JavaScript
 * is one XSS away from a month of account access.
 */
export async function POST(request: Request) {
  try {
    const body = LoginRequest.parse(await request.json());

    const result = await call({
      service: 'identity',
      path: '/v1/auth/login',
      schema: LoginResponse,
      method: 'POST',
      body,
    });

    if (result.tokens.refreshToken) {
      (await cookies()).set({
        ...refreshCookieOptions(REFRESH_MAX_AGE),
        value: result.tokens.refreshToken,
      });
    }

    // The access token is stripped too. It is short-lived, but there is no
    // reason for the browser to hold it — every call it makes goes through this
    // origin, and the server re-derives it per request.
    return NextResponse.json(
      {
        ...result,
        tokens: { ...result.tokens, refreshToken: undefined, accessToken: '' },
      },
      { headers: NO_STORE },
    );
  } catch (error) {
    return problemResponse(error, '/api/bff/auth/login');
  }
}
