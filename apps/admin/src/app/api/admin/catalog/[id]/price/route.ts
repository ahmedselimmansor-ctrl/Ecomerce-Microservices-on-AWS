import { z } from 'zod';

import { adminCall } from '@/lib/admin-api';
import { withAdmin } from '../../../_guard';

const PriceBody = z.object({
  sku: z.string().min(1).max(64),
  /** Minor units. The client converts; this never sees a decimal. */
  price: z.number().int().min(0),
  listPrice: z.number().int().min(0).nullable(),
  currency: z.string().regex(/^[A-Z]{3}$/),
  // Required, and long enough to be a sentence rather than a keystroke.
  // It lands in price_history, which is what finance reads back.
  reason: z.string().min(3).max(200),
}).strict();

/**
 * Change a price.
 *
 * A separate endpoint from the general update, mirroring catalog-service's own
 * split. Folding it into a PATCH would mean every no-op save writes an audit
 * row, and a `price_history` full of rows where nothing changed stops being
 * usable evidence of what a price actually was.
 *
 * There is no idempotency key. Two deliberate price changes to the same SKU
 * with the same reason are two events finance should see, not one — unlike a
 * retry, which the operator would recognise as such.
 */
export async function POST(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  return withAdmin(`/api/admin/catalog/${id}/price`, async (session) => {
    const body = PriceBody.parse(await request.json());

    return adminCall({
      service: 'catalog',
      path: `/v1/admin/products/${encodeURIComponent(id)}/variants/${encodeURIComponent(body.sku)}/price`,
      schema: z.unknown(),
      method: 'POST',
      body: {
        price: body.price,
        listPrice: body.listPrice,
        currency: body.currency,
        reason: body.reason,
      },
      accessToken: session.accessToken,
    });
  });
}
