import Link from 'next/link';
import { Suspense } from 'react';
import { AlertTriangle, Clock, ShoppingCart, TrendingUp } from 'lucide-react';

import { DashboardKpis, formatMoney } from '@souq/contracts';

import { adminCall, gatherAdmin } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin } from '@/lib/session';
import { PanelUnavailable, Unauthorised } from '@/components/layout/unauthorised';
import { Badge } from '@/components/ui/badge';
import { Card, CardContent } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { cn } from '@/lib/utils';

export const metadata = { title: 'Overview' };

/** Nothing in an operations tool may be cached. See admin-api.ts. */
export const dynamic = 'force-dynamic';

export default async function OverviewPage() {
  try {
    await requireAdmin();
  } catch (error) {
    if (error instanceof NotAuthorised) return <Unauthorised reason={error.reason} />;
    throw error;
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-bold tracking-tight">Overview</h1>
        <p className="text-sm text-muted-foreground">
          Everything here is live. Nothing on this page is cached.
        </p>
      </div>

      <Suspense fallback={<KpiSkeleton />}>
        <Kpis />
      </Suspense>
    </div>
  );
}

async function Kpis() {
  const { accessToken } = await requireAdmin();

  const { kpis } = await gatherAdmin({
    kpis: adminCall({
      service: 'order',
      path: '/v1/admin/kpis',
      schema: DashboardKpis,
      accessToken,
    }),
  });

  if (!kpis) return <PanelUnavailable name="Key figures" />;

  return (
    <>
      {/*
        The two operational figures come first, above the commercial ones.
        A dashboard that leads with revenue trains people to read revenue; the
        numbers that mean "go and do something right now" are stuck sagas and
        DLQ depth, and they belong where the eye lands.
      */}
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Kpi
          label="Stuck sagas"
          value={kpis.stuckSagas.toLocaleString()}
          icon={AlertTriangle}
          // Any non-zero value is actionable. This is orders past their timeout
          // in a non-terminal state — every one is a customer waiting.
          tone={kpis.stuckSagas === 0 ? 'ok' : 'bad'}
          href="/orders?status=stuck"
          hint={kpis.stuckSagas === 0 ? 'Nothing is stuck' : 'Orders past their timeout'}
        />

        <Kpi
          label="Dead letters"
          value={kpis.dlqDepth.toLocaleString()}
          icon={AlertTriangle}
          tone={kpis.dlqDepth === 0 ? 'ok' : kpis.dlqDepth < 10 ? 'warn' : 'bad'}
          href="/dlq"
          hint="Messages no consumer could process"
        />

        <Kpi
          label="Checkout p99"
          value={`${(kpis.p99CheckoutMs / 1000).toFixed(1)}s`}
          icon={Clock}
          // The saga's own budget is a few seconds. Past ten, customers are
          // refreshing and placing the order twice.
          tone={kpis.p99CheckoutMs < 5_000 ? 'ok' : kpis.p99CheckoutMs < 10_000 ? 'warn' : 'bad'}
          hint="Placed to terminal state"
        />

        <Kpi
          label="Error budget burn"
          value={`${kpis.errorBudgetBurn.toFixed(1)}%`}
          icon={TrendingUp}
          // Bands from the alert rules in infra/terraform. Burning the month's
          // budget in a day is the signal, not the raw error rate.
          tone={kpis.errorBudgetBurn < 2 ? 'ok' : kpis.errorBudgetBurn < 10 ? 'warn' : 'bad'}
          hint="Of this month's allowance"
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Kpi
          label="Orders today"
          value={kpis.ordersToday.toLocaleString()}
          icon={ShoppingCart}
          tone="neutral"
        />
        <Kpi
          label="Revenue today"
          value={formatMoney(kpis.revenueToday)}
          icon={TrendingUp}
          tone="neutral"
        />
        <Kpi
          label="Average order"
          value={formatMoney(kpis.averageOrderValue)}
          icon={TrendingUp}
          tone="neutral"
        />
        <Kpi
          label="Abandonment"
          value={`${(kpis.cartAbandonmentRate * 100).toFixed(1)}%`}
          icon={ShoppingCart}
          tone="neutral"
          hint={`Conversion ${(kpis.conversionRate * 100).toFixed(1)}%`}
        />
      </div>
    </>
  );
}

function Kpi({
  label,
  value,
  icon: Icon,
  tone,
  hint,
  href,
}: {
  label: string;
  value: string;
  icon: React.ComponentType<{ className?: string }>;
  tone: 'ok' | 'warn' | 'bad' | 'neutral';
  hint?: string;
  href?: string;
}) {
  const body = (
    <Card className={cn('h-full transition-colors', href && 'hover:border-foreground/30')}>
      <CardContent className="p-4">
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {label}
          </span>
          <Icon
            className={cn(
              'h-4 w-4',
              tone === 'ok' && 'text-success',
              tone === 'warn' && 'text-warning',
              tone === 'bad' && 'text-destructive',
              tone === 'neutral' && 'text-muted-foreground',
            )}
          />
        </div>

        <p
          className={cn(
            'tabular mt-2 text-2xl font-bold',
            tone === 'bad' && 'text-destructive',
            tone === 'warn' && 'text-warning',
          )}
        >
          {value}
        </p>

        {hint && <p className="mt-1 text-xs text-muted-foreground">{hint}</p>}

        {/*
          A colour alone is never the signal. Roughly one in twelve men has a
          red-green deficiency, and an operations dashboard where "bad" is only
          conveyed by hue is one a meaningful fraction of the team cannot read.
        */}
        {tone === 'bad' && (
          <Badge variant="destructive" className="mt-2">
            Needs attention
          </Badge>
        )}
        {tone === 'warn' && (
          <Badge variant="warning" className="mt-2">
            Watch
          </Badge>
        )}
      </CardContent>
    </Card>
  );

  return href ? <Link href={href}>{body}</Link> : body;
}

function KpiSkeleton() {
  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {Array.from({ length: 8 }, (_, i) => (
        <Card key={i}>
          <CardContent className="space-y-2 p-4">
            <Skeleton className="h-3 w-20" />
            <Skeleton className="h-7 w-16" />
            <Skeleton className="h-3 w-24" />
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
