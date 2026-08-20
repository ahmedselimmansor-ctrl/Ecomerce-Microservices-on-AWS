import type { Redis } from 'ioredis';
import { Cart, CartLine, Money, zeroMoney, addMoney, multiplyMoney } from '@souq/contracts';
import type { PricingClient, PricedCart } from './pricing.js';
import type { CatalogClient } from './catalog.js';
import { logger } from './telemetry.js';

/**
 * Cart storage and pricing.
 *
 * Redis, not Postgres. A cart is high-write, low-value, reconstructible and
 * short-lived — every property that makes durable storage the wrong tool.
 * The trade is explicit: an ElastiCache failover loses in-flight carts, and
 * that is acceptable in a way losing an order never would be.
 *
 * Two things here are less obvious than they look:
 *
 *   1. Concurrency. A cart is mutated from a phone and a laptop at once more
 *      often than you would expect. Every mutation is a Lua script that
 *      compares a version and swaps, so "last write wins" cannot silently
 *      discard the other device's addition.
 *
 *   2. Pricing. pricing-engine is a synchronous dependency on the browsing
 *      path with a 250ms budget. When it is unavailable the cart falls back
 *      to list price and sets `pricingDegraded`. Selling at list price is a
 *      bad day; failing to show a cart at all is worse.
 */

const CART_TTL_SECONDS = 30 * 24 * 60 * 60; // 30 days for a signed-in cart
const ANON_CART_TTL_SECONDS = 7 * 24 * 60 * 60;
const MAX_LINES = 100;
const MAX_QTY_PER_LINE = 99;

const key = (cartId: string) => `cart:${cartId}`;
const userIndex = (userId: string) => `cart:user:${userId}`;

/**
 * Compare-and-swap in Lua so the read, the check and the write are one
 * round trip and one atomic operation at the server.
 *
 * Doing this in TypeScript — GET, compare, SET — has a window between the
 * read and the write in which the other device writes. WATCH/MULTI would also
 * work but costs two round trips and fails under Redis Cluster when the
 * client reconnects to a different node mid-transaction.
 *
 * Returns the new version, or -1 when the caller's version was stale.
 */
const CAS_SCRIPT = `
local current = redis.call('HGET', KEYS[1], 'version')
if current and tonumber(current) ~= tonumber(ARGV[1]) then
  return -1
end
local nextVersion = (tonumber(current) or 0) + 1
redis.call('HSET', KEYS[1], 'data', ARGV[2], 'version', nextVersion)
redis.call('EXPIRE', KEYS[1], ARGV[3])
return nextVersion
`;

export interface StoredCart {
  id: string;
  userId: string | null;
  lines: StoredLine[];
  currency: string;
  coupons: string[];
  countryCode: string;
  createdAt: string;
  updatedAt: string;
}

export interface StoredLine {
  sku: string;
  productId: string;
  quantity: number;
  /** The price the customer was shown when the line was added. */
  addedAtPrice: number;
  addedAt: string;
}

export class CartStale extends Error {
  constructor(readonly expected: number, readonly actual: number) {
    super(`cart changed since it was loaded (you sent version ${expected})`);
    this.name = 'CartStale';
  }
}

export class CartFull extends Error {
  constructor() {
    super(`a cart may hold at most ${MAX_LINES} distinct items`);
    this.name = 'CartFull';
  }
}

export class CartService {
  private casSha: string | null = null;

  constructor(
    private readonly redis: Redis,
    private readonly pricing: PricingClient,
    private readonly catalog: CatalogClient,
  ) {}

  /** Loads the CAS script once. Redis keeps it until a FLUSH or a restart. */
  private async cas(cartId: string, version: number, payload: string, ttl: number): Promise<number> {
    if (!this.casSha) {
      this.casSha = await this.redis.script('LOAD', CAS_SCRIPT) as string;
    }
    try {
      return await this.redis.evalsha(this.casSha, 1, key(cartId), String(version), payload, String(ttl)) as number;
    } catch (err) {
      // NOSCRIPT: Redis restarted or the script was evicted. Reload and retry
      // once rather than failing the customer's request.
      if (err instanceof Error && err.message.includes('NOSCRIPT')) {
        this.casSha = null;
        return this.cas(cartId, version, payload, ttl);
      }
      throw err;
    }
  }

