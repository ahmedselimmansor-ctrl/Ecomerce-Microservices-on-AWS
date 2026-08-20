import { z } from 'zod';
import { randomUUID } from 'node:crypto';
import { headers } from 'next/headers';
import { ProblemDetails, type ErrorCode } from '@souq/contracts';

/**
 * The BFF's service client.
 *
 * The browser never calls a service directly (docs/CONTRACTS.md §8). Every
 * request goes through a Route Handler in this app, which fans out to the
 * services and validates what comes back before a component ever sees it.
 *
 * That indirection buys three things that are hard to get any other way:
 *
 *   1. Tokens stay out of JavaScript. The refresh token is an HttpOnly cookie
 *      the browser cannot read, and the access token never leaves the server.
 *   2. One Zod schema guards every response. When a service starts
 *      returning `available: null`, the storefront produces one clean 502 with
 *      a requestId — not `undefined is not a number` inside a memoised
 *      selector three renders later.
 *   3. Eleven services in five languages become one origin to the browser, so
 *      there is no CORS surface and no per-service client config.
 */

// Server-side only. These are container DNS names, not public URLs — if one of
// them ever appears in a NEXT_PUBLIC_ variable, the browser is being told how
// to reach a service directly and the boundary above has been broken.
const SERVICES = {
  identity: process.env.SOUQ_IDENTITY_URL,
  catalog: process.env.SOUQ_CATALOG_URL,
  cart: process.env.SOUQ_CART_URL,
  order: process.env.SOUQ_ORDER_URL,
  search: process.env.SOUQ_SEARCH_URL,
  recommendation: process.env.SOUQ_RECOMMENDATION_URL,
  review: process.env.SOUQ_REVIEW_URL,
} as const;

export type ServiceName = keyof typeof SERVICES;

/**
 * Per-service timeouts, from docs/CONTRACTS.md §5.4.
 *
 * Search is tighter than the rest because it is on the browsing path and a
 * slow search should degrade to "no results yet" rather than a spinner.
 * Order is the loosest because placing an order writes to Postgres and Kafka
 * in one transaction, and giving up on it early means the client retries a
 * request that may already have succeeded.
 */
const TIMEOUTS: Record<ServiceName, number> = {
  identity: 3_000,
  catalog: 3_000,
  cart: 3_000,
  order: 5_000,
  search: 2_000,
  recommendation: 1_000, // recommendations are optional; never make a page wait
  review: 3_000,
};

export class BffError extends Error {
  constructor(
    readonly status: number,
    readonly code: ErrorCode | string,
    readonly requestId: string,
    message: string,
    readonly problem?: z.infer<typeof ProblemDetails>,
  ) {
    super(message);
    this.name = 'BffError';
  }

  /**
   * Whether the caller may safely retry. Mirrors §5.4: connection failures and
   * 5xx yes, anything the client got wrong no. Retrying a 409 just burns the
   * budget and delays showing the user the real problem.
   */
  get retriable(): boolean {
    return this.status === 0 || this.status === 502 || this.status === 503 || this.status === 504;
  }
}

interface CallOptions<T extends z.ZodTypeAny> {
  service: ServiceName;
  path: string;
  schema: T;
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  /** Required on any mutation of money or stock (docs/CONTRACTS.md §2.1). */
  idempotencyKey?: string;
  /** Forwarded from the incoming request; absent for anonymous browsing. */
  accessToken?: string;
  /** Next.js fetch caching. Defaults to no-store — commerce data is not static. */
  revalidate?: number | false;
  tags?: string[];
  /** Attempts including the first. Only ever applied to idempotent requests. */
  attempts?: number;
}

/**
 * Call a service and validate the response.
 *
 * Returns parsed, typed data or throws BffError. There is no third outcome —
 * in particular there is no "returns unvalidated JSON" path, because the one
 * place that skips validation is the one that eventually ships a crash.
 */
export async function call<T extends z.ZodTypeAny>(opts: CallOptions<T>): Promise<z.infer<T>> {
  const base = SERVICES[opts.service];
  if (!base) {
    throw new BffError(500, 'INTERNAL_ERROR', '',
      `SOUQ_${opts.service.toUpperCase()}_URL is not configured`);
  }

  const requestId = (await incomingRequestId()) ?? randomUUID();
  const method = opts.method ?? 'GET';
  const url = `${base}${opts.path}`;

  // Retries only on requests that are safe to repeat. A POST without an
  // idempotency key must never be retried automatically: the first attempt may
  // have succeeded and only the response was lost.
  const safeToRetry = method === 'GET' || opts.idempotencyKey !== undefined;
  const maxAttempts = safeToRetry ? (opts.attempts ?? 3) : 1;

  let lastError: BffError | undefined;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      return await once(opts, url, method, requestId);
    } catch (err) {
      if (!(err instanceof BffError) || !err.retriable || attempt === maxAttempts) {
        throw err;
      }
      lastError = err;

      // Exponential backoff with FULL jitter. Fixed backoff means every client
      // that failed on the same blip retries in lockstep and re-creates the
      // herd that caused it.
      const ceiling = Math.min(100 * 2 ** (attempt - 1), 2_000);
      await sleep(Math.random() * ceiling);
    }
  }

  throw lastError ?? new BffError(500, 'INTERNAL_ERROR', requestId, 'retry loop exited without a result');
}

