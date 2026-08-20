import { z } from 'zod';
import {
  Money, Address, Timestamp,
  OrderId, UserId, PaymentId, ReservationId, ProductId, Sku,
} from './primitives.js';

/**
 * CloudEvents envelopes for every topic in contracts/asyncapi/souq-events.v1.yaml.
 *
 * The discriminated union at the bottom is the point: a consumer switches on
 * `type` and TypeScript narrows `data` for it. An unhandled event type is a
 * compile error in an exhaustive switch, not a runtime surprise at 3am.
 */

// ---------------------------------------------------------------------------
// Envelope

export const TOPICS = {
  orderEvents:          'souq.order.events.v1',
  orderCommands:        'souq.order.commands.v1',
  inventoryEvents:      'souq.inventory.events.v1',
  paymentEvents:        'souq.payment.events.v1',
  catalogEvents:        'souq.catalog.events.v1',
  userActivity:         'souq.user.activity.v1',
  notificationCommands: 'souq.notification.commands.v1',
} as const;

export type Topic = (typeof TOPICS)[keyof typeof TOPICS];

/** Dead-letter topic for a given source topic. */
export const dlqFor = (topic: Topic): string => `${topic}.dlq`;

const cloudEventBase = {
  specversion:     z.literal('1.0'),
  /** ULID. This is the inbox dedup key — `processed_events.event_id`. */
  id:              z.string().min(1),
  source:          z.string().min(1),
  subject:         z.string().optional(),
  time:            Timestamp,
  datacontenttype: z.literal('application/json').default('application/json'),
  traceparent:     z.string().optional(),
  correlationid:   z.string().optional(),
  dataschema:      z.string().optional(),
};

/** Wraps a payload schema in the CloudEvents envelope with a literal type tag. */
const cloudEvent = <T extends string, D extends z.ZodTypeAny>(type: T, data: D) =>
  z.object({ ...cloudEventBase, type: z.literal(type), data }).strict();

// ---------------------------------------------------------------------------
// Shared payload fragments

export const OrderItem = z.object({
  sku:       Sku,
  productId: ProductId,
  title:     z.string().max(500).optional(),
  quantity:  z.number().int().min(1).max(999),
  unitPrice: Money,
  lineTotal: Money.optional(),
}).strict();
export type OrderItem = z.infer<typeof OrderItem>;

export const UnavailableSku = z.object({
  sku:       z.string(),
  requested: z.number().int().min(1),
  available: z.number().int().min(0),
}).strict();

export const ReserveItem = z.object({
  sku:      Sku,
  quantity: z.number().int().min(1),
}).strict();

// ---------------------------------------------------------------------------
// Order events

export const OrderCreated = cloudEvent('souq.order.created.v1', z.object({
  orderId:        OrderId,
  userId:         UserId,
  items:          z.array(OrderItem).min(1).max(100),
  subtotal:       Money,
  discountTotal:  Money,
  shippingTotal:  Money,
  taxTotal:       Money,
  total:          Money,
  shippingAddress: Address,
  /**
   * Pricing rule set the totals were computed against. Without this an order
   * cannot be re-priced identically at capture time, and a promotion that
   * expired mid-checkout would silently change what the customer agreed to.
   */
  rulesVersion:   z.string(),
  idempotencyKey: z.string().uuid(),
}).strict());

export const OrderConfirmed = cloudEvent('souq.order.confirmed.v1', z.object({
  orderId:       OrderId,
  userId:        UserId,
  paymentId:     PaymentId,
  reservationId: ReservationId,
  total:         Money,
  confirmedAt:   Timestamp,
}).strict());

export const CancellationReason = z.enum([
  'INSUFFICIENT_STOCK',
  'PAYMENT_DECLINED',
  'PAYMENT_TIMEOUT',
  'RESERVATION_TIMEOUT',
  'CUSTOMER_CANCELLED',
  'FRAUD_REJECTED',
]);
export type CancellationReason = z.infer<typeof CancellationReason>;

export const OrderCancelled = cloudEvent('souq.order.cancelled.v1', z.object({
  orderId:     OrderId,
  userId:      UserId,
  reasonCode:  CancellationReason,
  failedStep:  z.enum(['RESERVE', 'AUTHORIZE', 'COMMIT', 'CAPTURE', 'NONE']).default('NONE'),
  unavailable: z.array(UnavailableSku).optional(),
}).strict());

export const OrderShipped = cloudEvent('souq.order.shipped.v1', z.object({
  orderId:        OrderId,
  trackingNumber: z.string(),
  carrier:        z.string(),
  shippedAt:      Timestamp,
  estimatedDelivery: z.string().optional(),
}).strict());

// ---------------------------------------------------------------------------
// Saga commands

export const InventoryReserve = cloudEvent('souq.inventory.reserve.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
  items:         z.array(ReserveItem).min(1),
  ttlSeconds:    z.number().int().positive().default(900),
}).strict());

export const InventoryRelease = cloudEvent('souq.inventory.release.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
  reasonCode:    z.string(),
}).strict());

