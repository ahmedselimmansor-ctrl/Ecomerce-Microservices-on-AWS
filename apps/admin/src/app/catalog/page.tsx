import Image from 'next/image';
import { Suspense } from 'react';

import { Product, formatMoney } from '@souq/contracts';
import { z } from 'zod';

import { adminCall, gatherAdmin } from '@/lib/admin-api';
import { NotAuthorised, requireAdmin } from '@/lib/session';
import { PanelUnavailable, Unauthorised } from '@/components/layout/unauthorised';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { ProductStatusControl } from '@/components/product-status-control';
import { PriceControl } from '@/components/price-control';

export const metadata = { title: 'Catalogue' };
export const dynamic = 'force-dynamic';

const AdminProductPage = z.object({
  items: z.array(z.object({
    product: Product,
    /** The optimistic-lock token. Every update must present it. */
    version: z.number().int().min(0),
    categoryId: z.string().nullable(),
  }).strict()),
  page: z.number().int(),
  size: z.number().int(),
  total: z.number().int(),
  totalPages: z.number().int(),
  hasNext: z.boolean(),
}).strict();

/**
 * The catalogue grid.
 *
 * Shows DRAFT and ARCHIVED products alongside live ones — that is the whole
 * point of an admin view, and it is why the public product endpoint refuses to
 * do it without a privileged token.
 *
 * The `version` column is carried through to every control on the row. Two
 * merchandisers editing the same product is the normal state of a merchandising
 * team, not a rare race, and without the version the second save silently
 * discards the first.
 */
export default async function CatalogPage({
  searchParams,
}: {
  searchParams: Promise<{ page?: string; status?: string }>;
}) {
  try {
    await requireAdmin();
  } catch (error) {
    if (error instanceof NotAuthorised) return <Unauthorised reason={error.reason} />;
    throw error;
  }

  const params = await searchParams;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-bold tracking-tight">Catalogue</h1>
        <p className="max-w-3xl text-sm text-muted-foreground">
          Drafts and archived products are included. A price change here writes an audit row and
          emits a compacted-topic event — the search index follows within seconds.
        </p>
      </div>

      <Suspense key={JSON.stringify(params)} fallback={<TableSkeleton />}>
        <ProductTable page={params.page} status={params.status} />
      </Suspense>
    </div>
  );
}

async function ProductTable({ page, status }: { page?: string; status?: string }) {
  const { accessToken } = await requireAdmin();

  const search = new URLSearchParams({ size: '50' });
  search.set('page', String(Math.max(0, Number.parseInt(page ?? '0', 10) || 0)));
  if (status) search.set('status', status);

  const { result } = await gatherAdmin({
    result: adminCall({
      service: 'catalog',
      path: `/v1/products?${search.toString()}`,
      schema: AdminProductPage,
      accessToken,
    }),
  });

  if (!result) return <PanelUnavailable name="The catalogue" />;

  if (result.items.length === 0) {
    return <p className="py-12 text-center text-sm text-muted-foreground">No products.</p>;
  }

  return (
    <>
      <p className="tabular text-xs text-muted-foreground">
        {result.total.toLocaleString()} products · page {result.page + 1} of {result.totalPages}
      </p>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-14" />
            <TableHead>Product</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Price</TableHead>
            <TableHead className="text-right">Stock</TableHead>
            <TableHead className="text-right">Version</TableHead>
          </TableRow>
        </TableHeader>

        <TableBody>
          {result.items.map(({ product, version }) => {
            const cheapest = product.variants.length > 0
              ? product.variants.reduce((a, b) => (b.price.amount < a.price.amount ? b : a))
              : null;

            const image = product.images[0] ?? cheapest?.images[0] ?? null;

            return (
              <TableRow key={product.id}>
                <TableCell>
                  <div className="relative h-10 w-10 overflow-hidden rounded bg-muted">
                    {image && (
                      <Image src={image.url} alt="" fill sizes="40px" className="object-cover" />
                    )}
                  </div>
                </TableCell>

                <TableCell>
                  <div className="text-sm font-medium">{product.title}</div>
                  <div className="font-mono text-[10px] text-muted-foreground">
                    {product.id} · /{product.slug}
                  </div>
                </TableCell>

                <TableCell>
                  <ProductStatusControl
                    productId={product.id}
                    status={product.status}
                    version={version}
                  />
                </TableCell>

                <TableCell className="text-right">
                  {cheapest ? (
                    <PriceControl
                      productId={product.id}
                      sku={cheapest.sku}
                      price={cheapest.price}
                      listPrice={cheapest.listPrice ?? null}
                    />
                  ) : (
                    <span className="text-xs text-muted-foreground">no variants</span>
                  )}
                </TableCell>

                <TableCell className="tabular text-right text-sm">
                  {/*
                    Summed across variants, and `null` is preserved rather than
                    coerced to 0. Null means inventory has not reported — an em
                    dash says "we do not know", where "0" would say "none left"
                    and send someone to reorder stock that exists.
                  */}
                  {product.variants.every((v) => v.available === null) ? (
                    <span className="text-muted-foreground" title="Inventory has not reported">
                      —
                    </span>
                  ) : (
                    <StockCell
                      total={product.variants.reduce((sum, v) => sum + (v.available ?? 0), 0)}
                      partial={product.variants.some((v) => v.available === null)}
                    />
                  )}
                </TableCell>

                <TableCell className="tabular text-right font-mono text-xs text-muted-foreground">
                  {version}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}

function StockCell({ total, partial }: { total: number; partial: boolean }) {
  return (
    <span className={total === 0 ? 'text-destructive' : total <= 5 ? 'text-warning' : ''}>
      {total.toLocaleString()}
      {partial && (
        <span className="ml-1 text-muted-foreground" title="Some variants have not reported">
          +?
        </span>
      )}
    </span>
  );
}

function TableSkeleton() {
  return (
    <div className="space-y-2">
      {Array.from({ length: 10 }, (_, i) => (
        <Skeleton key={i} className="h-14 w-full" />
      ))}
    </div>
  );
}
