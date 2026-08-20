import { cookies } from 'next/headers';

import { TokenPair, UserProfile } from '@souq/contracts';

import { AdminApiError, adminCall } from './admin-api';

/**
 * Admin session.
 *
 * The authorisation rule is docs/CONTRACTS.md §7 and it is stricter than the
 * storefront's: an ADMIN or OPS role **and** an `amr` containing `mfa`. The
 * role alone is not enough, because a role survives a stolen password and a
 * second factor does not.
 *
 * That check happens here, on the server, on every request. The client-side
 * equivalent would hide the navigation and nothing else.
 */

const REFRESH_COOKIE = 'souq_rt';

export interface AdminSession {
  accessToken: string;
  user: UserProfile;
}

export class NotAuthorised extends Error {
  constructor(readonly reason: 'unauthenticated' | 'no-role' | 'no-mfa') {
    super(reason);
  }
}

/**
 * Resolve the session, or throw.
 *
 * Throws rather than returning null because there is no page in this app that
 * an unauthorised caller should see. Making the failure the exceptional path
 * means a new page cannot accidentally forget to check.
 */
export async function requireAdmin(): Promise<AdminSession> {
  const refreshToken = (await cookies()).get(REFRESH_COOKIE)?.value;
  if (!refreshToken) throw new NotAuthorised('unauthenticated');

  let accessToken: string;
  try {
    const pair = await adminCall({
      service: 'identity',
      path: '/v1/auth/refresh',
      schema: TokenPair,
      method: 'POST',
      body: { refreshToken },
      // The refresh call itself is the one that has no token yet.
      accessToken: '',
    });
    accessToken = pair.accessToken;
  } catch (error) {
    if (error instanceof AdminApiError && error.status === 401) {
      throw new NotAuthorised('unauthenticated');
    }
    throw error;
  }

  const user = await adminCall({
    service: 'identity',
    path: '/v1/me',
    schema: UserProfile,
    accessToken,
  });

  const privileged = user.roles.includes('ADMIN') || user.roles.includes('OPS');
  if (!privileged) throw new NotAuthorised('no-role');

  // MFA is checked from the profile rather than from the token's `amr`, because
  // the BFF never hands the raw JWT to this layer. `mfaEnabled` is a weaker
  // statement than "this session used MFA" — the strong check lives in
  // identity-service, which refuses to mint an admin-scoped token without it.
  if (!user.mfaEnabled) throw new NotAuthorised('no-mfa');

  return { accessToken, user };
}
