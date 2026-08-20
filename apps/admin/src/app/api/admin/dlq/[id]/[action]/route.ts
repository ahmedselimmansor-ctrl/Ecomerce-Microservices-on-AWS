import { z } from 'zod';

import { adminCall } from '@/lib/admin-api';
import { adminProblem, withAdmin } from '../../../_guard';

const Action = z.enum(['replay', 'discard']);

/**
 * Replay or discard a dead-lettered message.
 *
 * The action is validated against an enum rather than interpolated into the
 * upstream path. Without that, `/api/admin/dlq/x/..%2Fpurge` would reach
 * order-service as a path it never meant to expose — the classic path-traversal
 * shape, moved from a filesystem to a URL.
 *
 * Replay carries an idempotency key derived from the message id, so an operator
 * double-clicking Replay produces one replay rather than two.
 */
export async function POST(
  _request: Request,
  { params }: { params: Promise<{ id: string; action: string }> },
) {
  const { id, action } = await params;

  const parsed = Action.safeParse(action);
  if (!parsed.success) {
    return adminProblem(
      new (await import('@/lib/admin-api')).AdminApiError(
        400, 'VALIDATION_FAILED', '', `unknown action '${action}'`,
      ),
      `/api/admin/dlq/${id}/${action}`,
    );
  }

  return withAdmin(`/api/admin/dlq/${id}/${parsed.data}`, async (session) =>
    adminCall({
      service: 'order',
      path: `/v1/admin/dlq/${encodeURIComponent(id)}/${parsed.data}`,
      schema: z.unknown(),
      method: 'POST',
      accessToken: session.accessToken,
      // Stable per message and action. A fresh uuid per click would defeat it.
      idempotencyKey: `dlq:${parsed.data}:${id}`,
    }),
  );
}