  private async load(cartId: string): Promise<{ cart: StoredCart; version: number } | null> {
    const raw = await this.redis.hmget(key(cartId), 'data', 'version');
    if (!raw[0]) return null;
    return { cart: JSON.parse(raw[0]) as StoredCart, version: Number(raw[1] ?? 0) };
  }

  private ttlFor(cart: StoredCart): number {
    return cart.userId ? CART_TTL_SECONDS : ANON_CART_TTL_SECONDS;
  }

  // -------------------------------------------------------------------------

  async get(cartId: string): Promise<Cart | null> {
    const loaded = await this.load(cartId);
    if (!loaded) return null;
    return this.price(loaded.cart, loaded.version);
  }

  async create(userId: string | null, currency: string, countryCode: string): Promise<Cart> {
    const cartId = `crt_${ulid()}`;
    const now = new Date().toISOString();

    const cart: StoredCart = {
      id: cartId, userId, lines: [], currency, coupons: [],
      countryCode, createdAt: now, updatedAt: now,
    };

    await this.cas(cartId, 0, JSON.stringify(cart), this.ttlFor(cart));
    if (userId) {
      await this.redis.set(userIndex(userId), cartId, 'EX', CART_TTL_SECONDS);
    }
    return this.price(cart, 1);
  }

  async addLine(cartId: string, sku: string, quantity: number, expectedVersion: number): Promise<Cart> {
    const loaded = await this.load(cartId);
    if (!loaded) throw new Error('CART_NOT_FOUND');

    const { cart, version } = loaded;
    if (version !== expectedVersion) throw new CartStale(expectedVersion, version);

    // Resolve the SKU before mutating. A cart holding a product that does not
    // exist produces a checkout that fails at reservation time with a
    // confusing error, three services away from the cause.
    const product = await this.catalog.getVariant(sku);
    if (!product) throw new Error('PRODUCT_NOT_FOUND');
    if (product.status !== 'ACTIVE') throw new Error('PRODUCT_NOT_AVAILABLE');

    const existing = cart.lines.find((l) => l.sku === sku);
    if (existing) {
      existing.quantity = Math.min(existing.quantity + quantity, MAX_QTY_PER_LINE);
    } else {
      if (cart.lines.length >= MAX_LINES) throw new CartFull();
      cart.lines.push({
        sku,
        productId: product.productId,
        quantity: Math.min(quantity, MAX_QTY_PER_LINE),
        addedAtPrice: product.price.amount,
        addedAt: new Date().toISOString(),
      });
    }
    cart.updatedAt = new Date().toISOString();

    const next = await this.cas(cartId, version, JSON.stringify(cart), this.ttlFor(cart));
    if (next === -1) {
      // Somebody else wrote between our load and our write. Do NOT silently
      // retry: the customer's other device may have changed the same line,
      // and merging without asking is how a quantity of 1 becomes 3.
      const fresh = await this.load(cartId);
      throw new CartStale(version, fresh?.version ?? -1);
    }

    return this.price(cart, next);
  }

  async updateLine(cartId: string, sku: string, quantity: number, expectedVersion: number): Promise<Cart> {
    const loaded = await this.load(cartId);
    if (!loaded) throw new Error('CART_NOT_FOUND');

    const { cart, version } = loaded;
    if (version !== expectedVersion) throw new CartStale(expectedVersion, version);

    if (quantity === 0) {
      cart.lines = cart.lines.filter((l) => l.sku !== sku);
    } else {
      const line = cart.lines.find((l) => l.sku === sku);
      if (!line) throw new Error('LINE_NOT_FOUND');
      line.quantity = Math.min(quantity, MAX_QTY_PER_LINE);
    }
    cart.updatedAt = new Date().toISOString();

    const next = await this.cas(cartId, version, JSON.stringify(cart), this.ttlFor(cart));
    if (next === -1) {
      const fresh = await this.load(cartId);
      throw new CartStale(version, fresh?.version ?? -1);
    }
    return this.price(cart, next);
  }

