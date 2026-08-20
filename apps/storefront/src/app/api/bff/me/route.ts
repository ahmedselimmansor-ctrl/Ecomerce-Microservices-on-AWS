import { NextResponse } from 'next/server';

import { UserProfile } from '@souq/contracts';

import { call } from '@/lib/bff';
import { getAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../_problem';

/**
 * The signed-in user's profile.
 *
 * A 401 here is the ordinary state of an anonymous visitor, not a fault, so it
 * is returned directly rather than raised — the client treats it as "signed
 * out" without logging anything.
 */
export async function GET() {
  try {
    const accessToken = await getAccessToken();

    if (!accessToken) {
      return NextResponse.json(
        {
          type: 'https://errors.souq.dev/bff/unauthenticated',
          title: 'Not signed in',
          status: 401,
          code: 'UNAUTHENTICATED',
          instance: '/api/bff/me',
          requestId: '',
          timestamp: new Date().toISOString(),
        },
        { status: 401, headers: { ...NO_STORE, 'Content-Type': 'application/problem+json' } },
      );
    }

    const profile = await call({
      service: 'identity',
      path: '/v1/me',
      schema: UserProfile,
      accessToken,
    });

    return NextResponse.json(profile, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/me');
  }
}
