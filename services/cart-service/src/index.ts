import Fastify from 'fastify';
import helmet from '@fastify/helmet';
import rateLimit from '@fastify/rate-limit';
import underPressure from '@fastify/under-pressure';
import { Redis } from 'ioredis';
import { z } from 'zod';
import { randomUUID } from 'node:crypto';

import { AddToCartRequest, UpdateCartLineRequest, ApplyCouponRequest, CartId, Sku } from '@souq/contracts';
import { CartService, CartStale, CartFull } from './cart.js';
import { PricingClient } from './pricing.js';
import { CatalogClient } from './catalog.js';
import { logger, registry, httpRequests, httpLatency, degradedCarts, cartConflicts } from './telemetry.js';

const required = (key: string): string => {
  const v = process.env[key];
  // Fail at boot, not on the first real request. A service that starts with
  // half its configuration discovers the gap in production.
  if (!v) throw new Error(`required environment variable ${key} is not set`);
  return v;
};

const redis = new Redis(required('SOUQ_REDIS_URL'), {
  maxRetriesPerRequest: 3,
  enableReadyCheck: true,
  // Exponential with a ceiling. Reconnecting in a tight loop against a
  // failing-over ElastiCache makes the failover slower.
  retryStrategy: (times: number) => Math.min(times * 200, 3000),
  // Reconnect on READONLY: that is what a replica promoted during an
  // ElastiCache failover returns until the client re-resolves the primary.
  reconnectOnError: (err: Error) => err.message.includes('READONLY'),
});

const pricing = new PricingClient(
  process.env.SOUQ_PRICING_GRPC ?? 'pricing-engine:9089',
  process.env.SOUQ_PRICING_PROTO ?? '/app/proto/pricing.proto',
);
const catalog = new CatalogClient(required('SOUQ_CATALOG_URL'));
const carts = new CartService(redis, pricing, catalog);

// unref so a pending prune timer does not hold the process open at shutdown.
setInterval(() => catalog.prune(), 60_000).unref();

const app = Fastify({
  logger,
  // The ALB in front of us sets X-Request-Id; trust it so one id spans the
  // whole fan-out.
  genReqId: (req) => (req.headers['x-request-id'] as string) ?? randomUUID(),
  trustProxy: true,
  bodyLimit: 64 * 1024,
  disableRequestLogging: false,
});

await app.register(helmet, { contentSecurityPolicy: false });

await app.register(rateLimit, {
  max: 300,
  timeWindow: '1 minute',
  // Per user when signed in, per IP otherwise. Keying purely on IP would
  // throttle everyone behind one corporate NAT together.
  keyGenerator: (req) => (req.headers['x-user-id'] as string) ?? req.ip,
  errorResponseBuilder: (req, ctx) => ({
    type: 'https://errors.souq.dev/cart/rate-limited',
    title: 'Too many requests',
    status: 429,
    code: 'RATE_LIMITED',
    detail: `Retry in ${Math.ceil(ctx.ttl / 1000)}s.`,
    requestId: req.id,
    timestamp: new Date().toISOString(),
  }),
});

// Sheds load before the event loop collapses. Returning 503 to some requests
// is strictly better than becoming unresponsive to all of them.
await app.register(underPressure, {
  maxEventLoopDelay: 1000,
  maxHeapUsedBytes: 400 * 1024 * 1024,
  message: 'service is under heavy load',
  retryAfter: 5,
  exposeStatusRoute: false,
});

app.addHook('onResponse', async (req, reply) => {
  const route = req.routeOptions.url ?? 'unknown';
  const cls = `${Math.floor(reply.statusCode / 100)}xx`;
  httpRequests.inc({ route, method: req.method, status: cls });
  httpLatency.observe({ route, method: req.method }, reply.elapsedTime / 1000);
});

// --------------------------------------------------------------------- errors

