import { NextResponse } from 'next/server';

/**
 * Liveness.
 *
 * Answers exactly one question: is this process able to serve a request at all?
 * It touches **nothing** — no service, no cache, no filesystem — and that is the
 * entire design.
 *
 * A liveness probe that checks a dependency fails for every pod at once when
 * that dependency has a blip, so Kubernetes restarts the whole fleet
 * simultaneously and a brownout becomes an outage. The restart also cannot
 * help: the pod was never the problem.
 *
 * Readiness is the probe that is allowed to have opinions about dependencies.
 */
export const dynamic = 'force-dynamic';

export function GET() {
  return NextResponse.json(
    { status: 'ok' },
    { headers: { 'Cache-Control': 'no-store' } },
  );
}
