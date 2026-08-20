import { NextResponse } from 'next/server';

import { z } from 'zod';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../../_problem';

const EnrolComplete = z.object({
  recoveryCodes: z.array(z.string()),
}).strict();

/**
 * Finish TOTP enrolment by proving the authenticator works.
 *
 * MFA is not enabled until this succeeds. Enabling it when the secret is issued
 * would lock out every user whose app failed to save it — a support call that
 * ends in a human identity check, which is both expensive and the weakest link
 * in the whole scheme.
 *
 * The recovery codes come back exactly once. identity-service stores only their
 * hashes, so this response is the only time they exist in readable form
 * anywhere — which is why the UI will not let the user move on until they say
 * they have saved them.
 */
export async function POST(request: Request) {
  try {
    const accessToken = await requireAccessToken();
    const body = z.object({ code: z.string().regex(/^\d{6}$/) }).parse(await request.json());

    const result = await call({
      service: 'identity',
      path: '/v1/me/mfa/confirm',
      schema: EnrolComplete,
      method: 'POST',
      body,
      accessToken,
    });

    return NextResponse.json(result, { headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/me/mfa/confirm');
  }
}