app.setErrorHandler((err, req, reply) => {
  const problem = (status: number, code: string, detail: string) => {
    reply.status(status).type('application/problem+json').send({
      type: `https://errors.souq.dev/cart/${code.toLowerCase().replace(/_/g, '-')}`,
      title: code.replace(/_/g, ' ').toLowerCase(),
      status, code, detail,
      requestId: req.id,
      timestamp: new Date().toISOString(),
    });
  };

  if (err instanceof CartStale) {
    cartConflicts.inc();
    // 409, not 400. The request was well formed; the world moved underneath
    // it. The client reloads and retries rather than "fixing" its input.
    return problem(409, 'CART_STALE', err.message);
  }
  if (err instanceof CartFull) return problem(422, 'VALIDATION_FAILED', err.message);

  switch ((err as Error).message) {
    case 'CART_NOT_FOUND':       return problem(404, 'CART_NOT_FOUND', 'No such cart.');
    case 'PRODUCT_NOT_FOUND':    return problem(404, 'PRODUCT_NOT_FOUND', 'No such product.');
    case 'PRODUCT_NOT_AVAILABLE':return problem(409, 'PRODUCT_NOT_FOUND', 'This product is no longer for sale.');
    case 'LINE_NOT_FOUND':       return problem(404, 'CART_NOT_FOUND', 'That item is not in the cart.');
  }

  if (err instanceof z.ZodError) {
    return reply.status(422).type('application/problem+json').send({
      type: 'https://errors.souq.dev/cart/validation-failed',
      title: 'Validation failed', status: 422, code: 'VALIDATION_FAILED',
      requestId: req.id, timestamp: new Date().toISOString(),
      errors: err.issues.map((i) => ({ field: i.path.join('.'), message: i.message })),
    });
  }

  req.log.error({ err }, 'unhandled error');
  // Never leak the cause on a 5xx: it can carry a Redis key, a hostname, or
  // another customer's data.
  return problem(500, 'INTERNAL_ERROR', 'The request could not be completed.');
});

// --------------------------------------------------------------------- routes

app.get('/health/live', async () => ({ status: 'UP' }));

app.get('/health/ready', async (_req, reply) => {
  try {
    await redis.ping();
  } catch {
    return reply.status(503).send({ status: 'DOWN', redis: 'unreachable' });
  }
  // pricing-engine being down is NOT unreadiness. The cart degrades to list
  // price and keeps working; taking this pod out of the load balancer would
  // turn a partial degradation into a full outage.
  return { status: 'UP', pricingBreaker: pricing.breakerState };
});

app.get('/metrics', async (_req, reply) => {
  reply.type(registry.contentType);
  return registry.metrics();
});

const userOf = (req: { headers: Record<string, unknown> }): string | null =>
  (req.headers['x-user-id'] as string) ?? null;

app.post('/v1/carts', async (req, reply) => {
  const body = z.object({
    currency: z.string().regex(/^[A-Z]{3}$/).default('EGP'),
    countryCode: z.string().regex(/^[A-Z]{2}$/).default('EG'),
  }).parse(req.body ?? {});

  const cart = await carts.create(userOf(req as never), body.currency, body.countryCode);
  if (cart.pricingDegraded) degradedCarts.inc();
  return reply.status(201).send(cart);
});

app.get('/v1/carts/:cartId', async (req, reply) => {
  const { cartId } = z.object({ cartId: CartId }).parse(req.params);
  const cart = await carts.get(cartId);
  if (!cart) return reply.status(404).send({ code: 'CART_NOT_FOUND', status: 404, requestId: req.id });
  if (cart.pricingDegraded) degradedCarts.inc();
  return cart;
});

app.post('/v1/carts/:cartId/lines', async (req) => {
  const { cartId } = z.object({ cartId: CartId }).parse(req.params);
  const body = AddToCartRequest.extend({ version: z.number().int().min(0) }).parse(req.body);
  return carts.addLine(cartId, body.sku, body.quantity, body.version);
});

app.patch('/v1/carts/:cartId/lines/:sku', async (req) => {
  const { cartId, sku } = z.object({ cartId: CartId, sku: Sku }).parse(req.params);
  const body = UpdateCartLineRequest.parse(req.body);
  return carts.updateLine(cartId, sku, body.quantity, body.version);
});

app.post('/v1/carts/:cartId/coupons', async (req) => {
  const { cartId } = z.object({ cartId: CartId }).parse(req.params);
  const body = ApplyCouponRequest.extend({ version: z.number().int().min(0) }).parse(req.body);
  return carts.applyCoupon(cartId, body.code, body.version);
});

/** Called by identity-service after a successful login. */
app.post('/v1/carts/:cartId/merge', async (req) => {
  const { cartId } = z.object({ cartId: CartId }).parse(req.params);
  const body = z.object({ userId: z.string() }).parse(req.body);
  return carts.merge(cartId, body.userId);
});

// ------------------------------------------------------------------ lifecycle

const shutdown = async (signal: string) => {
  logger.info({ signal }, 'shutting down');
  // Fastify drains in-flight requests before resolving.
  await app.close();
  pricing.close();
  await redis.quit();
  process.exit(0);
};
process.on('SIGTERM', () => void shutdown('SIGTERM'));
process.on('SIGINT', () => void shutdown('SIGINT'));

const addr = process.env.SOUQ_HTTP_ADDR ?? '0.0.0.0:8083';
const [host, port] = addr.split(':');
await app.listen({ host: host || '0.0.0.0', port: Number(port ?? 8083) });
