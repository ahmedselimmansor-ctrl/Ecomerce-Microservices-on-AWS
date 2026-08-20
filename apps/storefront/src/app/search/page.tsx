import { Suspense } from 'react';
import Link from 'next/link';
import { AlertTriangle, SearchX } from 'lucide-react';

import { SearchResponse } from '@souq/contracts';

import { call } from '@/lib/bff';
import { toQuery } from '@/lib/utils';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from '@/components/ui/sheet';
import { ProductCard, ProductCardSkeleton, ProductGrid, fromSearchHit } from '@/components/catalog/product-card';
import { ActiveFilters, Facets } from '@/components/catalog/facets';
import { SortSelect } from '@/components/catalog/sort-select';
import { Pagination } from '@/components/catalog/pagination';

/**
 * The product listing page.
 *
 * Search, category browsing and "everything" are one route. They differ only in
 * query parameters, and splitting them into three would mean three copies of
 * the facet, sort and pagination logic that would drift apart within a month.
 *
 * Dynamic, not static: the whole page is a function of `searchParams`.
 */
export const dynamic = 'force-dynamic';

/** Anything not in this list is treated as a facet filter. */
const RESERVED_PARAMS = new Set(['q', 'page', 'size', 'sort', 'category', 'priceMin', 'priceMax', 'inStockOnly']);

type SearchParams = Record<string, string | string[] | undefined>;

