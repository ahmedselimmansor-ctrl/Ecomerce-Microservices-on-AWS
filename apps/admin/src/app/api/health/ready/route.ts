import { NextResponse } from 'next/server';

/**
 * Readiness.
 *
 * Same reasoning as the storefront's: it checks configuration, not reachability.
 * Probing the services from here would put every admin pod into a polling loop
 * against seven backends, and would pull the operations tool out of the load
 * balancer during exactly the incident someone needs it for.
 */
export const dynamic = 'force-dynamic';

const REQUIRED = [
  'SOUQ_IDENTITY_URL',
  'SOUQ_CATALOG_URL',
  'SOUQ_ORDER_URL',
] as const;

export function GET() {
  const missing = REQUIRED.filter((name) => !process.env[name]);

  if (missing.length > 0) {
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