async function once<T extends z.ZodTypeAny>(
  opts: CallOptions<T>,
  url: string,
  method: string,
  requestId: string,
): Promise<z.infer<T>> {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), TIMEOUTS[opts.service]);

  const requestHeaders: Record<string, string> = {
    'X-Request-Id': requestId,
    'X-Correlation-Id': requestId,
    Accept: 'application/json',
  };
  if (opts.body !== undefined) requestHeaders['Content-Type'] = 'application/json';
  if (opts.accessToken) requestHeaders.Authorization = `Bearer ${opts.accessToken}`;
  if (opts.idempotencyKey) requestHeaders['Idempotency-Key'] = opts.idempotencyKey;

  let response: Response;
  try {
    response = await fetch(url, {
      method,
      headers: requestHeaders,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
      signal: controller.signal,
      // Commerce data is never statically cacheable by default. Pages opt in
      // explicitly where it is safe (a category tree, a product page).
      cache: opts.revalidate === undefined ? 'no-store' : undefined,
      next: opts.revalidate === undefined ? undefined : { revalidate: opts.revalidate, tags: opts.tags },
    });
  } catch (err) {
    clearTimeout(timeout);

    if (err instanceof Error && err.name === 'AbortError') {
      throw new BffError(504, 'UPSTREAM_TIMEOUT', requestId,
        `${opts.service} did not respond within ${TIMEOUTS[opts.service]}ms`);
    }
    throw new BffError(0, 'UPSTREAM_UNAVAILABLE', requestId,
      `could not reach ${opts.service}: ${err instanceof Error ? err.message : String(err)}`);
  } finally {
    clearTimeout(timeout);
  }

  // The service's own request id wins — it is the one in its logs.
  const upstreamRequestId = response.headers.get('X-Request-Id') ?? requestId;

  if (!response.ok) {
    throw await toProblem(response, opts.service, upstreamRequestId);
  }

  if (response.status === 204) {
    return opts.schema.parse(undefined);
  }

  let payload: unknown;
  try {
    payload = await response.json();
  } catch {
    throw new BffError(502, 'UPSTREAM_UNAVAILABLE', upstreamRequestId,
      `${opts.service} returned a non-JSON body with status ${response.status}`);
  }

  const parsed = opts.schema.safeParse(payload);
  if (!parsed.success) {
    // A contract violation, not a user error. Log the detail server-side and
    // give the client a requestId — the shape of our internal payloads is not
    // something to hand to a browser.
    console.error('[bff] contract violation', {
      service: opts.service,
      path: opts.path,
      requestId: upstreamRequestId,
      issues: parsed.error.issues.slice(0, 10),
    });
    throw new BffError(502, 'UPSTREAM_UNAVAILABLE', upstreamRequestId,
      `${opts.service} returned a response that does not match the contract`);
  }

  return parsed.data;
}

/**
 * Turn a service's error response into a BffError.
 *
 * Every service in every language emits the same RFC 9457 envelope
 * (docs/CONTRACTS.md §2.2), so this is the single place the storefront learns
 * what went wrong — and the reason the UI switches on `code` rather than
 * pattern-matching prose that changes with the next copy edit.
 */
async function toProblem(response: Response, service: ServiceName, requestId: string): Promise<BffError> {
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    return new BffError(response.status, 'INTERNAL_ERROR', requestId,
      `${service} returned ${response.status} with an unreadable body`);
  }

  const problem = ProblemDetails.safeParse(body);
  if (problem.success) {
    return new BffError(problem.data.status, problem.data.code, problem.data.requestId || requestId,
      problem.data.detail ?? problem.data.title, problem.data);
  }

  // A service that does not follow the envelope is itself a contract
  // violation, but failing the user's request over it would be worse than
  // passing the status through.
  console.warn('[bff] service returned a non-conforming error envelope', {
    service, status: response.status, requestId,
  });
  return new BffError(response.status, 'INTERNAL_ERROR', requestId,
    `${service} returned ${response.status}`);
}

/**
 * Propagates the request id from the incoming request so one id spans the whole
 * fan-out.
 *
 * `headers()` is async as of Next 15. Calling it synchronously returns a
 * Promise, `.get` on which is undefined — so the old form did not throw, it
 * quietly produced a fresh id per hop and every trace was broken into eleven
 * unrelated pieces.
 */
async function incomingRequestId(): Promise<string | undefined> {
  try {
    return (await headers()).get('x-request-id') ?? undefined;
  } catch {
    // Called outside a request scope (a build-time render).
    return undefined;
  }
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * Run several service calls concurrently, tolerating failure in the optional
 * ones.
 *
 * A product page needs the product; it does not need recommendations, and it
 * certainly should not 500 because the recommendation service is redeploying.
 * `Promise.all` gets this exactly wrong — one rejection loses every result.
 */
export async function gather<T extends Record<string, Promise<unknown>>>(
  required: (keyof T)[],
  promises: T,
): Promise<{ [K in keyof T]: Awaited<T[K]> | null }> {
  const keys = Object.keys(promises) as (keyof T)[];
  const settled = await Promise.allSettled(keys.map((k) => promises[k]));

  const out = {} as { [K in keyof T]: Awaited<T[K]> | null };

  keys.forEach((key, i) => {
    const result = settled[i]!;
    if (result.status === 'fulfilled') {
      out[key] = result.value as Awaited<T[typeof key]>;
      return;
    }

    if (required.includes(key)) {
      throw result.reason;
    }

    console.warn('[bff] optional call failed, degrading', {
      key: String(key),
      error: result.reason instanceof Error ? result.reason.message : String(result.reason),
    });
    out[key] = null;
  });

  return out;
}
