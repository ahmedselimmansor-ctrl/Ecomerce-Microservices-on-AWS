import Image from 'next/image';
import Link from 'next/link';

import type { Product, SearchHit } from '@souq/contracts';

import { cn } from '@/lib/utils';
import { Card } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { Price } from './price';
import { Rating } from './rating';

/**
 * What a card needs, independent of where it came from.
 *
 * search-service returns `SearchHit` and catalog-service returns `Product`, and
 * the two disagree on almost every field name — `image` versus `images[0]`,
 * `rating` versus `rating.average`, `inStock` versus a nullable per-variant
 * `available`. Normalising at the boundary keeps that mismatch in two adapters
 * instead of in every component that renders a product.
 */
export interface ProductCardItem {
  productId: string;
  title: string;
  slug: string;
  brand: string | null;
  image: string | null;
  price: { amount: number; currency: string };
  listPrice: { amount: number; currency: string } | null;
  rating: number | null;
  ratingCount: number;
  /**
   * Whether to dim the card and show the overlay. Derived from an eventually
   * consistent field, so it is a display hint — never the gate on purchase.
   */
  inStock: boolean;
}

export function fromSearchHit(hit: SearchHit): ProductCardItem {
  return {
    productId: hit.productId,
    title: hit.title,
    slug: hit.slug,
    brand: hit.brand,
    image: hit.image,
    price: hit.price,
    listPrice: hit.listPrice,
    rating: hit.rating,
    ratingCount: hit.ratingCount,
    inStock: hit.inStock,
  };
}

export function fromProduct(product: Product): ProductCardItem {
  // The cheapest active variant, matching what the PDP leads with. Showing a
  // grid price of one variant and opening on another is the most common
  // complaint about faceted catalogues.
  const cheapest = product.variants.length > 0
    ? product.variants.reduce((a, b) => (b.price.amount < a.price.amount ? b : a))
    : null;

  return {
    productId: product.id,
    title: product.title,
    slug: product.slug,
    brand: product.brand,
    image: product.images[0]?.url ?? cheapest?.images[0]?.url ?? null,
    price: cheapest?.price ?? product.price,
    listPrice: cheapest?.listPrice ?? product.listPrice,
    rating: product.rating?.average ?? null,
    ratingCount: product.rating?.count ?? 0,
    // `available === null` means inventory has not reported yet, which is NOT
    // out of stock. Treating unknown as in-stock keeps the card undimmed and
    // lets the reservation be the authority.
    inStock: product.variants.length === 0
      ? true
      : product.variants.some((v) => v.available === null || v.available > 0),
  };
}

/**
 * One product in a grid.
 *
 * The whole card is a link, via a stretched pseudo-element on the title rather
 * than by wrapping everything in an `<a>`. Wrapping produces a single enormous
 * link whose accessible name is every word in the card — "Sony WH-1000XM5 4.6
 * out of 5 1,204 reviews EGP 12,999 was EGP 14,999 −13% In stock" — read out in
 * full on every arrow-key press through a listing.
 *
 * With this arrangement the link's name is just the product title, and the rest
 * of the card is still clickable.
 */
export function ProductCard({
  item,
  priority = false,
  locale = 'en-GB',
  className,
}: {
  item: ProductCardItem;
  /**
   * Set on the first few cards above the fold. Next lazy-loads images by
   * default, and lazy-loading the largest visible element is a direct hit to
   * Largest Contentful Paint — the metric this grid is judged on.
   */
  priority?: boolean;
  locale?: string;
  className?: string;
}) {
  return (
    <Card
      className={cn(
        'group relative flex flex-col overflow-hidden transition-shadow hover:shadow-md',
        !item.inStock && 'opacity-75',
        className,
      )}
    >
      <div className="relative aspect-square overflow-hidden bg-muted">
        {item.image ? (
          <Image
            src={item.image}
            // The alt is the title, not "product image". A screen reader
            // already announces that it is an image; repeating it wastes the
            // only words the user gets.
            alt={item.title}
            fill
            // Tells Next which resolution to actually serve per breakpoint.
            // Without it every card downloads a full-width image, which on a
            // four-column grid is roughly four times the bytes needed.
            sizes="(min-width: 1280px) 20vw, (min-width: 768px) 33vw, 50vw"
            priority={priority}
            className="object-cover transition-transform duration-300 group-hover:scale-105"
          />
        ) : (
          <div className="flex h-full items-center justify-center text-xs text-muted-foreground">
            No image
          </div>
        )}

        {!item.inStock && (
          <div className="absolute inset-x-0 bottom-0 bg-background/90 py-1.5 text-center text-xs font-medium">
            Out of stock
          </div>
        )}
      </div>

      <div className="flex flex-1 flex-col gap-2 p-4">
        {item.brand && (
          <p className="text-xs uppercase tracking-wide text-muted-foreground">{item.brand}</p>
        )}

        <h3 className="text-sm font-medium leading-snug">
          <Link
            href={`/products/${item.slug}`}
            // The stretched pseudo-element. `after:absolute after:inset-0`
            // makes the card's whole area part of this one link.
            className="line-clamp-2 after:absolute after:inset-0 hover:underline"
          >
            {item.title}
          </Link>
        </h3>

        <Rating value={item.rating} count={item.ratingCount} size="sm" />

        <div className="mt-auto pt-1">
          <Price price={item.price} listPrice={item.listPrice} locale={locale} size="default" />
        </div>
      </div>
    </Card>
  );
}

/**
 * The loading placeholder.
 *
 * Sized to match the real card — same aspect-square image, same two text lines,
 * same price row. A skeleton of a different height produces a layout shift at
 * the exact moment the user starts reading, which counts against Cumulative
 * Layout Shift and, more to the point, is infuriating.
 */
export function ProductCardSkeleton() {
  return (
    <Card className="flex flex-col overflow-hidden">
      <Skeleton className="aspect-square rounded-none" />
      <div className="flex flex-col gap-2 p-4">
        <Skeleton className="h-3 w-16" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-2/3" />
        <Skeleton className="h-3 w-24" />
        <Skeleton className="mt-1 h-6 w-28" />
      </div>
    </Card>
  );
}

export function ProductGrid({ children }: { children: React.ReactNode }) {
  return (
    <div className="grid grid-cols-2 gap-4 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5">
      {children}
    </div>
  );
}
