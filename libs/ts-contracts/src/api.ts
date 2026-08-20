import { z } from 'zod';
import {
  Money, Address, Image, Timestamp, paginated,
  UserId, ProductId, Sku, OrderId, CartId, ReviewId, PaymentId, ReservationId,
} from './primitives.js';
import { ProductStatus, CancellationReason } from './events.js';

/**
 * HTTP request and response shapes for every endpoint the frontends touch.
 *
 * The storefront parses every BFF response through the matching schema here
 * before it reaches a component (docs/CONTRACTS.md §8). That is not paranoia:
 * these responses cross a service boundary written in five different
 * languages, and a service that starts emitting `available: null` should
 * produce one clean 502 with a requestId, not `undefined is not a number`
 * inside a memoised selector.
 */

// ===========================================================================
// identity-service
// ===========================================================================

export const Role = z.enum(['CUSTOMER', 'MERCHANT', 'SUPPORT', 'OPS', 'ADMIN']);
export type Role = z.infer<typeof Role>;

export const RegisterRequest = z.object({
  email:    z.string().email().max(320),
  // Length beats composition rules. NIST SP 800-63B has said so since 2017,
  // and composition rules mostly produce Passw0rd! — the server additionally
  // rejects known-breached passwords via a k-anonymity range query.
  password: z.string().min(12).max(256),
  fullName: z.string().min(1).max(200),
  locale:   z.string().default('en-GB'),
  acceptedTermsVersion: z.string(),
}).strict();

export const LoginRequest = z.object({
  email:    z.string().email(),
  password: z.string().min(1).max(256),
  mfaCode:  z.string().regex(/^\d{6}$/).optional(),
}).strict();

export const TokenPair = z.object({
  accessToken:  z.string(),
  /** Seconds. 900 — see docs/CONTRACTS.md §7. */
  expiresIn:    z.number().int().positive(),
  tokenType:    z.literal('Bearer'),
  /**
   * Absent in browser flows: the refresh token is set as an HttpOnly,
   * SameSite=Strict, Secure cookie by the BFF and never touches JavaScript.
   * Present only for native app clients that have no cookie jar.
   */
  refreshToken: z.string().optional(),
}).strict();

export const UserProfile = z.object({
  id:            UserId,
  email:         z.string().email(),
  fullName:      z.string(),
  locale:        z.string(),
  roles:         z.array(Role),
  mfaEnabled:    z.boolean(),
  emailVerified: z.boolean(),
  createdAt:     Timestamp,
}).strict();
export type UserProfile = z.infer<typeof UserProfile>;

export const LoginResponse = z.object({
  tokens: TokenPair,
  user:   UserProfile,
}).strict();

// ===========================================================================
// catalog-service
// ===========================================================================

export const ProductVariant = z.object({
  sku:        Sku,
  attributes: z.record(z.string()),
  price:      Money,
  listPrice:  Money.optional(),
  /**
   * Denormalised from inventory-service and therefore EVENTUALLY CONSISTENT.
   * Treat it as a display hint only. The authoritative check happens when the
   * saga reserves stock — never gate the Add to Cart button on this alone or
   * you will happily accept an order you cannot fulfil.
   */
  available:  z.number().int().min(0).nullable(),
  images:     z.array(Image).default([]),
}).strict();
export type ProductVariant = z.infer<typeof ProductVariant>;

export const Product = z.object({
  id:           ProductId,
  sku:          Sku,
  title:        z.string(),
  slug:         z.string(),
  description:  z.string().default(''),
  brand:        z.string().nullable(),
  categoryPath: z.array(z.string()),
  attributes:   z.record(z.string()),
  images:       z.array(Image),
  price:        Money,
  listPrice:    Money.nullable(),
  status:       ProductStatus,
  variants:     z.array(ProductVariant).default([]),
  rating: z.object({
    average: z.number().min(0).max(5),
    count:   z.number().int().min(0),
  }).nullable(),
  createdAt: Timestamp,
  updatedAt: Timestamp,
}).strict();
export type Product = z.infer<typeof Product>;

export const ProductPage = paginated(Product);
export type ProductPage = z.infer<typeof ProductPage>;

export const CreateProductRequest = Product
  .omit({ id: true, createdAt: true, updatedAt: true, rating: true, slug: true })
  .partial({ variants: true, attributes: true, images: true, listPrice: true })
  .strict();

export const Category = z.object({
  id:       z.string(),
  slug:     z.string(),
  name:     z.string(),
  path:     z.array(z.string()),
  parentId: z.string().nullable(),
  productCount: z.number().int().min(0),
}).strict();

// ===========================================================================
// cart-service
// ===========================================================================

