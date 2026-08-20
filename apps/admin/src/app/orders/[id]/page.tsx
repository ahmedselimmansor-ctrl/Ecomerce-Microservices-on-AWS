import Link from 'next/link';
import { ChevronLeft } from 'lucide-react';

import { SagaTrace } from '@souq/contracts';

import { adminCall } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin } from '@/lib/session';
import { Unauthorised } from '@/components/layout/unauthorised';
import { Button } from '@/components/ui/button';
import { SagaInspectorPanel } from '@/components/saga-inspector-panel';

export const metadata = { title: 'Order' };
export const dynamic = 'force-dynamic';

export default async function OrderDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;

  let session;
  try {
    session = await requireAdmin();
  } catch (error) {
    if (error instanceof NotAuthorised) return <Unauthorised reason={error.reason} />;
    throw error;
  }

  const trace = await adminCall({
    service: 'order',
    path: `/v1/admin/orders/${encodeURIComponent(id)}/saga`,
    schema: SagaTrace,
    accessToken: session.accessToken,
  });

  return (
    <div className="space-y-6">
      <div>
        <Button variant="ghost" size="sm" asChild className="-ml-3">
          <Link href="/orders">
            <ChevronLeft className="h-4 w-4" />
            All orders
          </Link>
        </Button>

        <h1 className="mt-2 font-mono text-lg font-bold tracking-tight">{trace.orderId}</h1>
        <p className="font-mono text-xs text-muted-foreground">
          correlation {trace.correlationId}
        </p>
      </div>

      {/*
        `canAct` is true because reaching this page already required an ADMIN or
        OPS role with MFA. The inspector itself decides which individual
        controls are safe — in particular it removes the cancel control
        entirely once the saga is past the point of no return, rather than
        disabling it. A disabled button invites someone to find another way.
      */}
      <SagaInspectorPanel trace={trace} canAct />
    </div>
  );
}
