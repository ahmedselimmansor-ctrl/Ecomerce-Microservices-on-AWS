import { z } from 'zod';
import { headers } from 'next/headers';
import { randomUUID } from 'node:crypto';

import { ProblemDetails } from '@souq/contracts';

/**
 * The admin app's service client.
 *
 * Deliberately separate from the storefront's `bff.ts` rather than shared. The
 * two have different rules and merging them would mean one set of defaults
 * quietly applying to both:
 *
 *   - **Nothing here is ever cached.** The storefront caches product pages at
 *     the edge; an operations tool showing a two-minute-old view of a stuck
 *     saga is actively dangerous during an incident.
 *   - **Nothing here is retried automatically**, not even a GET. An admin
 *     watching a screen refreshes it themselves, and a silent triple-retry
 *     turns "the service is down" into "the page is slow", which is the wrong
 *     thing to learn at 2am.
 *   - **Timeouts are longer.** An admin export or a DLQ scan legitimately takes
 *     ten seconds. Failing it at three would make the tool useless for the
 *     queries people actually run.
 */

const SERVICES = {
  identity: process.env.SOUQ_IDENTITY_URL,
  catalog: process.env.SOUQ_CATALOG_URL,
  order: process.env.SOUQ_ORDER_URL,
  inventory: process.env.SOUQ_INVENTORY_URL,
  payment: process.env.SOUQ_PAYMENT_URL,
  search: process.env.SOUQ_SEARCH_URL,
  review: process.env.SOUQ_REVIEW_URL,
} as const;

export type AdminService = keyof typeof SERVICES;

const TIMEOUT_MS = 10_000;

export class AdminApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly requestId: string,
    message: string,
  ) {
    super(message);
    this.name = 'AdminApiError';
  }
}

interface CallOptions<T extends z.ZodTypeAny> {
  service: AdminService;
  path: string;
  schema: T;
  method?: 'GET' | 'POST' | 'PATCH' | 'PUT' | 'DELETE';
  body?: unknown;
  accessToken: string;
  idempotencyKey?: string;
}

export async function adminCall<T extends z.ZodTypeAny>(
  options: CallOptions<T>,
): Promise<z.infer<T>> {
  const base = SERVICES[options.service];
  if (!base) {
    throw new AdminApiError(500, 'INTERNAL_ERROR', '',
      `SOUQ_${options.service.toUpperCase()}_URL is not configured`);
  }

  const requestId = (await incomingRequestId()) ?? randomUUID();
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), TIMEOUT_MS);

  const requestHeaders: Record<string, string> = {
    Accept: 'application/json',
    Authorization: `Bearer ${options.accessToken}`,
    'X-Request-Id': requestId,
    'X-Correlation-Id': requestId,
    // Marks the caller in every downstream log. An action taken from the admin
    // tool must be distinguishable from the same action taken by a customer,
    // or an audit trail cannot answer "who cancelled this order".
    'X-Actor-Kind': 'admin',
  };
  if (options.body !== undefined) requestHeaders['Content-Type'] = 'application/json';
  if (options.idempotencyKey) requestHeaders['Idempotency-Key'] = options.idempotencyKey;

  let response: Response;
  try {
    response = await fetch(`${base}${options.path}`, {
      method: options.method ?? 'GET',
      headers: requestHeaders,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: controller.signal,
      cache: 'no-store',
    });
  } catch (error) {
    clearTimeout(timeout);

    if (error instanceof Error && error.name === 'AbortError') {
      throw new AdminApiError(504, 'UPSTREAM_TIMEOUT', requestId,
        `${options.service} did not respond within ${TIMEOUT_MS}ms`);
    }
    throw new AdminApiError(502, 'UPSTREAM_UNAVAILABLE', requestId,
      `could not reach ${options.service}`);
  } finally {
    clearTimeout(timeout);
  }

  const upstreamRequestId = response.headers.get('X-Request-Id') ?? requestId;

  if (!response.ok) {
    const body: unknown = await response.json().catch(() => undefined);
    const problem = ProblemDetails.safeParse(body);

    throw new AdminApiError(
      response.status,
      problem.success ? problem.data.code : 'INTERNAL_ERROR',
      upstreamRequestId,
      problem.success ? (problem.data.detail ?? problem.data.title) : `status ${response.status}`,
    );
  }

  if (response.status === 204) return options.schema.parse(undefined);

  const payload: unknown = await response.json();
  const parsed = options.schema.safeParse(payload);

  if (!parsed.success) {
    console.error('[admin] contract violation', {
      service: options.service,
      path: options.path,
      requestId: upstreamRequestId,
      issues: parsed.error.issues.slice(0, 10),
    });
    throw new AdminApiError(502, 'UPSTREAM_UNAVAILABLE', upstreamRequestId,
      `${options.service} returned a response that does not match the contract`);
  }

  return parsed.data;
}

async function incomingRequestId(): Promise<string | undefined> {
  try {
    return (await headers()).get('x-request-id') ?? undefined;
  } catch {
    return undefined;
  }
}

/**
 * Run several admin calls, tolerating failures in the optional ones.
 *
 * The dashboard shows six KPI tiles from six services. `Promise.all` would blank
 * the entire dashboard because payment-service is redeploying — precisely when
 * someone needs to look at it.
 */
export async function gatherAdmin<T extends Record<string, Promise<unknown>>>(
  promises: T,
): Promise<{ [K in keyof T]: Awaited<T[K]> | null }> {
  const keys = Object.keys(promises) as (keyof T)[];
  const settled = await Promise.allSettled(keys.map((k) => promises[k]));

  const out = {} as { [K in keyof T]: Awaited<T[K]> | null };

  keys.forEach((key, index) => {
    const result = settled[index]!;
    if (result.status === 'fulfilled') {
      out[key] = result.value as Awaited<T[typeof key]>;
    } else {
      console.warn('[admin] panel unavailable', {
        key: String(key),
        error: result.reason instanceof Error ? result.reason.message : String(result.reason),
      });
      out[key] = null;
    }
  });

  return out;
}
