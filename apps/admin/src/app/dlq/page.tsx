import { Suspense } from 'react';
import { AlertTriangle, CheckCircle2 } from 'lucide-react';

import { DlqMessage } from '@souq/contracts';
import { z } from 'zod';

import { adminCall, gatherAdmin } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin } from '@/lib/session';
import { PanelUnavailable, Unauthorised } from '@/components/layout/unauthorised';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { DlqActions } from '@/components/dlq-actions';

export const metadata = { title: 'Dead letters' };
export const dynamic = 'force-dynamic';

const DlqPage = z.object({
  items: z.array(DlqMessage),
  nextCursor: z.string().nullable(),
  hasMore: z.boolean(),
}).strict();

export default async function DlqPageRoute() {
  try {
    await requireAdmin();
  } catch (error) {
    if (error instanceof NotAuthorised) return <Unauthorised reason={error.reason} />;
    throw error;
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-bold tracking-tight">Dead letters</h1>
        <p className="max-w-3xl text-sm text-muted-foreground">
          Messages a consumer could not process after its retry budget ran out. Every one is an
          event that has <em>not</em> been applied — a stock adjustment that never landed, a
          notification never sent. The queue being empty is the normal state.
        </p>
      </div>

      <Suspense fallback={<TableSkeleton />}>
        <DlqTable />
      </Suspense>
    </div>
  );
}

async function DlqTable() {
  const { accessToken } = await requireAdmin();

  const { page } = await gatherAdmin({
    page: adminCall({
      service: 'order',
      path: '/v1/admin/dlq?limit=50',
      schema: DlqPage,
      accessToken,
    }),
  });

  if (!page) return <PanelUnavailable name="The dead-letter queue" />;

  if (page.items.length === 0) {
    return (
      <Alert variant="success">
        <CheckCircle2 className="h-4 w-4" />
        <AlertTitle>Nothing here</AlertTitle>
        <AlertDescription>
          No message has exhausted its retries. This is what a healthy platform looks like.
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <>
      <Alert variant="destructive">
        <AlertTriangle className="h-4 w-4" />
        <AlertTitle>
          {page.items.length}
          {page.hasMore ? '+' : ''} message{page.items.length === 1 ? '' : 's'} need a decision
        </AlertTitle>
        <AlertDescription>
          Replaying is safe: every consumer in this platform dedupes on{' '}
          <code className="font-mono text-xs">event_id</code> through its inbox table, so a message
          that did partially apply will not apply twice. Discarding is not reversible.
        </AlertDescription>
      </Alert>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Failed</TableHead>
            <TableHead>Event</TableHead>
            <TableHead>Topic</TableHead>
            <TableHead className="text-right">Attempts</TableHead>
            <TableHead>Reason</TableHead>
            <TableHead className="text-right">Action</TableHead>
          </TableRow>
        </TableHeader>

        <TableBody>
          {page.items.map((message) => (
            <TableRow key={message.id}>
              <TableCell className="whitespace-nowrap">
                <time dateTime={message.failedAt} className="text-xs">
                  {new Date(message.failedAt).toLocaleString('en-GB', {
                    // A fixed 24-hour format. Ambiguity between 03:00 and 15:00
                    // in an incident timeline is worth avoiding.
                    year: 'numeric', month: 'short', day: '2-digit',
                    hour: '2-digit', minute: '2-digit', hour12: false,
                  })}
                </time>
              </TableCell>

              <TableCell>
                <div className="font-mono text-xs">{message.eventType}</div>
                <div className="font-mono text-[10px] text-muted-foreground">{message.eventId}</div>
              </TableCell>

              <TableCell className="font-mono text-xs">{message.originalTopic}</TableCell>

              <TableCell className="tabular text-right">
                <Badge variant={message.attempts >= 10 ? 'destructive' : 'secondary'}>
                  {message.attempts}
                </Badge>
              </TableCell>

              <TableCell className="max-w-sm">
                {/*
                  The `x-dlq-reason` header, verbatim. Truncated visually but
                  kept whole in the title — the exact string is what someone
                  greps the logs for.
                */}
                <span className="line-clamp-2 text-xs" title={message.reason}>
                  {message.reason}
                </span>
              </TableCell>

              <TableCell className="text-right">
                <DlqActions messageId={message.id} eventId={message.eventId} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </>
  );
}

function TableSkeleton() {
  return (
    <div className="space-y-2">
      {Array.from({ length: 6 }, (_, i) => (
        <Skeleton key={i} className="h-12 w-full" />
      ))}
    </div>
  );
}
