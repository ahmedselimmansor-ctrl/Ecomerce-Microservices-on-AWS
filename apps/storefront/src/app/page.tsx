import Link from 'next/link';
import { Suspense } from 'react';

import { Category, Product, RecommendationResponse, SearchResponse } from '@souq/contracts';

import { call, gather } from '@/lib/bff';
import { Button } from '@/components/ui/button';
import {
  ProductCard, ProductCardSkeleton, ProductGrid, fromProduct, fromSearchHit,
} from '@/components/catalog/product-card';

export const metadata = {
  title: 'SOUQ — a distributed commerce platform',
};

/**
 * The home page.
 *
 * Revalidated rather than rendered per request. Everything on it is the same
 * for every signed-out visitor, and personalised strips are fetched separately
 * so they do not force the whole page to be dynamic.
 */
export const revalidate = 120;

export default function HomePage() {
  return (
    <div className="container space-y-12 py-8">
      <section className="rounded-lg bg-secondary px-6 py-12 text-center sm:py-16">
        <h1 className="text-3xl font-bold tracking-tight sm:text-4xl">Everything, delivered</h1>
        <p className="mx-auto mt-3 max-w-xl text-muted-foreground">
          Eleven services, five languages, and a checkout whose correctness was established by
          exhaustive state-space search before any of it was written.
        </p>
        <Button size="lg" className="mt-6" asChild>
          <Link href="/search">Start browsing</Link>
        </Button>
      </section>

      {/*
        Each strip streams independently. A slow recommendation call delays its
        own section, not the hero above it — which is the whole reason these are
        separate Suspense boundaries rather than one await at the top.
      */}
      <Suspense fallback={<StripSkeleton title="Recommended for you" />}>
        <RecommendedStrip />
      </Suspense>

      <Suspense fallback={<StripSkeleton title="New arrivals" />}>
        <NewArrivalsStrip />
      </Suspense>

      <Suspense fallback={null}>
        <CategoryTiles />
      </Suspense>
    </div>
  );
}

/**
 * Personalised recommendations.
 *
 * Two hops, and the second is not optional. recommendation-service returns
 * ranked **product ids and nothing else** — no title, no price. That is the
 * right split: Personalize ranks, catalog-service owns the data, and a
 * recommender that cached product copy would serve stale prices the moment one
 * changed. So the ids are hydrated through catalog's batch endpoint, which
 * exists precisely to avoid ten sequential fetches here.
 *
 * `fallback` is true when Personalize was cold or unavailable and the service
 * ranked by popularity instead. The heading changes accordingly: the items are
 * still worth showing, but calling them "recommended for you" when they are the
 * same list everyone sees is a claim we cannot support.
 *
 * Any failure renders nothing. This strip is the definition of optional.
 */
async function RecommendedStrip() {
  const { recommendations } = await gather([], {
    recommendations: call({
      service: 'recommendation',
      path: '/v1/recommendations?placement=home_for_you&count=10',
      schema: RecommendationResponse,
      revalidate: 60,
    }),
  });

  if (!recommendations || recommendations.items.length === 0) return null;

  const ids = recommendations.items.map((item) => item.productId);

  const { products } = await gather([], {
    products: call({
      service: 'catalog',
      path: `/v1/products/batch?ids=${ids.map(encodeURIComponent).join(',')}`,
      schema: Product.array(),
      revalidate: 60,
    }),
  });

  if (!products || products.length === 0) return null;

  // Restore the recommender's ordering. The batch endpoint returns rows in
  // whatever order the query produced, and a ranked list rendered in arbitrary
  // order is not a ranked list.
  const byId = new Map(products.map((product) => [product.id, product]));
  const ordered = ids.map((id) => byId.get(id)).filter((p): p is Product => p !== undefined);

  if (ordered.length === 0) return null;

  return (
    <Strip
      title={recommendations.fallback ? 'Popular right now' : 'Recommended for you'}
      href="/search"
    >
      {ordered.map((product, index) => (
        <ProductCard key={product.id} item={fromProduct(product)} priority={index < 5} />
      ))}
    </Strip>
  );
}

async function NewArrivalsStrip() {
  const { results } = await gather([], {
    results: call({
      service: 'search',
      path: '/v1/search?q=&sort=newest&size=10',
      schema: SearchResponse,
      revalidate: 300,
    }),
  });

  if (!results || results.hits.length === 0) return null;

  return (
    <Strip title="New arrivals" href="/search?sort=newest">
      {results.hits.map((hit) => (
        <ProductCard key={hit.productId} item={fromSearchHit(hit)} />
      ))}
    </Strip>
  );
}

async function CategoryTiles() {
  const { categories } = await gather([], {
    categories: call({
      service: 'catalog',
      path: '/v1/categories',
      schema: Category.array(),
      revalidate: 600,
      tags: ['categories'],
    }),
  });

  const roots = (categories ?? []).filter((c) => c.path.length === 1 && c.productCount > 0);
  if (roots.length === 0) return null;

  return (
    <section>
      <h2 className="text-xl font-semibold tracking-tight">Shop by category</h2>
      <ul className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        {roots.map((category) => (
          <li key={category.id}>
            <Link
              href={`/search?category=${category.slug}`}
              className="flex h-full flex-col justify-between rounded-lg border p-4 transition-colors hover:bg-accent"
            >
              <span className="text-sm font-medium">{category.name}</span>
              <span className="tabular mt-2 text-xs text-muted-foreground">
                {category.productCount.toLocaleString()} items
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}

function Strip({
  title,
  href,
  children,
}: {
  title: string;
  href: string;
  children: React.ReactNode;
}) {
  return (
    <section>
      <div className="flex items-baseline justify-between">
        <h2 className="text-xl font-semibold tracking-tight">{title}</h2>
        <Link href={href} className="text-sm text-muted-foreground hover:text-foreground">
          See all
        </Link>
      </div>
      <div className="mt-4">
        <ProductGrid>{children}</ProductGrid>
      </div>
    </section>
  );
}

function StripSkeleton({ title }: { title: string }) {
  return (
    <section>
      <h2 className="text-xl font-semibold tracking-tight">{title}</h2>
      <div className="mt-4">
        <ProductGrid>
          {Array.from({ length: 5 }, (_, i) => (
            <ProductCardSkeleton key={i} />
          ))}
        </ProductGrid>
      </div>
    </section>
  );
}
