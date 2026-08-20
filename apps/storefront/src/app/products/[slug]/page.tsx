import { Suspense } from 'react';
import Image from 'next/image';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ChevronRight, Truck, Undo2 } from 'lucide-react';

import { Product, RecommendationResponse, ReviewPage, formatMoney } from '@souq/contracts';

import { BffError, call, gather } from '@/lib/bff';
import { Badge } from '@/components/ui/badge';
import { Separator } from '@/components/ui/separator';
import { Skeleton } from '@/components/ui/skeleton';
import { AddToCart } from '@/components/cart/add-to-cart';
import { Price } from '@/components/catalog/price';
import { ProductCard, ProductGrid, fromProduct } from '@/components/catalog/product-card';
import { Rating } from '@/components/catalog/rating';
import { StockIndicator } from '@/components/catalog/stock-indicator';
import { VariantPicker } from '@/components/catalog/variant-picker';
import { ReviewList } from '@/components/catalog/review-list';

type Params = Promise<{ slug: string }>;

/**
 * The product detail page.
 *
 * Statically rendered with a one-minute revalidate. A price change has to reach
 * shoppers quickly — showing a stale price is a legal problem in most markets,
 * not just a bad experience — but one minute of CDN caching removes essentially
 * all of the origin load from the highest-traffic page on the site.
 */
export const revalidate = 60;

async function loadProduct(slug: string): Promise<Product> {
  try {
    return await call({
      service: 'catalog',
      path: `/v1/products/${encodeURIComponent(slug)}`,
      schema: Product,
      revalidate: 60,
      tags: [`product:${slug}`],
    });
  } catch (error) {
    // 404 becomes Next's not-found page. Anything else is a real fault and
    // should reach the error boundary rather than being disguised as "no such
    // product" — a catalog outage that renders as 404 gets deindexed.
    if (error instanceof BffError && error.status === 404) notFound();
    throw error;
  }
}

export async function generateMetadata({ params }: { params: Params }) {
  const { slug } = await params;

  try {
    const product = await loadProduct(slug);
    const primary = product.variants.find((v) => v.available !== 0) ?? product.variants[0];

    return {
      title: product.title,
      description: product.description.slice(0, 160),
      alternates: { canonical: `/products/${product.slug}` },
      // A draft or archived product must not be indexed. It is unreachable from
      // navigation, but a crawler that saw it once keeps returning.
      robots: product.status === 'ACTIVE'
        ? { index: true, follow: true }
        : { index: false, follow: false },
      openGraph: {
        title: product.title,
        description: product.description.slice(0, 200),
        type: 'website',
        images: product.images[0] ? [{ url: product.images[0].url }] : [],
      },
      other: primary
        ? { 'product:price:amount': String(primary.price.amount / 100),
            'product:price:currency': primary.price.currency }
        : {},
    };
  } catch {
    return { title: 'Product' };
  }
}

export default async function ProductPage({ params }: { params: Params }) {
  const { slug } = await params;
  const product = await loadProduct(slug);

  const primary = product.variants.length > 0
    ? product.variants.reduce((a, b) => (b.price.amount < a.price.amount ? b : a))
    : null;

  const image = product.images[0] ?? primary?.images[0] ?? null;

  return (
    <div className="container py-8">
      <Breadcrumbs path={product.categoryPath} title={product.title} />

      <div className="mt-6 grid gap-8 lg:grid-cols-2">
        <Gallery images={product.images.length > 0 ? product.images : (primary?.images ?? [])}
                 title={product.title} />

        <div className="space-y-5">
          {product.brand && (
            <p className="text-sm uppercase tracking-wide text-muted-foreground">{product.brand}</p>
          )}

          <h1 className="text-2xl font-bold tracking-tight sm:text-3xl">{product.title}</h1>

          {product.rating && (
            <Rating value={product.rating.average} count={product.rating.count} />
          )}

          {primary && <Price price={primary.price} listPrice={primary.listPrice} size="lg" />}

          {primary && <StockIndicator available={primary.available} />}

          {/*
            The variant picker is a client component and owns which SKU is
            selected, so Add to Cart lives inside it. Hoisting the selection
            into this server component would need a round trip per colour change.
          */}
          {product.variants.length > 1 ? (
            <VariantPicker variants={product.variants} />
          ) : (
            primary && <AddToCart sku={primary.sku} available={primary.available} />
          )}

          <Separator />

          <ul className="space-y-2 text-sm text-muted-foreground">
            <li className="flex items-center gap-2">
              <Truck className="h-4 w-4 shrink-0" aria-hidden="true" />
              Free delivery on orders over {formatMoney({ amount: 50000, currency: primary?.price.currency ?? 'EGP' })}
            </li>
            <li className="flex items-center gap-2">
              <Undo2 className="h-4 w-4 shrink-0" aria-hidden="true" />
              Free returns within 30 days
            </li>
          </ul>

          {product.description && (
            <div className="prose-sm max-w-none">
              <h2 className="text-sm font-semibold">Description</h2>
              <p className="mt-1 whitespace-pre-line text-sm text-muted-foreground">
                {product.description}
              </p>
            </div>
          )}

          {Object.keys(product.attributes).length > 0 && (
            <div>
              <h2 className="text-sm font-semibold">Specifications</h2>
              <dl className="mt-2 divide-y rounded-md border text-sm">
                {Object.entries(product.attributes).map(([key, value]) => (
                  <div key={key} className="flex justify-between gap-4 px-3 py-2">
                    <dt className="capitalize text-muted-foreground">{key.replace(/_/g, ' ')}</dt>
                    <dd className="text-right font-medium">{value}</dd>
                  </div>
                ))}
              </dl>
            </div>
          )}
        </div>
      </div>

      {/* Both stream independently; neither can delay the buy box above. */}
      <Suspense fallback={<SectionSkeleton title="Customer reviews" />}>
        <Reviews productId={product.id} />
      </Suspense>

      <Suspense fallback={<SectionSkeleton title="You might also like" />}>
        <SimilarProducts productId={product.id} />
      </Suspense>

      {/*
        Product structured data. JSON-LD rather than microdata so the markup
        stays out of the render tree. `availability` is derived from the same
        eventually-consistent field the UI uses, so it can be briefly wrong —
        which is why it is not the thing that gates a purchase.
      */}
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{
          __html: JSON.stringify({
            '@context': 'https://schema.org',
            '@type': 'Product',
            name: product.title,
            description: product.description,
            sku: primary?.sku,
            brand: product.brand ? { '@type': 'Brand', name: product.brand } : undefined,
            image: image ? [image.url] : undefined,
            aggregateRating: product.rating && product.rating.count > 0
              ? {
                  '@type': 'AggregateRating',
                  ratingValue: product.rating.average,
                  reviewCount: product.rating.count,
                }
              : undefined,
            offers: primary
              ? {
                  '@type': 'Offer',
                  price: (primary.price.amount / 100).toFixed(2),
                  priceCurrency: primary.price.currency,
                  availability: primary.available === 0
                    ? 'https://schema.org/OutOfStock'
                    : 'https://schema.org/InStock',
                }
              : undefined,
          }),
        }}
      />
    </div>
  );
}