export const CartLine = z.object({
  sku:       Sku,
  productId: ProductId,
  title:     z.string(),
  image:     z.string().url().nullable(),
  quantity:  z.number().int().min(1).max(99),
  unitPrice: Money,
  lineTotal: Money,
  /** Set when the price moved since the line was added; the UI must surface it. */
  priceChanged: z.boolean().default(false),
}).strict();
export type CartLine = z.infer<typeof CartLine>;

export const Cart = z.object({
  id:     CartId,
  userId: UserId.nullable(),
  lines:  z.array(CartLine),
  subtotal:      Money,
  discountTotal: Money,
  shippingTotal: Money,
  taxTotal:      Money,
  total:         Money,
  appliedCoupons: z.array(z.object({
    code:     z.string(),
    discount: Money,
    name:     z.string(),
  }).strict()).default([]),
  rejectedCoupons: z.array(z.object({
    code:       z.string(),
    reasonCode: z.string(),
  }).strict()).default([]),
  currency: z.string(),
  /**
   * True when pricing-engine was unreachable and we fell back to list price.
   * The cart is still chargeable at these numbers; promo messaging is hidden.
   */
  pricingDegraded: z.boolean().default(false),
  rulesVersion: z.string().nullable(),
  /** Optimistic-concurrency token. Send it back on mutations or get CART_STALE. */
  version:   z.number().int().min(0),
  expiresAt: Timestamp,
  updatedAt: Timestamp,
}).strict();
export type Cart = z.infer<typeof Cart>;

export const AddToCartRequest = z.object({
  sku:      Sku,
  quantity: z.number().int().min(1).max(99),
}).strict();

export const UpdateCartLineRequest = z.object({
  quantity: z.number().int().min(0).max(99),  // 0 removes the line
  version:  z.number().int().min(0),
}).strict();

export const ApplyCouponRequest = z.object({
  code: z.string().min(1).max(50).transform((s) => s.trim().toUpperCase()),
}).strict();

// ===========================================================================
// order-service
// ===========================================================================

/**
 * Mirrors the saga state machine in internal/saga/model_test.go exactly. The
 * storefront maps these onto three user-visible states — placing, placed,
 * failed — but the raw value is exposed because support and the admin saga
 * inspector need it.
 */
export const OrderStatus = z.enum([
  'PENDING',
  'STOCK_RESERVED',
  'PAID',
  'STOCK_COMMITTED',
  'CONFIRMED',
  'COMPENSATING',
  'CANCELLED',
  'SHIPPED',
  'DELIVERED',
  'REFUNDED',
]);
export type OrderStatus = z.infer<typeof OrderStatus>;

/** The states from which nothing can be rolled back. docs/DESIGN-INVARIANTS.md §1. */
export const POINT_OF_NO_RETURN: readonly OrderStatus[] = ['PAID', 'STOCK_COMMITTED', 'CONFIRMED'];

/** Whether the checkout UI should keep polling. */
export const isTerminal = (s: OrderStatus): boolean =>
  ['CONFIRMED', 'CANCELLED', 'SHIPPED', 'DELIVERED', 'REFUNDED'].includes(s);

export const OrderLine = z.object({
  sku:       Sku,
  productId: ProductId,
  title:     z.string(),
  image:     z.string().url().nullable(),
  quantity:  z.number().int().min(1),
  unitPrice: Money,
  lineTotal: Money,
}).strict();
export type OrderLine = z.infer<typeof OrderLine>;

export const Order = z.object({
  id:     OrderId,
  userId: UserId,
  status: OrderStatus,
  lines:  z.array(OrderLine),
  subtotal:      Money,
  discountTotal: Money,
  shippingTotal: Money,
  taxTotal:      Money,
  total:         Money,
  shippingAddress: Address,
  billingAddress:  Address.nullable(),
  paymentId:       PaymentId.nullable(),
  reservationId:   ReservationId.nullable(),
  cancellationReason: CancellationReason.nullable(),
  trackingNumber:  z.string().nullable(),
  rulesVersion:    z.string(),
  placedAt:        Timestamp,
  updatedAt:       Timestamp,
}).strict();
export type Order = z.infer<typeof Order>;

export const OrderPage = paginated(Order);
export type OrderPage = z.infer<typeof OrderPage>;

export const CreateOrderRequest = z.object({
  cartId:  CartId,
  /** Must match the cart the user actually saw, or the request is rejected. */
  cartVersion: z.number().int().min(0),
  shippingAddress: Address,
  billingAddress:  Address.optional(),
  paymentMethodToken: z.string().min(1),
  /** Echoed from the cart. Guarantees we charge what was displayed. */
  expectedTotal: Money,
}).strict();
export type CreateOrderRequest = z.infer<typeof CreateOrderRequest>;

