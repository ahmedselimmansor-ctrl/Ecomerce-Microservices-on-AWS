'use client';

import { z } from 'zod';
import { ProblemDetails } from '@souq/contracts';

/**
 * The browser's client for this app's own Route Handlers.
 *
 * Distinct from `bff.ts`, which runs on the server and talks to the eleven
 * services. This one only ever calls `/api/bff/*` on the same origin — the
 * browser has no service URLs, no access token and no CORS surface
 * (docs/CONTRACTS.md §8).
 *
 * Every response is parsed with the same Zod schemas the BFF used. That looks
 * redundant and is not: the BFF validated what the *service* sent, and this
 * validates what *the route handler* sent, which is a different thing the
 * moment a handler reshapes a payload.
 */

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly requestId: string,
    message: string,
    readonly fieldErrors?: { field: string; message: string }[],
    readonly retryAfterSeconds?: number,
  ) {
    super(message);
    this.name = 'ApiError';
  }

  /** Whether showing a "try again" button makes sense. */
  get retriable(): boolean {
    return this.status === 0 || this.status >= 502;
  }

  /**
   * Copy for the user, keyed on the stable machine code.
   *
   * Never on `detail`: that string is written for an engineer reading a log,
   * it is not localised, and it changes with the next copy edit. Switching on
   * it is how a UI ends up showing "SKU SKU-4471 has 2 available" to a shopper.
   */
  get userMessage(): string {
    switch (this.code) {
      case 'UNAUTHENTICATED':
        return 'Please sign in to continue.';
      case 'TOKEN_EXPIRED':
        return 'Your session has expired. Please sign in again.';
      case 'REFRESH_TOKEN_REUSED':
        return 'We signed you out for security. Please sign in again.';
      case 'MFA_REQUIRED':
        return 'Enter the six-digit code from your authenticator app.';
      case 'FORBIDDEN':
        return 'You do not have access to that.';
      case 'ACCOUNT_LOCKED':
        return this.retryAfterSeconds
          ? `Too many attempts. Try again in ${Math.max(1, Math.ceil(this.retryAfterSeconds / 60))} minutes.`
          : 'Too many attempts. Please try again later.';
      case 'WEAK_PASSWORD':
        // The server's detail IS the guidance here, and it is written for a
        // person. This is the one case where passing it through is right.
        return this.message;
      case 'VALIDATION_FAILED':
        return this.message || 'Please check the highlighted fields.';
      case 'INVENTORY_INSUFFICIENT_STOCK':
        return 'Sorry — that is no longer available in the quantity you wanted.';
      case 'CART_STALE':
        return 'Your basket changed in another tab. We have refreshed it.';
      case 'ORDER_NOT_CANCELLABLE':
        return 'This order has gone too far to cancel. Contact support and we will help.';
      case 'PAYMENT_DECLINED':
        return 'Your payment was declined. Try a different card.';
      case 'PAYMENT_REQUIRES_ACTION':
        return 'Your bank needs to confirm this payment.';
      case 'RATE_LIMITED':
        return 'You are going a little fast. Give it a moment.';
      case 'PRODUCT_NOT_FOUND':
        return 'We could not find that product.';
      case 'UPSTREAM_TIMEOUT':
      case 'UPSTREAM_UNAVAILABLE':
        return 'We are having trouble reaching part of our system. Please try again.';
      default:
        return `Something went wrong. Quote reference ${this.requestId} if you contact us.`;
    }
  }
}

interface RequestOptions<T extends z.ZodTypeAny> {
  schema: T;
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
  /**
   * Required on anything that moves money or stock. The BFF forwards it as
   * `Idempotency-Key`, which is what makes a retry safe (CONTRACTS §2.1).
   */
  idempotencyKey?: string;
}

export async function apiFetch<T extends z.ZodTypeAny>(
  path: string,
  options: RequestOptions<T>,
): Promise<z.infer<T>> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;

  let response: Response;
  try {
    response = await fetch(path, {
      method: options.method ?? 'GET',
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: options.signal,
      // The refresh-token cookie is HttpOnly and SameSite=Strict; it has to be
      // sent for the session to survive.
      credentials: 'same-origin',
    });
  } catch (err) {
    // An aborted request is the caller cancelling, not a failure. Rethrowing it
    // as an ApiError makes every component show an error toast when a user
    // simply types another character into the search box.
    if (err instanceof DOMException && err.name === 'AbortError') throw err;

    throw new ApiError(0, 'UPSTREAM_UNAVAILABLE', '', 'Network request failed');
  }

  if (!response.ok) {
    throw await toApiError(response);
  }

  if (response.status === 204) {
    return options.schema.parse(undefined);
  }

  const payload = await response.json().catch(() => undefined);
  const parsed = options.schema.safeParse(payload);

  if (!parsed.success) {
    console.error('[api] response did not match the contract', {
      path,
      issues: parsed.error.issues.slice(0, 5),
    });
    throw new ApiError(502, 'UPSTREAM_UNAVAILABLE', '', 'Unexpected response from the server');
  }

  return parsed.data;
}

async function toApiError(response: Response): Promise<ApiError> {
  const requestId = response.headers.get('X-Request-Id') ?? '';

  const body: unknown = await response.json().catch(() => undefined);
  const problem = ProblemDetails.safeParse(body);

  if (!problem.success) {
    return new ApiError(response.status, 'INTERNAL_ERROR', requestId,
      `Request failed with status ${response.status}`);
  }

  // `retryAfterSeconds` is an RFC 9457 extension member, so it sits at the top
  // level of the body rather than inside a nested object.
  const extension = body as Record<string, unknown> | undefined;
  const retryAfter = typeof extension?.retryAfterSeconds === 'number'
    ? extension.retryAfterSeconds
    : undefined;

  return new ApiError(
    problem.data.status,
    problem.data.code,
    problem.data.requestId || requestId,
    problem.data.detail ?? problem.data.title,
    problem.data.errors,
    retryAfter,
  );
}

/**
 * A key that is stable for one logical attempt.
 *
 * Generated once when a form mounts and reused across retries of that same
 * submission, so a user hammering a flaky "Place order" button produces one
 * order rather than four. A fresh key per click would defeat the entire point
 * of the header.
 */
export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}
