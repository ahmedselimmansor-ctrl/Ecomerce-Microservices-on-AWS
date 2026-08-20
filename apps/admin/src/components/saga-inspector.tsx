'use client';

import { useMemo } from 'react';
import { AlertTriangle, Ban, CheckCircle2, Clock, Loader2, RotateCw, XCircle } from 'lucide-react';

import { SagaTrace, type OrderStatus } from '@souq/contracts';
import { cn } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import {
  Tooltip, TooltipContent, TooltipProvider, TooltipTrigger,
} from '@/components/ui/tooltip';

/**
 * The saga inspector.
 *
 * This is the screen support and on-call actually use, and it is the reason
 * `saga_steps` is a separate table rather than a status column. When a
 * customer says "my order is stuck", the question is never "what state is it
 * in" — it is "which of the five participants has not replied, how many times
 * have we asked, and is it still safe to cancel".
 *
 * The single most important thing on this screen is the point-of-no-return
 * boundary. `docs/DESIGN-INVARIANTS.md` §1 proves that compensating past it loses
 * money, so once an order crosses it the cancel control is not disabled — it
 * is **absent**, and the reason is stated inline. A disabled button invites
 * someone to find another way; an explanation does not.
 */

type Step = SagaTrace['steps'][number];

/**
 * The happy path, in order. Compensation steps are rendered separately
 * because they are not a continuation of this sequence — they are what
 * happens instead of it.
 */
const FORWARD_STEPS = ['RESERVE', 'AUTHORIZE', 'COMMIT', 'CAPTURE'] as const;
const COMPENSATION_STEPS = ['RELEASE', 'VOID'] as const;

const STEP_DETAIL: Record<string, { owner: string; what: string; reversible: boolean }> = {
  RESERVE:   { owner: 'inventory-service', what: 'Hold the stock',        reversible: true },
  AUTHORIZE: { owner: 'payment-service',   what: 'Ring-fence the funds',  reversible: true },
  COMMIT:    { owner: 'inventory-service', what: 'Deduct the stock',      reversible: false },
  CAPTURE:   { owner: 'payment-service',   what: 'Take the money',        reversible: false },
  RELEASE:   { owner: 'inventory-service', what: 'Return the stock',      reversible: true },
  VOID:      { owner: 'payment-service',   what: 'Release the funds',     reversible: true },
};

/** Mirrors saga.RollbackForbidden in the Go implementation. */
const PAST_NO_RETURN: ReadonlySet<OrderStatus> = new Set(['PAID', 'STOCK_COMMITTED', 'CONFIRMED']);

interface Props {
  trace: SagaTrace;
  onRetryStep?: (step: string) => void;
  onForceCancel?: () => void;
  canAct: boolean;
}

