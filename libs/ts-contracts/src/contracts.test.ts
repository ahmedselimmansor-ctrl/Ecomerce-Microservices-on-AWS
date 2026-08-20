import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import {
  Money, formatMoney, addMoney, multiplyMoney, ProblemDetails, Address,
  OrderId, Sku, Timestamp,
} from './primitives.js';
import {
  OrderEvent, SagaCommand, InventoryEvent, SCHEMA_BY_TOPIC, TOPICS, dlqFor,
  OrderCreated, InventoryReservationFailed,
} from './events.js';
import { OrderStatus, isTerminal, POINT_OF_NO_RETURN, Cart, SearchResponse } from './api.js';

const HERE = dirname(fileURLToPath(import.meta.url));
const ulid = (p: string) => `${p}_01J8Z3K9S2M4P6R8T0V2X4Y6A8`;
const now = '2026-08-17T10:00:00.000Z';

const envelope = <T>(type: string, data: T) => ({
  specversion: '1.0' as const,
  id: '01J8Z3K9S2M4P6R8T0V2X4Y6A8',
  source: 'souq/order-service',
  type,
  time: now,
  datacontenttype: 'application/json' as const,
  data,
});

describe('money', () => {
  it('rejects a float amount, because minor units are integers', () => {
    expect(Money.safeParse({ amount: 12.99, currency: 'EUR' }).success).toBe(false);
  });

  it('rejects a lowercase or malformed currency', () => {
    expect(Money.safeParse({ amount: 100, currency: 'eur' }).success).toBe(false);
    expect(Money.safeParse({ amount: 100, currency: 'EURO' }).success).toBe(false);
  });

  it('accepts negative amounts for discounts and refunds', () => {
    expect(Money.parse({ amount: -500, currency: 'EUR' })).toEqual({ amount: -500, currency: 'EUR' });
  });

  it('rejects unknown keys rather than silently dropping them', () => {
    expect(Money.safeParse({ amount: 1, currency: 'EUR', cents: 99 }).success).toBe(false);
  });

  it('formats without ever going through a float total', () => {
    expect(formatMoney({ amount: 129900, currency: 'EUR' }, 'de-DE')).toContain('1.299,00');
  });

  it('refuses to add across currencies', () => {
    expect(() => addMoney({ amount: 1, currency: 'EUR' }, { amount: 1, currency: 'USD' })).toThrow();
  });

  it('multiplies exactly — the case that breaks float arithmetic', () => {
    // 0.1 * 3 !== 0.3 in IEEE 754. In minor units it is just 10 * 3.
    expect(multiplyMoney({ amount: 10, currency: 'EUR' }, 3)).toEqual({ amount: 30, currency: 'EUR' });
  });
});

describe('identifiers', () => {
  it('accepts a well formed prefixed ULID', () => {
    expect(OrderId.parse(ulid('ord'))).toBe(ulid('ord'));
  });

  it('rejects an id carrying the wrong prefix', () => {
    // The whole point: a sku handed to an order field fails at the edge.
    expect(OrderId.safeParse(ulid('sku')).success).toBe(false);
  });

  it('rejects ULID-ambiguous characters', () => {
    expect(Sku.safeParse('sku_01J8Z3K9S2M4P6R8T0V2X4Y6AI').success).toBe(false); // I
    expect(Sku.safeParse('sku_01J8Z3K9S2M4P6R8T0V2X4Y6AL').success).toBe(false); // L
  });

  it('rejects a bare uuid', () => {
    expect(OrderId.safeParse('550e8400-e29b-41d4-a716-446655440000').success).toBe(false);
  });
});

describe('timestamps', () => {
  it('requires UTC with a Z suffix', () => {
    expect(Timestamp.safeParse('2026-08-17T10:00:00.000Z').success).toBe(true);
    expect(Timestamp.safeParse('2026-08-17T10:00:00+02:00').success).toBe(false);
    expect(Timestamp.safeParse('2026-08-17 10:00:00').success).toBe(false);
  });
});