  async applyCoupon(cartId: string, code: string, expectedVersion: number): Promise<Cart> {
    const loaded = await this.load(cartId);
    if (!loaded) throw new Error('CART_NOT_FOUND');

    const { cart, version } = loaded;
    if (version !== expectedVersion) throw new CartStale(expectedVersion, version);

    const normalised = code.trim().toUpperCase();
    if (!cart.coupons.includes(normalised)) cart.coupons.push(normalised);
    cart.updatedAt = new Date().toISOString();

    const next = await this.cas(cartId, version, JSON.stringify(cart), this.ttlFor(cart));
    if (next === -1) throw new CartStale(version, (await this.load(cartId))?.version ?? -1);

    // Whether the coupon actually applies is pricing-engine's call, not ours.
    // A rejected coupon comes back in `rejectedCoupons` with a reason the UI
    // can explain, rather than being silently dropped here.
    return this.price(cart, next);
  }

  /**
   * Merges an anonymous cart into a user's cart at login.
   *
   * Union, not replace. A customer who browsed anonymously and then signed in
   * expects both sets of items; picking one silently discards work they did.
   * On a quantity conflict the larger wins — over-showing is recoverable in
   * one click, under-showing is invisible.
   */
  async merge(anonCartId: string, userId: string): Promise<Cart> {
    const anon = await this.load(anonCartId);
    const existingId = await this.redis.get(userIndex(userId));
    const existing = existingId ? await this.load(existingId) : null;

    if (!anon && !existing) {
      return this.create(userId, 'EGP', 'EG');
    }
    if (!existing) {
      anon!.cart.userId = userId;
      const next = await this.cas(anon!.cart.id, anon!.version, JSON.stringify(anon!.cart), CART_TTL_SECONDS);
      await this.redis.set(userIndex(userId), anon!.cart.id, 'EX', CART_TTL_SECONDS);
      return this.price(anon!.cart, next);
    }
    if (!anon) {
      return this.price(existing.cart, existing.version);
    }

    const merged = existing.cart;
    for (const line of anon.cart.lines) {
      const match = merged.lines.find((l) => l.sku === line.sku);
      if (match) {
        match.quantity = Math.min(Math.max(match.quantity, line.quantity), MAX_QTY_PER_LINE);
      } else if (merged.lines.length < MAX_LINES) {
        merged.lines.push(line);
      }
    }
    merged.coupons = [...new Set([...merged.coupons, ...anon.cart.coupons])];
    merged.updatedAt = new Date().toISOString();

    const next = await this.cas(merged.id, existing.version, JSON.stringify(merged), CART_TTL_SECONDS);
    await this.redis.del(key(anonCartId));

    logger.info({ userId, mergedLines: merged.lines.length }, 'merged an anonymous cart on login');
    return this.price(merged, next);
  }

  // -------------------------------------------------------------------------

  /**
   * Prices a cart through pricing-engine, degrading to list price on failure.
   *
   * The catch block here is the whole reliability story for the browsing path.
   * pricing-engine is C++ behind a 250ms gRPC deadline; when it is
   * redeploying, overloaded, or simply slow, the customer still needs to see
   * their cart. They see it at list price with `pricingDegraded: true`, and
   * the storefront hides promotional messaging rather than showing prices it
   * cannot justify.
   */
  private async price(cart: StoredCart, version: number): Promise<Cart> {
    const lines = await this.hydrate(cart);

    let priced: PricedCart | null = null;
    try {
      priced = await this.pricing.calculate({
        lines: lines.map((l) => ({
          sku: l.sku, productId: l.productId, quantity: l.quantity,
          unitListPrice: l.unitPrice,
          categoryPath: l.categoryPath, brand: l.brand, attributes: l.attributes,
        })),
        context: {
          userId: cart.userId ?? undefined,
          currency: cart.currency,
          countryCode: cart.countryCode,
          couponCodes: cart.coupons,
          channel: 'web',
        },
      });
    } catch (err) {
      logger.warn(
        { cartId: cart.id, err: err instanceof Error ? err.message : String(err) },
        'pricing-engine unavailable; falling back to list price',
      );
    }

    if (!priced) {
      return this.degradedCart(cart, lines, version);
    }

    const cartLines: CartLine[] = lines.map((l, i) => {
      const p = priced!.lines[i];
      const stored = cart.lines[i]!;
      return {
        sku: l.sku,
        productId: l.productId,
        title: l.title,
        image: l.image,
        quantity: l.quantity,
        unitPrice: p?.unitEffectivePrice ?? l.unitPrice,
        lineTotal: p?.lineTotal ?? multiplyMoney(l.unitPrice, l.quantity),
        // Surfaced so the UI can say "this went up since you added it" rather
        // than quietly charging more than the customer remembers.
        priceChanged: (p?.unitEffectivePrice.amount ?? l.unitPrice.amount) !== stored.addedAtPrice,
      };
    });

    return {
      id: cart.id,
      userId: cart.userId,
      lines: cartLines,
      subtotal: priced.subtotal,
      discountTotal: priced.discountTotal,
      shippingTotal: priced.shippingTotal,
      taxTotal: priced.taxTotal,
      total: priced.grandTotal,
      appliedCoupons: priced.appliedPromotions
        .filter((p) => p.couponCode)
        .map((p) => ({ code: p.couponCode!, discount: p.discount, name: p.name })),
      rejectedCoupons: priced.rejectedPromotions.map((r) => ({
        code: r.couponCode, reasonCode: r.reasonCode,
      })),
      currency: cart.currency,
      pricingDegraded: priced.degraded,
      rulesVersion: priced.rulesVersion,
      version,
      expiresAt: new Date(Date.now() + this.ttlFor(cart) * 1000).toISOString(),
      updatedAt: cart.updatedAt,
    };
  }