export const InventoryCommit = cloudEvent('souq.inventory.commit.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
}).strict());

export const PaymentAuthorize = cloudEvent('souq.payment.authorize.v1', z.object({
  orderId:   OrderId,
  paymentId: PaymentId,
  userId:    UserId,
  amount:    Money,
  /** PSP-side token. A raw PAN never enters this platform. */
  paymentMethodToken: z.string(),
  billingAddress: Address.optional(),
}).strict());

export const PaymentCapture = cloudEvent('souq.payment.capture.v1', z.object({
  orderId:   OrderId,
  paymentId: PaymentId,
  amount:    Money.optional(),
}).strict());

export const PaymentVoid = cloudEvent('souq.payment.void.v1', z.object({
  orderId:    OrderId,
  paymentId:  PaymentId,
  reasonCode: z.string(),
}).strict());

// ---------------------------------------------------------------------------
// Inventory events

export const InventoryReserved = cloudEvent('souq.inventory.reserved.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
  expiresAt:     Timestamp,
  items:         z.array(ReserveItem).optional(),
}).strict());

export const InventoryReservationFailed = cloudEvent('souq.inventory.reservation_failed.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
  reasonCode: z.enum([
    'INSUFFICIENT_STOCK',
    'SKU_NOT_FOUND',
    'SKU_DISCONTINUED',
    'RESERVATION_TOMBSTONED',
  ]),
  unavailable: z.array(UnavailableSku).optional(),
}).strict());

export const InventoryReleased = cloudEvent('souq.inventory.released.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
  /** True when Release arrived before Reserve was ever processed. See docs/DESIGN-INVARIANTS.md §2. */
  wasTombstone:  z.boolean().default(false),
}).strict());

export const InventoryCommitted = cloudEvent('souq.inventory.committed.v1', z.object({
  orderId:       OrderId,
  reservationId: ReservationId,
}).strict());

export const StockChanged = cloudEvent('souq.inventory.stock_changed.v1', z.object({
  sku:       Sku,
  onHand:    z.number().int().min(0),
  reserved:  z.number().int().min(0),
  available: z.number().int().min(0),
  reasonCode: z.enum([
    'RESERVATION', 'RELEASE', 'COMMIT', 'RESTOCK', 'ADJUSTMENT', 'RETURN', 'SHRINKAGE',
  ]),
}).strict());

// ---------------------------------------------------------------------------
// Payment events

export const PaymentAuthorized = cloudEvent('souq.payment.authorized.v1', z.object({
  orderId:   OrderId,
  paymentId: PaymentId,
  amount:    Money,
  provider:  z.enum(['stripe', 'adyen', 'checkout', 'mock']),
  authCode:  z.string().optional(),
  /** Authorisation window. Capture after this fails and needs a re-auth. */
  expiresAt: Timestamp.optional(),
}).strict());

export const PaymentDeclineReason = z.enum([
  'INSUFFICIENT_FUNDS',
  'CARD_DECLINED',
  'CARD_EXPIRED',
  'INVALID_CVC',
  'FRAUD_SUSPECTED',
  'THREE_DS_FAILED',
  'PROVIDER_UNAVAILABLE',
  'AUTHORIZATION_EXPIRED',
]);
export type PaymentDeclineReason = z.infer<typeof PaymentDeclineReason>;

export const PaymentFailed = cloudEvent('souq.payment.failed.v1', z.object({
  orderId:     OrderId,
  paymentId:   PaymentId,
  reasonCode:  PaymentDeclineReason,
  declineCode: z.string().optional(),
  /**
   * PROVIDER_UNAVAILABLE is retriable and the saga backs off.
   * CARD_DECLINED is not, and the saga compensates immediately. Getting this
   * flag wrong either hammers a struggling PSP or cancels a recoverable order.
   */
  retriable:   z.boolean(),
}).strict());

export const PaymentCaptured = cloudEvent('souq.payment.captured.v1', z.object({
  orderId:    OrderId,
  paymentId:  PaymentId,
  amount:     Money,
  capturedAt: Timestamp.optional(),
}).strict());

export const PaymentVoided = cloudEvent('souq.payment.voided.v1', z.object({
  orderId:      OrderId,
  paymentId:    PaymentId,
  wasTombstone: z.boolean().default(false),
}).strict());

export const PaymentRefunded = cloudEvent('souq.payment.refunded.v1', z.object({
  orderId:    OrderId,
  paymentId:  PaymentId,
  refundId:   z.string(),
  amount:     Money,
  partial:    z.boolean().default(false),
  reasonCode: z.string().optional(),
}).strict());

// ---------------------------------------------------------------------------
// Catalog events

export const ProductStatus = z.enum(['DRAFT', 'ACTIVE', 'ARCHIVED', 'DISCONTINUED']);
export type ProductStatus = z.infer<typeof ProductStatus>;