describe('order events', () => {
  const created = envelope('souq.order.created.v1', {
    orderId: ulid('ord'),
    userId: ulid('usr'),
    items: [{
      sku: ulid('sku'), productId: ulid('prd'), quantity: 2,
      unitPrice: { amount: 4999, currency: 'EUR' },
    }],
    subtotal:      { amount: 9998, currency: 'EUR' },
    discountTotal: { amount: 0, currency: 'EUR' },
    shippingTotal: { amount: 499, currency: 'EUR' },
    taxTotal:      { amount: 1900, currency: 'EUR' },
    total:         { amount: 12397, currency: 'EUR' },
    shippingAddress: {
      recipient: 'A. Hassan', line1: '1 Nile St', city: 'Cairo',
      postalCode: '11511', countryCode: 'EG',
    },
    rulesVersion: 'rules-2026-08-01',
    idempotencyKey: '550e8400-e29b-41d4-a716-446655440000',
  });

  it('parses a valid OrderCreated', () => {
    const parsed = OrderEvent.parse(created);
    expect(parsed.type).toBe('souq.order.created.v1');
  });

  it('narrows the union on `type`', () => {
    const parsed = OrderEvent.parse(created);
    if (parsed.type === 'souq.order.created.v1') {
      // Type-level assertion: this only compiles if narrowing works.
      expect(parsed.data.items[0]!.quantity).toBe(2);
    } else {
      throw new Error('discriminated union failed to narrow');
    }
  });

  it('rejects an order with zero items', () => {
    const bad = structuredClone(created);
    (bad.data as { items: unknown[] }).items = [];
    expect(OrderEvent.safeParse(bad).success).toBe(false);
  });

  it('rejects a missing rulesVersion — without it an order cannot be re-priced', () => {
    const bad = structuredClone(created) as Record<string, any>;
    delete bad.data.rulesVersion;
    expect(OrderEvent.safeParse(bad).success).toBe(false);
  });

  it('rejects an unknown event type on the topic', () => {
    expect(OrderEvent.safeParse(envelope('souq.order.teleported.v1', {})).success).toBe(false);
  });
});

describe('saga commands', () => {
  it('defaults the reservation TTL to 900s as CONTRACTS.md §4 requires', () => {
    const cmd = SagaCommand.parse(envelope('souq.inventory.reserve.v1', {
      orderId: ulid('ord'),
      reservationId: ulid('rsv'),
      items: [{ sku: ulid('sku'), quantity: 1 }],
    }));
    if (cmd.type !== 'souq.inventory.reserve.v1') throw new Error('wrong type');
    expect(cmd.data.ttlSeconds).toBe(900);
  });
});

describe('inventory events', () => {
  it('carries the tombstone flag on a release that overtook its reserve', () => {
    const e = InventoryEvent.parse(envelope('souq.inventory.released.v1', {
      orderId: ulid('ord'), reservationId: ulid('rsv'), wasTombstone: true,
    }));
    if (e.type !== 'souq.inventory.released.v1') throw new Error('wrong type');
    expect(e.data.wasTombstone).toBe(true);
  });

  it('constrains reservation failure reasons to the modelled set', () => {
    const bad = envelope('souq.inventory.reservation_failed.v1', {
      orderId: ulid('ord'), reservationId: ulid('rsv'), reasonCode: 'BECAUSE_I_SAID_SO',
    });
    expect(InventoryReservationFailed.safeParse(bad).success).toBe(false);
  });
});

describe('topic routing', () => {
  it('has a schema for every topic', () => {
    for (const topic of Object.values(TOPICS)) {
      expect(SCHEMA_BY_TOPIC[topic], `no schema registered for ${topic}`).toBeDefined();
    }
  });

  it('derives DLQ names consistently', () => {
    expect(dlqFor(TOPICS.orderEvents)).toBe('souq.order.events.v1.dlq');
  });
});