function Breadcrumbs({ path, title }: { path: string[]; title: string }) {
  return (
    <nav aria-label="Breadcrumb">
      <ol className="flex flex-wrap items-center gap-1 text-sm text-muted-foreground">
        <li>
          <Link href="/" className="hover:text-foreground">Home</Link>
        </li>
        {path.map((segment, index) => (
          <li key={segment} className="flex items-center gap-1">
            <ChevronRight className="h-3.5 w-3.5" aria-hidden="true" />
            <Link href={`/search?category=${segment}`} className="capitalize hover:text-foreground">
              {segment.replace(/-/g, ' ')}
            </Link>
            {index === path.length - 1 && <span className="sr-only">, current section</span>}
          </li>
        ))}
        <li className="flex items-center gap-1">
          <ChevronRight className="h-3.5 w-3.5" aria-hidden="true" />
          <span aria-current="page" className="truncate text-foreground">{title}</span>
        </li>
      </ol>
    </nav>
  );
}

function Gallery({ images, title }: { images: { url: string; alt: string }[]; title: string }) {
  const first = images[0];

  return (
    <div className="space-y-3">
      <div className="relative aspect-square overflow-hidden rounded-lg bg-muted">
        {first ? (
          <Image
            src={first.url}
            // The product's own alt when the catalogue provides one, the title
            // otherwise. Never "product image" — the screen reader already
            // announces that it is an image.
            alt={first.alt || title}
            fill
            sizes="(min-width: 1024px) 45vw, 100vw"
            // The largest element on the page and almost always the LCP one.
            priority
            className="object-cover"
          />
        ) : (
          <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
            No image available
          </div>
        )}
      </div>

      {images.length > 1 && (
        <ul className="grid grid-cols-5 gap-2">
          {images.slice(0, 5).map((image, index) => (
            <li key={image.url} className="relative aspect-square overflow-hidden rounded-md bg-muted">
              <Image
                src={image.url}
                alt={image.alt || `${title} — view ${index + 1}`}
                fill
                sizes="15vw"
                className="object-cover"
              />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

async function Reviews({ productId }: { productId: string }) {
  const { reviews } = await gather([], {
    reviews: call({
      service: 'review',
      path: `/v1/products/${encodeURIComponent(productId)}/reviews?limit=5`,
      schema: ReviewPage,
      revalidate: 120,
    }),
  });

  if (!reviews || reviews.items.length === 0) return null;

  return (
    <section className="mt-12">
      <h2 className="text-xl font-semibold tracking-tight">Customer reviews</h2>
      <div className="mt-4">
        <ReviewList reviews={reviews.items} />
      </div>
    </section>
  );
}

async function SimilarProducts({ productId }: { productId: string }) {
  const { recommendations } = await gather([], {
    recommendations: call({
      service: 'recommendation',
      path: `/v1/recommendations?placement=pdp_similar&itemId=${encodeURIComponent(productId)}&count=5`,
      schema: RecommendationResponse,
      revalidate: 300,
    }),
  });

  const ids = (recommendations?.items ?? []).map((item) => item.productId);
  if (ids.length === 0) return null;

  const { products } = await gather([], {
    products: call({
      service: 'catalog',
      path: `/v1/products/batch?ids=${ids.map(encodeURIComponent).join(',')}`,
      schema: Product.array(),
      revalidate: 300,
    }),
  });

  if (!products || products.length === 0) return null;

  const byId = new Map(products.map((p) => [p.id, p]));
  const ordered = ids.map((id) => byId.get(id)).filter((p): p is Product => p !== undefined);

  if (ordered.length === 0) return null;

  return (
    <section className="mt-12">
      <h2 className="text-xl font-semibold tracking-tight">You might also like</h2>
      <div className="mt-4">
        <ProductGrid>
          {ordered.map((product) => (
            <ProductCard key={product.id} item={fromProduct(product)} />
          ))}
        </ProductGrid>
      </div>
    </section>
  );
}

function SectionSkeleton({ title }: { title: string }) {
  return (
    <section className="mt-12">
      <h2 className="text-xl font-semibold tracking-tight">{title}</h2>
      <div className="mt-4 space-y-3">
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-24 w-full" />
      </div>
    </section>
  );
}