export const ProductUpserted = cloudEvent('souq.catalog.product_upserted.v1', z.object({
  productId:    ProductId,
  sku:          Sku,
  title:        z.string().max(500),
  description:  z.string().max(20_000).optional(),
  brand:        z.string().optional(),
  categoryPath: z.array(z.string()).default([]),
  attributes:   z.record(z.string()).default({}),
  images: z.array(z.object({
    url:    z.string().url(),
    alt:    z.string().default(''),
    width:  z.number().int().optional(),
    height: z.number().int().optional(),
  }).strict()).default([]),
  price:     Money,
  listPrice: Money.optional(),
  status:    ProductStatus,
  locale:    z.string().default('en-GB'),
  updatedAt: Timestamp,
}).strict());

export const ProductDeleted = cloudEvent('souq.catalog.product_deleted.v1', z.object({
  productId: ProductId,
}).strict());

// ---------------------------------------------------------------------------
// User activity

export const ActivityType = z.enum([
  'VIEW', 'ADD_TO_CART', 'REMOVE_FROM_CART', 'PURCHASE', 'SEARCH', 'WISHLIST', 'REVIEW',
]);
export type ActivityType = z.infer<typeof ActivityType>;

export const UserActivity = cloudEvent('souq.activity.v1', z.object({
  eventType:   ActivityType,
  userId:      UserId.optional(),
  anonymousId: z.string().optional(),
  sessionId:   z.string(),
  itemId:      ProductId.optional(),
  query:       z.string().max(500).optional(),
  resultCount: z.number().int().min(0).optional(),
  position:    z.number().int().min(0).optional(),
  value:       Money.optional(),
  deviceType:  z.enum(['desktop', 'mobile', 'tablet', 'app']).optional(),
  locale:      z.string().optional(),
  occurredAt:  Timestamp,
  /** Set when the interaction came from a Personalize response — closes the loop. */
  recommendationId: z.string().optional(),
}).strict()).refine(
  (e) => e.data.userId !== undefined || e.data.anonymousId !== undefined,
  { message: 'activity requires either userId or anonymousId' },
);

// ---------------------------------------------------------------------------
// Notification commands

export const NotificationCommand = cloudEvent('souq.notify.v1', z.object({
  channel:  z.enum(['EMAIL', 'SMS', 'PUSH', 'WEBHOOK']),
  userId:   UserId.optional(),
  to:       z.string().optional(),
  template: z.string(),
  locale:   z.string().default('en-GB'),
  params:   z.record(z.unknown()).default({}),
  /**
   * Second line of defence behind the inbox table: even if two services
   * independently produce the same logical notification, the delivery log's
   * primary key stops the customer receiving two emails.
   */
  dedupeKey: z.string(),
  sendAfter: Timestamp.optional(),
}).strict());

// ---------------------------------------------------------------------------
// Unions, one per topic. Consumers parse with these.

export const OrderEvent = z.discriminatedUnion('type', [
  OrderCreated, OrderConfirmed, OrderCancelled, OrderShipped,
]);
export type OrderEvent = z.infer<typeof OrderEvent>;

export const SagaCommand = z.discriminatedUnion('type', [
  InventoryReserve, InventoryRelease, InventoryCommit,
  PaymentAuthorize, PaymentCapture, PaymentVoid,
]);
export type SagaCommand = z.infer<typeof SagaCommand>;

export const InventoryEvent = z.discriminatedUnion('type', [
  InventoryReserved, InventoryReservationFailed, InventoryReleased,
  InventoryCommitted, StockChanged,
]);
export type InventoryEvent = z.infer<typeof InventoryEvent>;

export const PaymentEvent = z.discriminatedUnion('type', [
  PaymentAuthorized, PaymentFailed, PaymentCaptured, PaymentVoided, PaymentRefunded,
]);
export type PaymentEvent = z.infer<typeof PaymentEvent>;

export const CatalogEvent = z.discriminatedUnion('type', [
  ProductUpserted, ProductDeleted,
]);
export type CatalogEvent = z.infer<typeof CatalogEvent>;

export const AnyEvent = z.union([
  OrderEvent, SagaCommand, InventoryEvent, PaymentEvent, CatalogEvent,
  UserActivity, NotificationCommand,
]);
export type AnyEvent = z.infer<typeof AnyEvent>;

/** Which schema to parse a message with, given the topic it arrived on. */
export const SCHEMA_BY_TOPIC = {
  [TOPICS.orderEvents]:          OrderEvent,
  [TOPICS.orderCommands]:        SagaCommand,
  [TOPICS.inventoryEvents]:      InventoryEvent,
  [TOPICS.paymentEvents]:        PaymentEvent,
  [TOPICS.catalogEvents]:        CatalogEvent,
  [TOPICS.userActivity]:         UserActivity,
  [TOPICS.notificationCommands]: NotificationCommand,
} as const;

/**
 * Exhaustiveness helper. Put it in the `default:` arm of a switch over an
 * event union and the compiler will refuse to build once someone adds an
 * event type you have not handled.
 */
export function assertNever(x: never, context = 'unhandled event'): never {
  throw new Error(`${context}: ${JSON.stringify(x)}`);
}
