import pino from 'pino';
import { Counter, Histogram, Registry, collectDefaultMetrics } from 'prom-client';

/**
 * Structured logging and metrics, matching docs/CONTRACTS.md §9.
 *
 * The redact list is not optional. A cart request body carries a payment
 * method token and a customer address, and pino logs whole objects by default.
 */
export const logger = pino({
  level: process.env.SOUQ_LOG_LEVEL ?? 'info',
  base: {
    service: 'cart-service',
    version: process.env.SOUQ_VERSION ?? 'dev',
    env: process.env.SOUQ_ENV ?? 'local',
  },
  timestamp: () => `,"timestamp":"${new Date().toISOString()}"`,
  redact: {
    paths: [
      'req.headers.authorization',
      'req.headers.cookie',
      '*.paymentMethodToken',
      '*.password',
      '*.mfaCode',
      '*.billingData',
    ],
    censor: '[REDACTED]',
  },
});

export const registry = new Registry();
collectDefaultMetrics({ register: registry });

export const httpRequests = new Counter({
  name: 'http_server_requests_total',
  help: 'HTTP requests by route, method and status class.',
  labelNames: ['route', 'method', 'status'] as const,
  registers: [registry],
});

export const httpLatency = new Histogram({
  name: 'http_server_requests_seconds',
  help: 'HTTP request duration.',
  labelNames: ['route', 'method'] as const,
  buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5],
  registers: [registry],
});

export const pricingCalls = new Counter({
  name: 'souq_pricing_calls_total',
  help: 'Calls to pricing-engine by outcome (ok/error/deadline/circuit_open).',
  labelNames: ['outcome'] as const,
  registers: [registry],
});

export const pricingLatency = new Histogram({
  name: 'souq_pricing_latency_seconds',
  help: 'pricing-engine gRPC latency. The 250ms deadline should sit above p999.',
  buckets: [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5],
  registers: [registry],
});

/**
 * Carts served at list price because pricing-engine was unreachable. Not an
 * error — the cart still works — but a sustained rise means customers are
 * being shown prices without their promotions, which is a revenue and a trust
 * problem long before it is an availability one.
 */
export const degradedCarts = new Counter({
  name: 'souq_cart_pricing_degraded_total',
  help: 'Carts priced at list price because pricing-engine was unavailable.',
  registers: [registry],
});

export const cartConflicts = new Counter({
  name: 'souq_cart_version_conflicts_total',
  help: 'Optimistic-concurrency conflicts, i.e. two devices editing one cart.',
  registers: [registry],
});
