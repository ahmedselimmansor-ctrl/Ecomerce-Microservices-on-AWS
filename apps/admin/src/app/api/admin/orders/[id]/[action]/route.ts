import { z } from 'zod';

import { AdminApiError, adminCall } from '@/lib/admin-api';
import { adminProblem, withAdmin } from '../../../_guard';

const Action = z.enum(['retry', 'cancel']);
const RetryBody = z.object({
  step: z.enum(['RESERVE', 'AUTHORIZE', 'COMMIT', 'CAPTURE', 'RELEASE', 'VOID']),
}).strict();

/**
 * Operator actions on a saga.
 *
 * Neither action decides for itself whether it is safe. **order-service is the
 * authority** — its state machine refuses a rollback past the point of no
 * return, and a `CHECK` constraint refuses it again at the database
 * (docs/DESIGN-INVARIANTS.md §1, enforced in three independent places).
 *
 * This route does not re-implement that rule, and that is deliberate: a fourth
 * copy of it here would be a fourth thing to keep in step, and the one most
 * likely to drift. The UI hides the control, the service enforces it, and a
 * request that gets past the UI anyway is correctly rejected upstream.
 */
export async function POST(
  request: Request,
  { params }: { params: Promise<{ id: string; action: string }> },
) {
  const { id, action } = await params;

  const parsed = Action.safeParse(action);
  if (!parsed.success) {
    return adminProblem(
      new AdminApiError(400, 'VALIDATION_FAILED', '', `unknown action '${action}'`),
      `/api/admin/orders/${id}/${action}`,
    );
  }

  const instance = `/api/admin/orders/${id}/${parsed.data}`;

  if (parsed.data === 'retry') {
    let body: z.infer<typeof RetryBody>;
    try {
      body = RetryBody.parse(await request.json());
    } catch {
      return adminProblem(
        new AdminApiError(400, 'VALIDATION_FAILED', '', 'a retry must name a saga step'),
        instance,
      );
    }

    return withAdmin(instance, async (session) =>
      adminCall({
        service: 'order',
        path: `/v1/admin/orders/${encodeURIComponent(id)}/retry`,
        schema: z.unknown(),
        method: 'POST',
        body,
        accessToken: session.accessToken,
        // Retrying a step is idempotent by construction — the participant
        // dedupes on event id — but the key stops a double-click producing two
        // outbound messages and two log lines to reconcile.
        idempotencyKey: `saga:retry:${id}:${body.step}`,
      }),
    );
  }

  return withAdmin(instance, async (session) =>
    adminCall({
      service: 'order',
      path: `/v1/admin/orders/${encodeURIComponent(id)}/cancel`,
      schema: z.unknown(),
      method: 'POST',
      body: { reason: 'OPERATOR_CANCELLED' },
      accessToken: session.accessToken,
      idempotencyKey: `saga:cancel:${id}`,
    }),
  );
}
