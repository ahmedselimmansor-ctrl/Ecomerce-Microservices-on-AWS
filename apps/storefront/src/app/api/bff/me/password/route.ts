import { NextResponse } from 'next/server';

import { z } from 'zod';

import { call } from '@/lib/bff';
import { requireAccessToken } from '@/lib/session';
import { NO_STORE, problemResponse } from '../../_problem';

const ChangePasswordRequest = z.object({
  currentPassword: z.string().min(1).max(1024),
  newPassword: z.string().min(1).max(1024),
}).strict();

/**
 * Change the signed-in user's password.
 *
 * The refresh cookie is deliberately NOT cleared. identity-service revokes
 * every session except the calling one, which is the point of changing a
 * password after a suspected compromise: the attacker is signed out and the
 * user is not. Clearing the cookie here would undo that and sign out the person
 * who just secured their account.
 */
export async function POST(request: Request) {
  try {
    const accessToken = await requireAccessToken();
    const body = ChangePasswordRequest.parse(await request.json());

    await call({
      service: 'identity',
      path: '/v1/me/password',
      schema: z.undefined(),
      method: 'POST',
      body,
      accessToken,
    });

    return new NextResponse(null, { status: 204, headers: NO_STORE });
  } catch (error) {
    return problemResponse(error, '/api/bff/me/password');
  }
}
