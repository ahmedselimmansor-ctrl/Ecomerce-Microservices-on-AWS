import { z } from 'zod';

import { adminCall } from '@/lib/admin-api';
import { withAdmin } from '../../_guard';

const UpdateBody = z.object({
  version: z.number().int().min(0),
  title: z.string().max(500).optional(),
  description: z.string().max(20_000).optional(),
  brand: z.string().max(200).optional(),
  categorySlug: z.string().max(200).optional(),
  status: z.enum(['DRAFT', 'ACTIVE', 'ARCHIVED', 'DISCONTINUED']).optional(),
}).strict();

/**
 * Update a product.
 *
 * `version` is forwarded unchanged and a 409 is passed straight back to the
 * browser. This route deliberately does **not** re-read the product and retry
 * with a fresh version: that would reapply the operator's change on top of
 * someone else's without either of them seeing it, which is the exact lost
 * update the version column exists to prevent.
 */
export async function PATCH(
  request: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  return withAdmin(`/api/admin/catalog/${id}`, async (session) => {
    const body = UpdateBody.parse(await request.json());

    return adminCall({
      service: 'catalog',
      path: `/v1/admin/products/${encodeURIComponent(id)}`,
      schema: z.unknown(),
      method: 'PATCH',
      body,
      accessToken: session.accessToken,
    });
  });
}

/**
 * Archive a product.
 *
 * DELETE in HTTP terms, an UPDATE in the database. Orders reference SKUs, so a
 * hard delete would leave an order history that cannot render what was bought.
 */
export async function DELETE(
  _request: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;

  return withAdmin(`/api/admin/catalog/${id}`, async (session) =>
    adminCall({
      service: 'catalog',
      path: `/v1/admin/products/${encodeURIComponent(id)}`,
      schema: z.unknown(),
      method: 'DELETE',
      accessToken: session.accessToken,
    }),
  );
}
