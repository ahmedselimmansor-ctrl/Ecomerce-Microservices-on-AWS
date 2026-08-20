import { z } from 'zod';

/**
 * The scalar building blocks of every contract in the platform.
 *
 * These are deliberately strict. A schema that accepts anything is a schema
 * that will let a malformed upstream response reach a React component and
 * blow up three layers away from the cause. When one of these rejects, the
 * BFF returns 502 with the requestId, and the failure is attributed correctly.
 */

// ---------------------------------------------------------------------------
// Identifiers
//
// Every id is a prefixed ULID: sortable by creation time, 26 chars, Crockford
// base32 (no I/L/O/U, so no ambiguity when a human reads one off a screen to
// support). The prefix means a stray id in the wrong field fails at the edge
// instead of producing a mystifying 404 three services deep.

const ULID = '[0-9A-HJKMNP-TV-Z]{26}';

const prefixedId = (prefix: string, label: string) =>
  z.string().regex(new RegExp(`^${prefix}_${ULID}$`), `expected a ${label} id like ${prefix}_01J8Z3K9S2M4P6R8T0V2X4Y6A8`);

export const UserId         = prefixedId('usr', 'user');
export const ProductId      = prefixedId('prd', 'product');
export const Sku            = prefixedId('sku', 'SKU');
export const OrderId        = prefixedId('ord', 'order');
export const PaymentId      = prefixedId('pay', 'payment');
export const ReservationId  = prefixedId('rsv', 'reservation');
export const CartId         = prefixedId('crt', 'cart');
export const ReviewId       = prefixedId('rev', 'review');

// ---------------------------------------------------------------------------
// Money
//
// Minor units only. There is no float in this platform (docs/CONTRACTS.md §2.5)
// because 0.1 + 0.2 !== 0.3 and a cart total that is off by a cent is a
// support ticket, a failed reconciliation, and eventually an audit finding.

export const CurrencyCode = z.string().regex(/^[A-Z]{3}$/, 'ISO-4217 alphabetic code');

export const Money = z.object({
  /** Minor units. 129900 === €1,299.00. Negative for discounts and refunds. */
  amount: z.number().int().safe(),
  currency: CurrencyCode,
}).strict();
export type Money = z.infer<typeof Money>;

/** Formats money for display. The ONLY place minor units become a decimal. */
export function formatMoney(m: Money, locale = 'en-GB'): string {
  return new Intl.NumberFormat(locale, {
    style: 'currency',
    currency: m.currency,
  }).format(m.amount / 100);
}

export function addMoney(a: Money, b: Money): Money {
  if (a.currency !== b.currency) {
    throw new Error(`cannot add ${a.currency} to ${b.currency}`);
  }
  return { amount: a.amount + b.amount, currency: a.currency };
}

export function multiplyMoney(m: Money, qty: number): Money {
  if (!Number.isInteger(qty)) throw new Error('quantity must be an integer');
  return { amount: m.amount * qty, currency: m.currency };
}

export const zeroMoney = (currency: string): Money => ({ amount: 0, currency });

// ---------------------------------------------------------------------------
// Time

/** RFC 3339 UTC with a Z suffix. Not "any parseable date string". */
export const Timestamp = z
  .string()
  .regex(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$/, 'RFC 3339 UTC timestamp ending in Z');

export const DateOnly = z.string().regex(/^\d{4}-\d{2}-\d{2}$/);

// ---------------------------------------------------------------------------
// Common value objects

export const Address = z.object({
  recipient:   z.string().min(1).max(200),
  line1:       z.string().min(1).max(300),
  line2:       z.string().max(300).optional(),
  city:        z.string().min(1).max(150),
  region:      z.string().max(150).optional(),
  postalCode:  z.string().min(1).max(30),
  countryCode: z.string().regex(/^[A-Z]{2}$/),
  phone:       z.string().max(40).optional(),
}).strict();
export type Address = z.infer<typeof Address>;

export const Image = z.object({
  url:    z.string().url(),
  alt:    z.string().max(300).default(''),
  width:  z.number().int().positive().optional(),
  height: z.number().int().positive().optional(),
}).strict();
export type Image = z.infer<typeof Image>;

// ---------------------------------------------------------------------------
// Pagination — cursor based everywhere (docs/CONTRACTS.md §2.4).
//
// Offset pagination is not offered. On a table that is being written to while
// a user pages through it, offsets silently skip and duplicate rows, and on a
// large offset the database has to walk every skipped row to get there.

export const PageRequest = z.object({
  limit:  z.coerce.number().int().min(1).max(100).default(20),
  cursor: z.string().max(500).optional(),
}).strict();
export type PageRequest = z.infer<typeof PageRequest>;

export const paginated = <T extends z.ZodTypeAny>(item: T) =>
  z.object({
    items:      z.array(item),
    nextCursor: z.string().nullable(),
    hasMore:    z.boolean(),
  }).strict();

// ---------------------------------------------------------------------------
// Error envelope — RFC 9457 Problem Details, extended (docs/CONTRACTS.md §2.2).

export const FieldError = z.object({
  field:   z.string(),
  message: z.string(),
}).strict();

export const ProblemDetails = z.object({
  type:      z.string(),
  title:     z.string(),
  status:    z.number().int().min(100).max(599),
  detail:    z.string().optional(),
  instance:  z.string().optional(),
  /** Stable machine identifier. Switch on this, never on `detail`. */
  code:      z.string().regex(/^[A-Z][A-Z0-9_]*$/),
  requestId: z.string(),
  timestamp: Timestamp,
  errors:    z.array(FieldError).optional(),
}).strict();
export type ProblemDetails = z.infer<typeof ProblemDetails>;

/**
 * Every error code the storefront and admin are expected to handle explicitly.
 * Anything not in this list is rendered as a generic failure, so adding a new
 * code to a service without adding it here is a silently degraded experience
 * rather than a crash — deliberate, but it should still be caught in review.
 */
export const ERROR_CODES = [
  // 400 / 422
  'VALIDATION_FAILED',
  'UNSUPPORTED_CURRENCY',
  'WEAK_PASSWORD',
  // 401 / 403
  'UNAUTHENTICATED',
  'TOKEN_EXPIRED',
  'REFRESH_TOKEN_REUSED',
  'FORBIDDEN',
  'MFA_REQUIRED',
  // 404
  'PRODUCT_NOT_FOUND',
  'ORDER_NOT_FOUND',
  'CART_NOT_FOUND',
  // 409
  'INVENTORY_INSUFFICIENT_STOCK',
  'IDEMPOTENCY_KEY_REUSE',
  'REQUEST_IN_PROGRESS',
  'CART_STALE',
  'ORDER_NOT_CANCELLABLE',
  // 423 / 429
  'ACCOUNT_LOCKED',
  'RATE_LIMITED',
  // 402
  'PAYMENT_DECLINED',
  'PAYMENT_REQUIRES_ACTION',
  // 5xx
  'UPSTREAM_UNAVAILABLE',
  'UPSTREAM_TIMEOUT',
  'INTERNAL_ERROR',
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

export const isErrorCode = (v: string): v is ErrorCode =>
  (ERROR_CODES as readonly string[]).includes(v);