/**
 * 202 Accepted. Checkout is asynchronous — the saga has started but has not
 * finished, and pretending otherwise would mean holding an HTTP connection
 * open across three services and a card network.
 */
export const CreateOrderResponse = z.object({
  orderId: OrderId,
  status:  OrderStatus,
  /** Poll here. Or subscribe to the SSE stream at the same path + /stream. */
  statusUrl: z.string(),
  /** Server's hint for how long to wait before the first poll, in ms. */
  pollAfterMs: z.number().int().positive().default(500),
}).strict();

/** Lightweight shape for the checkout progress poller. */
export const OrderStatusResponse = z.object({
  orderId: OrderId,
  status:  OrderStatus,
  terminal: z.boolean(),
  cancellationReason: CancellationReason.nullable(),
  unavailable: z.array(z.object({
    sku:       z.string(),
    requested: z.number().int(),
    available: z.number().int(),
  }).strict()).optional(),
  updatedAt: Timestamp,
}).strict();
export type OrderStatusResponse = z.infer<typeof OrderStatusResponse>;

// ===========================================================================
// search-service
// ===========================================================================

export const SearchFacetValue = z.object({
  value:    z.string(),
  count:    z.number().int().min(0),
  selected: z.boolean().default(false),
}).strict();

export const SearchFacet = z.object({
  field:  z.string(),
  label:  z.string(),
  type:   z.enum(['terms', 'range', 'boolean']),
  values: z.array(SearchFacetValue),
}).strict();
export type SearchFacet = z.infer<typeof SearchFacet>;

export const SearchHit = z.object({
  productId: ProductId,
  sku:       Sku,
  title:     z.string(),
  slug:      z.string(),
  brand:     z.string().nullable(),
  image:     z.string().url().nullable(),
  price:     Money,
  listPrice: Money.nullable(),
  rating:    z.number().min(0).max(5).nullable(),
  ratingCount: z.number().int().min(0).default(0),
  inStock:   z.boolean(),
  score:     z.number(),
  /** Elasticsearch highlight fragments, already HTML-escaped by the service. */
  highlights: z.record(z.array(z.string())).optional(),
}).strict();
export type SearchHit = z.infer<typeof SearchHit>;

export const SearchRequest = z.object({
  q:      z.string().max(200).default(''),
  page:   z.coerce.number().int().min(1).max(100).default(1),
  size:   z.coerce.number().int().min(1).max(100).default(24),
  sort:   z.enum(['relevance', 'price_asc', 'price_desc', 'newest', 'rating']).default('relevance'),
  filters: z.record(z.array(z.string())).default({}),
  priceMin: z.coerce.number().int().optional(),
  priceMax: z.coerce.number().int().optional(),
  inStockOnly: z.coerce.boolean().default(false),
}).strict();
export type SearchRequest = z.infer<typeof SearchRequest>;

export const SearchResponse = z.object({
  hits:   z.array(SearchHit),
  total:  z.number().int().min(0),
  /** ES caps deep pagination; this says whether `total` is exact or a floor. */
  totalIsLowerBound: z.boolean().default(false),
  page:   z.number().int(),
  size:   z.number().int(),
  facets: z.array(SearchFacet),
  tookMs: z.number().int(),
  /** Populated when the query had no hits and a spelling correction exists. */
  didYouMean: z.string().nullable(),
  /** Set when OpenSearch was unavailable and we fell back to a Postgres LIKE. */
  degraded: z.boolean().default(false),
}).strict();
export type SearchResponse = z.infer<typeof SearchResponse>;

export const SuggestResponse = z.object({
  suggestions: z.array(z.object({
    text: z.string(),
    type: z.enum(['query', 'product', 'category', 'brand']),
    productId: ProductId.optional(),
    image: z.string().url().nullable().optional(),
  }).strict()),
}).strict();

// ===========================================================================
// recommendation-service
// ===========================================================================

export const RecommendationRequest = z.object({
  /** Personalize campaign selector, not a raw ARN — the ARN stays server-side. */
  placement: z.enum([
    'home_for_you',
    'pdp_similar',
    'pdp_frequently_bought_together',
    'cart_upsell',
    'search_reranked',
    'email_winback',
  ]),
  userId:    UserId.optional(),
  itemId:    ProductId.optional(),
  itemIds:   z.array(ProductId).max(50).optional(),
  count:     z.coerce.number().int().min(1).max(50).default(10),
  context:   z.record(z.string()).optional(),
}).strict();

