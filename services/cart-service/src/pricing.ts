import { credentials, Client, Metadata, type ServiceError } from '@grpc/grpc-js';
import { loadSync } from '@grpc/proto-loader';
import type { Money } from '@souq/contracts';
import { CircuitBreaker, type BreakerState } from './circuit-breaker.js';

import { logger, pricingCalls, pricingLatency } from './telemetry.js';

/**
 * gRPC client for pricing-engine, wrapped in a circuit breaker.
 *
 * pricing-engine is the only synchronous dependency on the browsing path with
 * a sub-millisecond budget. It is also the one most likely to be redeploying,
 * because promotion rules change daily. So the interesting code here is not
 * the happy path — it is what happens when it is down.
 *
 * The breaker exists because retrying into a struggling service is how a
 * brownout becomes an outage. Once it opens, every call fails instantly at
 * this end and the cart degrades to list price, which both protects
 * pricing-engine and keeps our own p99 flat instead of pinned at the 250ms
 * deadline.
 */

const DEADLINE_MS = 250;

export interface PricedCart {
  lines: { unitEffectivePrice: Money; lineTotal: Money; lineDiscount: Money }[];
  subtotal: Money;
  discountTotal: Money;
  shippingTotal: Money;
  taxTotal: Money;
  grandTotal: Money;
  appliedPromotions: { promotionId: string; name: string; discount: Money; couponCode?: string }[];
  rejectedPromotions: { couponCode: string; reasonCode: string }[];
  rulesVersion: string;
  degraded: boolean;
}

export interface CalculateRequest {
  lines: {
    sku: string; productId: string; quantity: number; unitListPrice: Money;
    categoryPath: string[]; brand: string | null; attributes: Record<string, string>;
  }[];
  context: {
    userId?: string; currency: string; countryCode: string;
    couponCodes: string[]; channel: string;
  };
}


export class PricingClient {
  private client: Client;

  // The policy lives in CircuitBreaker so it can be tested without a gRPC
  // channel. Tuning is docs/CONTRACTS.md §5.4.
  private readonly breaker = new CircuitBreaker({
    windowSize: 20,
    failureThreshold: 0.5,
    probeAfterMs: 10_000,
    onHalfOpen: () => logger.info('pricing-engine circuit is half-open; sending a probe'),
    onClose: () => logger.info('pricing-engine recovered; circuit closed'),
    onOpen: (failureRate) =>
      logger.error({ failureRate }, 'pricing-engine circuit opened; carts will show list prices'),
  });

  constructor(target: string, protoPath: string) {
    const pkg = loadSync(protoPath, {
      keepCase: false, longs: String, enums: String, defaults: true, oneofs: true,
    });
    const proto = require('@grpc/grpc-js').loadPackageDefinition(pkg) as any;
    this.client = new proto.souq.pricing.v1.PricingService(
      target,
      credentials.createInsecure(), // mTLS is terminated by the mesh sidecar
      {
        'grpc.keepalive_time_ms': 30_000,
        'grpc.keepalive_timeout_ms': 5_000,
        // Bounded so a struggling pricing-engine cannot make us queue
        // unboundedly and run out of memory. Bulkhead, §5.4.
        'grpc.max_concurrent_streams': 50,
      },
    );
  }

  async calculate(req: CalculateRequest): Promise<PricedCart> {
    if (!this.allow()) {
      pricingCalls.inc({ outcome: 'circuit_open' });
      throw new Error('pricing-engine circuit is open');
    }

    const started = process.hrtime.bigint();

    return new Promise<PricedCart>((resolve, reject) => {
      const deadline = new Date(Date.now() + DEADLINE_MS);

      this.client.makeUnaryRequest(
        '/souq.pricing.v1.PricingService/CalculateCart',
        (v: unknown) => Buffer.from(JSON.stringify(v)),
        (b: Buffer) => JSON.parse(b.toString()),
        req,
        new Metadata(),
        { deadline },
        (err: ServiceError | null, res?: PricedCart) => {
          const ms = Number(process.hrtime.bigint() - started) / 1e6;
          pricingLatency.observe(ms / 1000);

          if (err || !res) {
            this.record(false);
            pricingCalls.inc({ outcome: err?.code === 4 ? 'deadline' : 'error' });
            reject(err ?? new Error('pricing-engine returned an empty response'));
            return;
          }
          this.record(true);
          pricingCalls.inc({ outcome: 'ok' });
          resolve(res);
        },
      );
    });
  }

  private allow(): boolean {
    return this.breaker.allow();
  }

  private record(success: boolean): void {
    this.breaker.record(success);
  }

  get breakerState(): BreakerState {
    return this.breaker.currentState;
  }

  close(): void { this.client.close(); }
}