export async function generateMetadata({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const params = await searchParams;
  const q = typeof params.q === 'string' ? params.q : '';
  const category = typeof params.category === 'string' ? params.category : '';

  const title = q ? `"${q}"` : category ? category.replace(/-/g, ' ') : 'All products';

  return {
    title,
    // A filtered listing is near-duplicate content and there are combinatorially
    // many of them. Indexing the lot dilutes the pages that should rank, so
    // only the clean, unfiltered listings are indexable.
    robots: Object.keys(params).some((k) => !RESERVED_PARAMS.has(k) || k === 'page')
      ? { index: false, follow: true }
      : { index: true, follow: true },
  };
}

export default async function SearchPage({
  searchParams,
}: {
  searchParams: Promise<SearchParams>;
}) {
  const params = await searchParams;

  return (
    <div className="container py-8">
      {/*
        Keyed on the serialised params so React discards the previous subtree
        and shows the skeleton when the filters change. Without the key it
        reuses the tree and the old results sit there looking current.
      */}
      <Suspense key={JSON.stringify(params)} fallback={<ResultsSkeleton />}>
        <Results params={params} />
      </Suspense>
    </div>
  );
}

async function Results({ params }: { params: SearchParams }) {
  const q = typeof params.q === 'string' ? params.q : '';
  const sort = typeof params.sort === 'string' ? params.sort : 'relevance';
  const page = Math.max(1, Number.parseInt(String(params.page ?? '1'), 10) || 1);
  const size = 24;

  // Everything that is not a reserved parameter is a facet filter. Passing them
  // through as repeated keys rather than a JSON blob keeps the URL readable and
  // means a facet value containing a comma cannot break the parse.
  const filters: Record<string, string[]> = {};
  for (const [key, value] of Object.entries(params)) {
    if (RESERVED_PARAMS.has(key) || value === undefined) continue;
    filters[key] = Array.isArray(value) ? value : [value];
  }

  const query = toQuery({
    q,
    sort,
    page,
    size,
    category: typeof params.category === 'string' ? params.category : undefined,
    inStockOnly: params.inStockOnly === 'true' ? 'true' : undefined,
    priceMin: typeof params.priceMin === 'string' ? params.priceMin : undefined,
    priceMax: typeof params.priceMax === 'string' ? params.priceMax : undefined,
    ...filters,
  });

  const results = await call({
    service: 'search',
    path: `/v1/search${query}`,
    schema: SearchResponse,
  });

  const heading = q ? `Results for "${q}"` : 'All products';

  return (
    <>
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{heading}</h1>
          <p aria-live="polite" className="tabular mt-1 text-sm text-muted-foreground">
            {/*
              Elasticsearch caps how deep it will count. `totalIsLowerBound`
              says the number is a floor, and rendering "10,000 results" when
              the truth is "at least 10,000" is a small lie that makes the last
              page of pagination behave inexplicably.
            */}
            {results.totalIsLowerBound ? 'More than ' : ''}
            {results.total.toLocaleString()} {results.total === 1 ? 'result' : 'results'}
          </p>
        </div>

        <div className="flex items-center gap-2">
          <Sheet>
            <SheetTrigger asChild>
              <Button variant="outline" size="sm" className="lg:hidden">
                Filters
              </Button>
            </SheetTrigger>
            <SheetContent side="left">
              <SheetHeader>
                <SheetTitle>Filters</SheetTitle>
              </SheetHeader>
              <div className="mt-4">
                <Facets facets={results.facets} />
              </div>
            </SheetContent>
          </Sheet>

          <SortSelect value={sort} />
        </div>
      </div>

      {/*
        Search fell back to a Postgres LIKE because OpenSearch was unavailable.
        Said plainly, because relevance ordering and facets are gone and the
        user will otherwise conclude the catalogue is broken.
      */}
      {results.degraded && (
        <Alert variant="warning" className="mt-4">
          <AlertTriangle className="h-4 w-4" />
          <AlertTitle>Search is running in a reduced mode</AlertTitle>
          <AlertDescription>
            Results may be less relevant and filters are unavailable. Everything is still
            purchasable.
          </AlertDescription>
        </Alert>
      )}

      <div className="mt-6 flex gap-8">
        {results.facets.length > 0 && (
          <div className="hidden w-60 shrink-0 lg:block">
            <Facets facets={results.facets} />
          </div>
        )}

        <div className="min-w-0 flex-1">
          <ActiveFilters facets={results.facets} />

          {results.hits.length === 0 ? (
            <EmptyResults query={q} didYouMean={results.didYouMean} />
          ) : (
            <>
              <ProductGrid>
                {results.hits.map((hit, index) => (
                  <ProductCard
                    key={hit.productId}
                    item={fromSearchHit(hit)}
                    // The first row is above the fold on every breakpoint.
                    // Lazy-loading the largest visible element is a direct hit
                    // to Largest Contentful Paint.
                    priority={index < 5}
                  />
                ))}
              </ProductGrid>

              <Pagination
                page={results.page}
                size={results.size}
                total={results.total}
                totalIsLowerBound={results.totalIsLowerBound}
              />
            </>
          )}
        </div>
      </div>
    </>
  );
}

function EmptyResults({ query, didYouMean }: { query: string; didYouMean: string | null }) {
  return (
    <div className="flex flex-col items-center py-16 text-center">
      <SearchX className="h-10 w-10 text-muted-foreground" aria-hidden="true" />
      <h2 className="mt-4 text-lg font-semibold">No results</h2>

      {didYouMean ? (
        <p className="mt-2 text-sm text-muted-foreground">
          Did you mean{' '}
          <Link
            href={`/search?q=${encodeURIComponent(didYouMean)}`}
            className="font-medium text-primary hover:underline"
          >
            {didYouMean}
          </Link>
          ?
        </p>
      ) : (
        <p className="mt-2 max-w-sm text-sm text-muted-foreground">
          {query
            ? 'Try fewer words, or check the spelling.'
            : 'Try removing a filter or two.'}
        </p>
      )}

      <Button variant="outline" className="mt-6" asChild>
        <Link href="/search">Browse everything</Link>
      </Button>
    </div>
  );
}

function ResultsSkeleton() {
  return (
    <div className="flex gap-8">
      <div className="hidden w-60 shrink-0 lg:block" />
      <div className="min-w-0 flex-1">
        <ProductGrid>
          {Array.from({ length: 12 }, (_, i) => (
            <ProductCardSkeleton key={i} />
          ))}
        </ProductGrid>
      </div>
    </div>
  );
}
