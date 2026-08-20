import { NextResponse } from 'next/server';

/**
 * Readiness.
 *
 * Says whether this pod should be sent traffic. Unlike liveness it may check
 * configuration — but it deliberately does **not** call the services.
 *
 * Calling them would mean every pod polls seven services every five seconds,
 * which is a self-inflicted load spike proportional to the fleet size. It would
 * also take the storefront out of the load balancer when a service it barely
 * uses is redeploying, even though the pages that do not touch that service
 * still render perfectly — `gather()` in `bff.ts` exists precisely so a
 * recommendation outage degrades one strip rather than the site.
 *
 * What it does check is that the URLs are configured at all. A pod started with
 * a missing environment variable will fail every request, and it is better to
 * never receive traffic than to receive it and 500.
 */
export const dynamic = 'force-dynamic';

const REQUIRED = [
  'SOUQ_IDENTITY_URL',
  'SOUQ_CATALOG_URL',
  'SOUQ_CART_URL',
  'SOUQ_ORDER_URL',
  'SOUQ_SEARCH_URL',
] as const;

export function GET() {
  const missing = REQUIRED.filter((name) => !process.env[name]);

  if (missing.length > 0) {
    // 503 keeps the pod out of the load balancer. The names are in the body
    // because whoever is looking at this is an operator running `curl` against
    // a pod that will not come up, and the answer should be in front of them.
    return NextResponse.json(
      { status: 'not-ready', missing },
      { status: 503, headers: { 'Cache-Control': 'no-store' } },
    );
  }

  return NextResponse.json(
    { status: 'ready' },
    { headers: { 'Cache-Control': 'no-store' } },
  );
}
