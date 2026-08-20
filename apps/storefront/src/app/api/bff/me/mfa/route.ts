import { NextResponse } from 'next/server';

import { z } from 'zod';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

const EnrolStart = z.object({
  secret: z.string(),
  uri: z.string(),
}).strict();

/**
 * Begin TOTP enrolment.
 *
 * Returns the shared secret, which is as sensitive as a password for as long as
 * enrolment is pending. `no-store` is not decoration here: a cached response
 * containing this value, in a browser cache or an intermediary, is a second
 * factor somebody else can install.
 *
 * The secret is not active yet. identity-service stores it as pending and only
 * enables MFA once a generated code is verified — see the confirm route.
 */
export async function POST() {
  try {
    const accessToken = await requireAccessToken();

    const enrolment = await call({
      service: 'identity',
      path: '/v1/me/mfa',
      schema: EnrolStart,
      method: 'POST',
      accessToken,
    });

    return NextResponse.json(enrolment, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/me/mfa');
  }
}

/** Turn MFA off. Requires a currently valid code — see the confirm route's comment. */
export async function DELETE(request: Request) {
  try {
    const accessToken = await requireAccessToken();
    const body = z.object({ code: z.string().regex(/^\d{6}$/) }).parse(await request.json());

    await call({
      service: 'identity',
      path: '/v1/me/mfa',
      schema: z.undefined(),
      method: 'DELETE',
      body,
      accessToken,
    });

    return new NextResponse(null, { status: 204, headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/me/mfa');
  }
}