export const RecommendationResponse = z.object({
  /**
   * Echo this on the resulting activity events so the campaign can actually be
   * evaluated. Without it, Personalize has no way to attribute a purchase to
   * a recommendation and the model never improves.
   */
  recommendationId: z.string(),
  placement: z.string(),
  items: z.array(z.object({
    productId: ProductId,
    score:     z.number().nullable(),
    reason:    z.string().nullable(),
  }).strict()),
  /**
   * True when Personalize was unavailable or cold-start and we served the
   * fallback ranker (bestsellers in category). The UI drops the "Recommended
   * for you" heading in that case rather than lying about personalisation.
   */
  fallback: z.boolean().default(false),
  fallbackReason: z.string().nullable(),
}).strict();
export type RecommendationResponse = z.infer<typeof RecommendationResponse>;

// ===========================================================================
// review-service
// ===========================================================================

export const Review = z.object({
  id:        ReviewId,
  productId: ProductId,
  userId:    UserId,
  authorName: z.string(),
  rating:    z.number().int().min(1).max(5),
  title:     z.string().max(200),
  body:      z.string().max(5000),
  /** Only true when the reviewer has a CONFIRMED order containing this product. */
  verifiedPurchase: z.boolean(),
  helpfulCount: z.number().int().min(0).default(0),
  images:    z.array(z.string().url()).default([]),
  status:    z.enum(['PENDING_MODERATION', 'PUBLISHED', 'REJECTED']),
  createdAt: Timestamp,
}).strict();
export type Review = z.infer<typeof Review>;

export const ReviewPage = paginated(Review);
export type ReviewPage = z.infer<typeof ReviewPage>;

export const CreateReviewRequest = z.object({
  productId: ProductId,
  rating:    z.number().int().min(1).max(5),
  title:     z.string().min(1).max(200),
  body:      z.string().min(10).max(5000),
  images:    z.array(z.string().url()).max(5).default([]),
}).strict();

export const RatingSummary = z.object({
  productId: ProductId,
  average:   z.number().min(0).max(5),
  count:     z.number().int().min(0),
  /** Index 0 is 1-star. Drives the histogram on the PDP. */
  distribution: z.tuple([
    z.number().int(), z.number().int(), z.number().int(),
    z.number().int(), z.number().int(),
  ]),
}).strict();

// ===========================================================================
// admin-only
// ===========================================================================

/** What the admin saga inspector renders for a single order. */
export const SagaTrace = z.object({
  orderId: OrderId,
  status:  OrderStatus,
  steps: z.array(z.object({
    step:      z.enum(['RESERVE', 'AUTHORIZE', 'COMMIT', 'CAPTURE', 'RELEASE', 'VOID']),
    state:     z.enum(['PENDING', 'SENT', 'ACKED', 'FAILED', 'TIMED_OUT', 'SKIPPED']),
    attempts:  z.number().int().min(0),
    sentAt:    Timestamp.nullable(),
    ackedAt:   Timestamp.nullable(),
    /**
     * When this step stops being "in flight" and starts being "overdue".
     *
     * Null on steps past the point of no return: docs/DESIGN-INVARIANTS.md §1
     * shows that a timeout there would trigger a compensation that loses money,
     * so those steps roll forward only and have no deadline at all. The admin
     * inspector uses the same predicate as the sweeper — SENT, unacknowledged,
     * deadline passed — so what an operator sees matches what the system acts on.
     */
    deadlineAt: Timestamp.nullable(),
    error:     z.string().nullable(),
    eventId:   z.string().nullable(),
  }).strict()),
  /** True while the saga is past the point of no return — the UI hides "cancel". */
  rollbackForbidden: z.boolean(),
  correlationId: z.string(),
  startedAt: Timestamp,
  updatedAt: Timestamp,
}).strict();
export type SagaTrace = z.infer<typeof SagaTrace>;

export const DlqMessage = z.object({
  id:            z.string(),
  originalTopic: z.string(),
  eventId:       z.string(),
  eventType:     z.string(),
  attempts:      z.number().int(),
  reason:        z.string(),
  payload:       z.unknown(),
  failedAt:      Timestamp,
}).strict();

export const DashboardKpis = z.object({
  ordersToday:        z.number().int(),
  revenueToday:       Money,
  averageOrderValue:  Money,
  conversionRate:     z.number().min(0).max(1),
  cartAbandonmentRate: z.number().min(0).max(1),
  /** Orders stuck in a non-terminal saga state past their timeout. Should be 0. */
  stuckSagas:         z.number().int().min(0),
  dlqDepth:           z.number().int().min(0),
  p99CheckoutMs:      z.number().int(),
  /** Percentage of the monthly error budget consumed. Alert bands: 2/5/10%. */
  errorBudgetBurn:    z.number(),
}).strict();
export type DashboardKpis = z.infer<typeof DashboardKpis>;
