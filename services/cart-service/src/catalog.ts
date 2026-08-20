import type { Money } from '@souq/contracts';
import { logger } from './telemetry.js';

/**
 * Read-through client for catalog-service.
 *
 * Product data changes rarely and is read constantly, so it is cached in
 * process for 60 seconds. That is deliberately short: a price change must
 * reach a customer's cart quickly, and pricing-engine re-prices on every
 * cart read anyway, so the cache only serves titles and images.
 */

export interface Variant {
  sku: string;
  productId: string;
  title: string;
  image: string | null;
  price: Money;
  status: string;
  categoryPath: string[];
  brand: string | null;
  attributes: Record<string, string>;
}

const TTL_MS = 60_000;

export class CatalogClient {
  private cache = new Map<string, { v: Variant; at: number }>();

  constructor(private readonly baseUrl: string) {}

  async getVariant(sku: string): Promise<Variant | null> {
    const hit = this.cache.get(sku);
    if (hit && Date.now() - hit.at < TTL_MS) return hit.v;

    const found = await this.getVariants([sku]);
    return found[0] ?? null;
  }

  /**
   * One request for every SKU in a cart.
   *
   * A cart with 20 lines issuing 20 sequential lookups is how a cart page ends
   * up slower than checkout — and it puts 20x the load on catalog-service for
   * no reason.
   */
  async getVariants(skus: string[]): Promise<Variant[]> {
    const now = Date.now();
    const out: Variant[] = [];
    const missing: string[] = [];

    for (const sku of skus) {
      const hit = this.cache.get(sku);
      if (hit && now - hit.at < TTL_MS) out.push(hit.v);
      else missing.push(sku);
    }
    if (missing.length === 0) return out;

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 3_000);

    try {
      const res = await fetch(
        `${this.baseUrl}/v1/variants?skus=${encodeURIComponent(missing.join(','))}`,
        { signal: controller.signal, headers: { Accept: 'application/json' } },
      );
      if (!res.ok) throw new Error(`catalog-service returned ${res.status}`);

      const body = await res.json() as { items: Variant[] };
      for (const v of body.items) {
        this.cache.set(v.sku, { v, at: now });
        out.push(v);
      }
    } catch (err) {
      // Serve stale rather than failing the cart. A slightly out-of-date
      // product title is not worth a 500 on the cart page.
      logger.warn(
        { err: err instanceof Error ? err.message : String(err), skus: missing.length },
        'catalog-service unavailable; serving stale entries where possible',
      );
      for (const sku of missing) {
        const stale = this.cache.get(sku);
        if (stale) out.push(stale.v);
      }
    } finally {
      clearTimeout(timer);
    }

    return out;
  }

  /** Bounded so a long-running pod cannot grow the cache without limit. */
  prune(): void {
    const cutoff = Date.now() - TTL_MS * 10;
    for (const [sku, entry] of this.cache) {
      if (entry.at < cutoff) this.cache.delete(sku);
    }
  }
}