  /**
   * The fallback. List prices, no promotions, no tax lines.
   *
   * `rulesVersion` is null deliberately: order-service refuses to accept an
   * order without one, so a degraded cart cannot be checked out. That is the
   * correct trade — showing a cart we cannot price is fine, taking money for
   * one is not.
   */
  private degradedCart(cart: StoredCart, lines: HydratedLine[], version: number): Cart {
    const currency = cart.currency;
    let subtotal = zeroMoney(currency);

    const cartLines: CartLine[] = lines.map((l) => {
      const lineTotal = multiplyMoney(l.unitPrice, l.quantity);
      subtotal = addMoney(subtotal, lineTotal);
      return {
        sku: l.sku, productId: l.productId, title: l.title, image: l.image,
        quantity: l.quantity, unitPrice: l.unitPrice, lineTotal,
        priceChanged: false,
      };
    });

    return {
      id: cart.id,
      userId: cart.userId,
      lines: cartLines,
      subtotal,
      discountTotal: zeroMoney(currency),
      shippingTotal: zeroMoney(currency),
      taxTotal: zeroMoney(currency),
      total: subtotal,
      appliedCoupons: [],
      rejectedCoupons: [],
      currency,
      pricingDegraded: true,
      rulesVersion: null,
      version,
      expiresAt: new Date(Date.now() + this.ttlFor(cart) * 1000).toISOString(),
      updatedAt: cart.updatedAt,
    };
  }

  /** Fetches product detail for every line in ONE catalog call. */
  private async hydrate(cart: StoredCart): Promise<HydratedLine[]> {
    if (cart.lines.length === 0) return [];

    const variants = await this.catalog.getVariants(cart.lines.map((l) => l.sku));
    const bySku = new Map(variants.map((v) => [v.sku, v]));

    return cart.lines.flatMap((line) => {
      const v = bySku.get(line.sku);
      if (!v) {
        // The product was deleted while it sat in a cart. Drop the line rather
        // than rendering a broken row or failing the whole cart.
        logger.info({ cartId: cart.id, sku: line.sku }, 'dropping a cart line for a product that no longer exists');
        return [];
      }
      return [{
        sku: line.sku,
        productId: v.productId,
        title: v.title,
        image: v.image,
        quantity: line.quantity,
        unitPrice: v.price,
        categoryPath: v.categoryPath,
        brand: v.brand,
        attributes: v.attributes,
      }];
    });
  }
}

interface HydratedLine {
  sku: string;
  productId: string;
  title: string;
  image: string | null;
  quantity: number;
  unitPrice: Money;
  categoryPath: string[];
  brand: string | null;
  attributes: Record<string, string>;
}

// Crockford base32 ULID, matching the ids every other service mints.
const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
function ulid(): string {
  let ts = Date.now();
  const time = Array.from({ length: 10 }, () => {
    const c = CROCKFORD[ts % 32]!;
    ts = Math.floor(ts / 32);
    return c;
  }).reverse().join('');

  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  const rand = Array.from(bytes.slice(0, 16), (b) => CROCKFORD[b % 32]!).join('');

  return time + rand;
}
