import { NextResponse } from 'next/server';
import { cookies } from 'next/headers';

import { z } from 'zod';

import { call } from '@/lib/bff';
import { CART_COOKIE, refreshCookieOptions } from '@/lib/session';
import { NO_STORE } from '../../_problem';

/**
 * Sign out.
 *
 * Always 204, whatever happens. The cookie is cleared **before** the upstream
 * call and in a `finally`-equivalent position, because the order matters: if
 * identity-service is down and we skipped clearing, the user stays signed in on
 * a machine they are trying to leave.
 *
 * Server-side revocation is still attempted, so the token is dead everywhere
 * rather than merely forgotten by this browser. But a failure there must not
 * surface — there is nothing useful the user can do with it, and a logout that
 * can fail is one people abandon halfway.
 */
export async function POST() {
  const jar = await cookies();
  const refreshToken = jar.get('souq_rt')?.value;

  // Local first. This is the part that must not be able to fail.
  jar.set({ ...refreshCookieOptions(0), value: '' });
  // The guest basket goes too. Leaving it means the next person on this device
  // inherits the previous user's basket.
  jar.set({ name: CART_COOKIE, value: '', path: '/', maxAge: 0 });

  if (refreshToken) {
    try {
      await call({
        service: 'identity',
        path: '/v1/auth/logout',
        schema: z.undefined(),
        method: 'POST',
        body: { refreshToken },
      });
    } catch {
      // Deliberately swallowed. See above.
    }
  }

  return new NextResponse(null, { status: 204, headers: NO_STORE });
}
