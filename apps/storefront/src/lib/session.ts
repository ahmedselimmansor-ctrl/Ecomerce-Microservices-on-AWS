import { cookies } from 'next/headers';

import { TokenPair } from '@souq/contracts';

import { BffError, call } from './bff';

/**
 * Server-side session handling for the Route Handlers.
 *
 * The whole point of the BFF (docs/CONTRACTS.md §8) is that neither token ever
 * reaches JavaScript:
 *
 *   - the **refresh** token is an HttpOnly, SameSite=Strict, Secure cookie the
 *     browser cannot read, and
 *   - the **access** token is derived from it here, per request, and never
 *     leaves the server.
 *
 * That means an XSS can make requests as the user — which any session design
 * allows — but cannot steal a 30-day credential and replay it from elsewhere
 * for a month. Those are very different incidents.
 */

const REFRESH_COOKIE = 'souq_rt';
/** Matches identity-service's cookie scope exactly, or clearing it silently fails. */
const REFRESH_PATH = '/api/bff/auth';

/**
 * Per-request cache of the exchanged access token.
 *
 * One page render fans out to several services. Without this, each fan-out
 * would rotate the refresh token again — and since rotation retires the
 * previous one, the second exchange would present a token the first had just
 * invalidated and trip reuse detection, logging the user out for browsing.
 */
const perRequest = new WeakMap<object, Promise<string | null>>();

export interface Session {
  accessToken: string;
  userId: string;
  roles: string[];
}

/**
 * The access token for this request, or null when signed out.
 *
 * Returns null rather than throwing. Most routes here serve both signed-in and
 * anonymous callers — a product page, a guest basket — and forcing every one of
 * them into a try/catch to handle the ordinary case is how a route ends up
 * 500ing for logged-out visitors.
 */
export async function getAccessToken(): Promise<string | null> {
  const jar = await cookies();
  const refreshToken = jar.get(REFRESH_COOKIE)?.value;

  if (!refreshToken) return null;

  const key = jar as unknown as object;
  const cached = perRequest.get(key);
  if (cached) return cached;

  const exchange = (async (): Promise<string | null> => {
    try {
      const pair = await call({
        service: 'identity',
        path: '/v1/auth/refresh',
        schema: TokenPair,
        method: 'POST',
        body: { refreshToken },
      });
      return pair.accessToken;
    } catch (error) {
      if (error instanceof BffError && error.status === 401) {
        // Expired, revoked, or reuse-detected. All three mean "sign in again",
        // and none is an error worth logging.
        return null;
      }
      // identity-service being unreachable is NOT a signed-out state. Returning
      // null would log every user out during a redeploy of that one service.
      throw error;
    }
  })();

  perRequest.set(key, exchange);
  return exchange;
}

/** Like {@link getAccessToken}, but for routes that genuinely require a user. */
export async function requireAccessToken(): Promise<string> {
  const token = await getAccessToken();
  if (!token) {
    throw new BffError(401, 'UNAUTHENTICATED', '', 'this endpoint requires a signed-in user');
  }
  return token;
}

/**
 * The cookie attributes, in one place.
 *
 * They have to match byte for byte between setting and clearing — a mismatch on
 * name, path or SameSite leaves the browser holding the original cookie
 * alongside the deletion, so "sign out" does not.
 */
export function refreshCookieOptions(maxAgeSeconds: number) {
  return {
    name: REFRESH_COOKIE,
    httpOnly: true,
    // Off only for plain-HTTP local development. Anywhere else, a refresh token
    // over cleartext is the entire session in the first packet.
    secure: process.env.NODE_ENV === 'production',
    sameSite: 'strict' as const,
    path: REFRESH_PATH,
    maxAge: maxAgeSeconds,
  };
}

/**
 * The anonymous basket id.
 *
 * A guest basket needs an identity before there is a user. Not HttpOnly-strict
 * about it — this is an opaque id with no authority, and cart-service treats it
 * as a lookup key rather than as a credential.
 */
export const CART_COOKIE = 'souq_cart';

export async function getCartId(): Promise<string | null> {
  return (await cookies()).get(CART_COOKIE)?.value ?? null;
}