describe('order status', () => {
  it('marks exactly the settled states terminal', () => {
    expect(isTerminal('CONFIRMED')).toBe(true);
    expect(isTerminal('CANCELLED')).toBe(true);
    expect(isTerminal('PENDING')).toBe(false);
    expect(isTerminal('COMPENSATING')).toBe(false);
  });

  it('never lets a point-of-no-return state be rolled back', () => {
    // docs/DESIGN-INVARIANTS.md §1 — these are the states with no compensation edge.
    for (const s of POINT_OF_NO_RETURN) {
      expect(['PAID', 'STOCK_COMMITTED', 'CONFIRMED']).toContain(s);
    }
    expect(POINT_OF_NO_RETURN).not.toContain('STOCK_RESERVED');
    expect(POINT_OF_NO_RETURN).not.toContain('PENDING');
  });

  it('covers every saga state declared in order-service', () => {
    // Cross-language check against the Go source itself, not a parallel
    // description of it. Adding a state to machine.go without adding it to
    // OrderStatus means the storefront renders an unknown status as a blank
    // step, which is the kind of bug that only shows up in production.
    const machine = readFileSync(
      resolve(HERE, '../../../services/order-service/internal/saga/machine.go'),
      'utf8',
    );

    const declared = [...machine.matchAll(/State\w+\s+State\s*=\s*"([A-Z_]+)"/g)].map((m) => m[1]!);
    expect(declared.length, 'no saga states parsed out of machine.go').toBeGreaterThan(0);

    for (const state of declared) {
      expect(
        OrderStatus.options,
        `${state} is a saga state in machine.go but not in OrderStatus`,
      ).toContain(state);
    }
  });
});

describe('problem details', () => {
  it('requires a SCREAMING_SNAKE_CASE machine code', () => {
    const base = {
      type: 'https://errors.souq.dev/inventory/insufficient-stock',
      title: 'Insufficient stock',
      status: 409,
      code: 'INVENTORY_INSUFFICIENT_STOCK',
      requestId: 'abc',
      timestamp: now,
    };
    expect(ProblemDetails.safeParse(base).success).toBe(true);
    expect(ProblemDetails.safeParse({ ...base, code: 'insufficient-stock' }).success).toBe(false);
  });
});

describe('degraded-mode flags exist where a fallback is allowed', () => {
  // Each of these defaults to false, so a service that forgets to set it
  // cannot accidentally claim it was degraded — and the UI is never left
  // guessing whether personalisation or promotions actually ran.
  it('cart defaults pricingDegraded to false', () => {
    const c = Cart.parse({
      id: ulid('crt'), userId: null, lines: [],
      subtotal: { amount: 0, currency: 'EUR' },
      discountTotal: { amount: 0, currency: 'EUR' },
      shippingTotal: { amount: 0, currency: 'EUR' },
      taxTotal: { amount: 0, currency: 'EUR' },
      total: { amount: 0, currency: 'EUR' },
      currency: 'EUR', rulesVersion: null, version: 0,
      expiresAt: now, updatedAt: now,
    });
    expect(c.pricingDegraded).toBe(false);
    expect(c.appliedCoupons).toEqual([]);
  });

  it('search defaults degraded to false', () => {
    const r = SearchResponse.parse({
      hits: [], total: 0, page: 1, size: 24, facets: [], tookMs: 3, didYouMean: null,
    });
    expect(r.degraded).toBe(false);
    expect(r.totalIsLowerBound).toBe(false);
  });
});

describe('address', () => {
  it('requires an ISO-3166 alpha-2 country code', () => {
    const a = { recipient: 'X', line1: 'Y', city: 'Z', postalCode: '1', countryCode: 'EG' };
    expect(Address.safeParse(a).success).toBe(true);
    expect(Address.safeParse({ ...a, countryCode: 'EGY' }).success).toBe(false);
  });
});
