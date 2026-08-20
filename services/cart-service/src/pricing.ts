import { credentials, Client, Metadata, type ServiceError } from '@grpc/grpc-js';
import { loadSync } from '@grpc/proto-loader';
import type { Money } from '@souq/contracts';
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

type BreakerState = 'closed' | 'open' | 'half-open';

export class PricingClient {
  private client: Client;
  private state: BreakerState = 'closed';
  private failures: boolean[] = [];   // rolling window
  private openedAt = 0;

  // Tuning from docs/CONTRACTS.md §5.4: open at 50% failures over a
  // 20-request window, probe after 10 seconds.
  private readonly windowSize = 20;
  private readonly failureThreshold = 0.5;
  private readonly probeAfterMs = 10_000;

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

  /** Whether a call may proceed under the current breaker state. */
  private allow(): boolean {
    if (this.state === 'closed') return true;

    if (this.state === 'open') {
      if (Date.now() - this.openedAt < this.probeAfterMs) return false;
      // One probe. If it succeeds the breaker closes; if it fails we go
      // straight back to open without a thundering herd of retries.
      this.state = 'half-open';
      logger.info('pricing-engine circuit is half-open; sending a probe');
      return true;
    }

    // half-open: only the single probe is in flight.
    return false;
  }

  private record(success: boolean): void {
    if (this.state === 'half-open') {
      this.state = success ? 'closed' : 'open';
      if (success) {
        this.failures = [];
        logger.info('pricing-engine recovered; circuit closed');
      } else {
        this.openedAt = Date.now();
        logger.warn('pricing-engine probe failed; circuit re-opened');
      }
      return;
    }

    this.failures.push(!success);
    if (this.failures.length > this.windowSize) this.failures.shift();

    // Only judge on a full window. Opening after the first two failures of a
    // cold start would take pricing out on every deploy.
    if (this.failures.length < this.windowSize) return;

    const rate = this.failures.filter(Boolean).length / this.failures.length;
    if (rate >= this.failureThreshold && this.state === 'closed') {
      this.state = 'open';
      this.openedAt = Date.now();
      logger.error({ failureRate: rate }, 'pricing-engine circuit opened; carts will show list prices');
    }
  }

  get breakerState(): BreakerState { return this.state; }

  close(): void { this.client.close(); }
}