export function SagaInspector({ trace, onRetryStep, onForceCancel, canAct }: Props) {
  const byStep = useMemo(
    () => new Map(trace.steps.map((s) => [s.step, s])),
    [trace.steps],
  );

  const pastNoReturn = trace.rollbackForbidden || PAST_NO_RETURN.has(trace.status);
  const compensating = trace.status === 'COMPENSATING';

  // A step is overdue when it was sent, never acknowledged, and its deadline
  // has passed. This is the same predicate the sweeper uses, so what the
  // operator sees matches what the system is acting on.
  const overdue = trace.steps.filter(
    (s) => s.state === 'SENT' && s.deadlineAt !== null && new Date(s.deadlineAt) < new Date(),
  );

  return (
    <div className="space-y-6">
      {/* ---------------------------------------------------------------- */}
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="space-y-1">
          <div className="flex items-center gap-2">
            <h2 className="font-mono text-lg">{trace.orderId}</h2>
            <StatusBadge status={trace.status} />
          </div>
          <p className="text-sm text-muted-foreground">
            Started {formatRelative(trace.startedAt)} · correlation{' '}
            <span className="font-mono text-xs">{trace.correlationId}</span>
          </p>
        </div>

        {/*
          The cancel control is absent, not disabled, past the point of no
          return. A greyed-out button reads as "ask someone with more
          permissions"; its absence plus the explanation below reads as "this
          is not a thing that can be done".
        */}
        {canAct && !pastNoReturn && trace.status !== 'CANCELLED' && (
          <Button variant="destructive" size="sm" onClick={onForceCancel}>
            <Ban className="mr-2 size-4" aria-hidden />
            Cancel and compensate
          </Button>
        )}
      </header>

      {/* ---------------------------------------------------------------- */}
      {pastNoReturn && (
        <Alert>
          <AlertTriangle className="size-4" aria-hidden />
          <AlertTitle>Past the point of no return — this order cannot be rolled back</AlertTitle>
          <AlertDescription className="space-y-2 text-sm">
            <p>
              The <code className="font-mono text-xs">inventory.commit</code> command has been
              sent, so the stock may already be picked. Compensating from here would release a
              payment for goods that have left the warehouse.
            </p>
            <p>
              This order rolls forward or is settled by hand. To refund the customer, issue a
              refund against the payment — do not cancel the saga.
            </p>
            <p className="text-xs text-muted-foreground">
              The state machine, the sweeper and a database CHECK constraint each independently
              refuse this. See <code className="font-mono">docs/DESIGN-INVARIANTS.md</code> §1.
            </p>
          </AlertDescription>
        </Alert>
      )}

      {overdue.length > 0 && !pastNoReturn && (
        <Alert variant="destructive">
          <Clock className="size-4" aria-hidden />
          <AlertTitle>
            {overdue.length === 1
              ? `Waiting on ${overdue[0]!.step}`
              : `${overdue.length} steps are overdue`}
          </AlertTitle>
          <AlertDescription className="text-sm">
            {overdue.map((s) => (
              <div key={s.step}>
                <span className="font-medium">{s.step}</span> was sent{' '}
                {formatRelative(s.sentAt!)} and {STEP_DETAIL[s.step]?.owner} has not replied.
                Attempt {s.attempts}.
              </div>
            ))}
          </AlertDescription>
        </Alert>
      )}

      {/* ---------------------------------------------------------------- */}
      <section>
        <h3 className="mb-3 text-sm font-medium text-muted-foreground">Forward path</h3>
        <ol className="space-y-2">
          {FORWARD_STEPS.map((name, i) => {
            const step = byStep.get(name);
            const detail = STEP_DETAIL[name]!;
            // The boundary sits between AUTHORIZE and COMMIT.
            const crossesBoundary = name === 'COMMIT';

            return (
              <li key={name}>
                {crossesBoundary && (
                  <div
                    className="my-3 flex items-center gap-3 text-xs uppercase tracking-wide text-amber-600 dark:text-amber-500"
                    role="separator"
                    aria-label="Point of no return"
                  >
                    <span className="h-px flex-1 bg-amber-500/40" />
                    point of no return
                    <span className="h-px flex-1 bg-amber-500/40" />
                  </div>
                )}
                <StepRow
                  index={i + 1}
                  name={name}
                  step={step}
                  owner={detail.owner}
                  what={detail.what}
                  reversible={detail.reversible}
                  canRetry={canAct && step?.state === 'FAILED'}
                  onRetry={() => onRetryStep?.(name)}
                />
              </li>
            );
          })}
        </ol>
      </section>

      {(compensating || COMPENSATION_STEPS.some((s) => byStep.has(s))) && (
        <section>
          <h3 className="mb-3 text-sm font-medium text-muted-foreground">
            Compensation
            {compensating && (
              <span className="ml-2 font-normal">— running now, nothing will be charged</span>
            )}
          </h3>
          <ol className="space-y-2">
            {COMPENSATION_STEPS.map((name) => {
              const step = byStep.get(name);
              if (!step) return null;
              const detail = STEP_DETAIL[name]!;
              return (
                <li key={name}>
                  <StepRow
                    name={name}
                    step={step}
                    owner={detail.owner}
                    what={detail.what}
                    reversible
                    canRetry={canAct && step.state === 'FAILED'}
                    onRetry={() => onRetryStep?.(name)}
                  />
                </li>
              );
            })}
          </ol>
        </section>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------

interface StepRowProps {
  index?: number;
  name: string;
  step: Step | undefined;
  owner: string;
  what: string;
  reversible: boolean;
  canRetry: boolean;
  onRetry: () => void;
}

function StepRow({ index, name, step, owner, what, reversible, canRetry, onRetry }: StepRowProps) {
  const state = step?.state ?? 'PENDING';

  const isOverdue =
    step?.state === 'SENT' && step.deadlineAt !== null && new Date(step.deadlineAt) < new Date();

  return (
    <div
      className={cn(
        'flex items-start gap-3 rounded-lg border px-3 py-2.5',
        state === 'ACKED' && 'border-emerald-500/30 bg-emerald-500/5',
        state === 'FAILED' && 'border-destructive/40 bg-destructive/5',
        isOverdue && 'border-amber-500/40 bg-amber-500/5',
        state === 'PENDING' && 'border-dashed opacity-60',
      )}
    >
      <StepIcon state={state} overdue={isOverdue} />

      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2">
          {index !== undefined && (
            <span className="text-xs tabular-nums text-muted-foreground">{index}.</span>
          )}
          <span className="text-sm font-medium">{name}</span>
          <span className="text-xs text-muted-foreground">{what}</span>

          {!reversible && (
            <TooltipProvider>
              <Tooltip>
                <TooltipTrigger asChild>
                  <Badge variant="outline" className="border-amber-500/50 text-[10px] text-amber-600">
                    irreversible
                  </Badge>
                </TooltipTrigger>
                <TooltipContent className="max-w-xs">
                  Once this succeeds it cannot be undone by the saga. A refund or a manual
                  restock is the only remedy.
                </TooltipContent>
              </Tooltip>
            </TooltipProvider>
          )}
        </div>

        <div className="mt-0.5 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
          <span>{owner}</span>
          {step && step.attempts > 1 && (
            // More than one attempt is worth surfacing: it means a retry
            // happened, which usually means the participant was slow or down.
            <span className={cn(step.attempts >= 5 && 'font-medium text-destructive')}>
              {step.attempts} attempts
            </span>
          )}
          {step?.sentAt && <span>sent {formatRelative(step.sentAt)}</span>}
          {step?.ackedAt && <span>acked {formatRelative(step.ackedAt)}</span>}
          {isOverdue && step?.deadlineAt && (
            <span className="font-medium text-amber-600">
              overdue since {formatRelative(step.deadlineAt)}
            </span>
          )}
        </div>

        {step?.error && (
          <p className="mt-1.5 rounded bg-destructive/10 px-2 py-1 font-mono text-xs text-destructive">
            {step.error}
          </p>
        )}

        {step?.eventId && (
          // The event id is the inbox dedup key. It is what someone greps for
          // in the logs of the participant that did or did not reply.
          <p className="mt-1 font-mono text-[10px] text-muted-foreground">
            event {step.eventId}
          </p>
        )}
      </div>

      {canRetry && (
        <Button variant="ghost" size="sm" onClick={onRetry}>
          <RotateCw className="mr-1.5 size-3.5" aria-hidden />
          Retry
        </Button>
      )}
    </div>
  );
}

function StepIcon({ state, overdue }: { state: string; overdue: boolean }) {
  const cls = 'mt-0.5 size-5 shrink-0';

  if (state === 'ACKED') return <CheckCircle2 className={cn(cls, 'text-emerald-600')} aria-label="acknowledged" />;
  if (state === 'FAILED') return <XCircle className={cn(cls, 'text-destructive')} aria-label="failed" />;
  if (state === 'TIMED_OUT' || overdue) return <Clock className={cn(cls, 'text-amber-600')} aria-label="overdue" />;
  if (state === 'SENT') return <Loader2 className={cn(cls, 'animate-spin text-primary')} aria-label="in flight" />;
  if (state === 'SKIPPED') return <Ban className={cn(cls, 'text-muted-foreground/40')} aria-label="skipped" />;

  return <div className={cn(cls, 'rounded-full border-2 border-dashed border-muted-foreground/30')} aria-label="not started" />;
}

function StatusBadge({ status }: { status: OrderStatus }) {
  const variant =
    status === 'CONFIRMED' || status === 'DELIVERED' ? 'default'
    : status === 'CANCELLED' || status === 'REFUNDED' ? 'destructive'
    : status === 'COMPENSATING' ? 'outline'
    : 'secondary';

  return <Badge variant={variant} className="font-mono text-xs">{status}</Badge>;
}

/**
 * Relative time. Operators reason in "how long ago", not in timestamps —
 * "sent 4m ago" answers "is this stuck?" and "2026-08-17T10:04:33Z" does not.
 */
function formatRelative(iso: string): string {
  const seconds = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);

  if (seconds < 5) return 'just now';
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}
