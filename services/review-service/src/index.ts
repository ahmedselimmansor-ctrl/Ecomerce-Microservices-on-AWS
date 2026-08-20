import Fastify from 'fastify';
import helmet from '@fastify/helmet';
import rateLimit from '@fastify/rate-limit';
import { MongoClient } from 'mongodb';
import { Kafka } from 'kafkajs';
import { z } from 'zod';
import { randomUUID } from 'node:crypto';

import { CreateReviewRequest, ProductId, ReviewId } from '@souq/contracts';
import { ReviewsRepository, DuplicateReview, ReviewNotFound } from './reviews.js';
import { logger, registry, httpRequests, httpLatency, reviewsSubmitted, reviewsFlagged, moderationQueueDepth } from './telemetry.js';

const required = (key: string): string => {
  const v = process.env[key];
  if (!v) throw new Error(`required environment variable ${key} is not set`);
  return v;
};

const mongo = new MongoClient(required('SOUQ_MONGO_URL'), {
  maxPoolSize: 20,
  serverSelectionTimeoutMS: 5_000,
  // Reviews are not money. A retried write that lands twice is caught by the
  // unique index, and w:1 keeps the PDP fast.
  retryWrites: true,
});
await mongo.connect();

const db = mongo.db('reviews');
const repo = new ReviewsRepository(db);
await repo.ensureIndexes();

const app = Fastify({
  logger,
  genReqId: (req) => (req.headers['x-request-id'] as string) ?? randomUUID(),
  trustProxy: true,
  bodyLimit: 128 * 1024, // review bodies can carry image URLs
});

await app.register(helmet, { contentSecurityPolicy: false });
await app.register(rateLimit, {
  max: 60,
  timeWindow: '1 minute',
  keyGenerator: (req) => (req.headers['x-user-id'] as string) ?? req.ip,
});

app.addHook('onResponse', async (req, reply) => {
  const route = req.routeOptions.url ?? 'unknown';
  httpRequests.inc({ route, method: req.method, status: `${Math.floor(reply.statusCode / 100)}xx` });
  httpLatency.observe({ route, method: req.method }, reply.elapsedTime / 1000);
});

const problem = (reply: any, req: any, status: number, code: string, detail: string) =>
  reply.status(status).type('application/problem+json').send({
    type: `https://errors.souq.dev/review/${code.toLowerCase().replace(/_/g, '-')}`,
    title: code.replace(/_/g, ' ').toLowerCase(),
    status, code, detail,
    requestId: req.id,
    timestamp: new Date().toISOString(),
  });

app.setErrorHandler((err, req, reply) => {
  if (err instanceof DuplicateReview) {
    // 409, not 400: the request is fine, the world already contains one.
    return problem(reply, req, 409, 'REVIEW_ALREADY_EXISTS', err.message);
  }
  if (err instanceof ReviewNotFound) {
    return problem(reply, req, 404, 'REVIEW_NOT_FOUND', err.message);
  }
  if (err instanceof z.ZodError) {
    return reply.status(422).type('application/problem+json').send({
      type: 'https://errors.souq.dev/review/validation-failed',
      title: 'Validation failed', status: 422, code: 'VALIDATION_FAILED',
      requestId: req.id, timestamp: new Date().toISOString(),
      errors: err.issues.map((i) => ({ field: i.path.join('.'), message: i.message })),
    });
  }
  req.log.error({ err }, 'unhandled error');
  // Never leak the cause on a 5xx.
  return problem(reply, req, 500, 'INTERNAL_ERROR', 'The request could not be completed.');
});

// ----------------------------------------------------------------- routes

app.get('/health/live', async () => ({ status: 'UP' }));

app.get('/health/ready', async (_req, reply) => {
  try {
    await db.command({ ping: 1 });
    return { status: 'UP' };
  } catch {
    return reply.status(503).send({ status: 'DOWN', mongodb: 'unreachable' });
  }
});

app.get('/metrics', async (_req, reply) => {
  reply.type(registry.contentType);
  return registry.metrics();
});

app.get('/v1/products/:productId/reviews', async (req) => {
  const { productId } = z.object({ productId: ProductId }).parse(req.params);
  const qs = z.object({
    limit: z.coerce.number().int().min(1).max(50).default(20),
    cursor: z.string().optional(),
    verifiedOnly: z.coerce.boolean().default(false),
    rating: z.coerce.number().int().min(1).max(5).optional(),
  }).parse(req.query);

  return repo.listForProduct(productId, qs);
});

app.get('/v1/products/:productId/rating', async (req) => {
  const { productId } = z.object({ productId: ProductId }).parse(req.params);
  const summary = await repo.summary(productId);

  // A product with no reviews is not an error — the PDP renders "no reviews
  // yet" from a zeroed summary rather than branching on a 404.
  return summary ?? {
    productId, average: 0, count: 0, distribution: [0, 0, 0, 0, 0], verifiedCount: 0,
  };
});

app.post('/v1/reviews', async (req, reply) => {
  const userId = req.headers['x-user-id'] as string | undefined;
  if (!userId) return problem(reply, req, 401, 'UNAUTHENTICATED', 'Sign in to write a review.');

  const body = CreateReviewRequest.parse(req.body);
  const authorName = (req.headers['x-user-name'] as string) ?? 'A customer';

  const review = await repo.create({
    id: `rev_${ulid()}`,
    productId: body.productId,
    userId,
    authorName,
    rating: body.rating as 1 | 2 | 3 | 4 | 5,
    title: body.title,
    body: body.body,
    images: body.images,
    locale: (req.headers['accept-language'] as string)?.slice(0, 5) ?? 'en-GB',
  });

  reviewsSubmitted.inc({ verified: String(review.verifiedPurchase) });
  for (const flag of review.moderation?.autoFlags ?? []) reviewsFlagged.inc({ flag });

  // 202, not 201: everything queues for moderation, so the review is accepted
  // but not yet visible. Returning 201 would imply it is live.
  return reply.status(202).send({
    reviewId: review._id,
    status: review.status,
    verifiedPurchase: review.verifiedPurchase,
  });
});

app.post('/v1/reviews/:reviewId/helpful', async (req) => {
  const { reviewId } = z.object({ reviewId: ReviewId }).parse(req.params);
  const userId = (req.headers['x-user-id'] as string) ?? req.ip;
  // Idempotent on (review, user): one vote per person, not per click.
  const count = await repo.markHelpful(reviewId, userId, db);
  return { reviewId, helpfulCount: count };
});

// Moderation. Gated on the role header the gateway sets after verifying the JWT.
app.post('/v1/admin/reviews/:reviewId/moderate', async (req, reply) => {
  const roles = ((req.headers['x-user-roles'] as string) ?? '').split(',');
  if (!roles.includes('ADMIN') && !roles.includes('SUPPORT')) {
    return problem(reply, req, 403, 'FORBIDDEN', 'Moderation requires the SUPPORT or ADMIN role.');
  }

  const { reviewId } = z.object({ reviewId: ReviewId }).parse(req.params);
  const body = z.object({
    decision: z.enum(['PUBLISHED', 'REJECTED']),
    reason: z.string().max(500).optional(),
  }).parse(req.body);

  await repo.moderate(reviewId, body.decision, req.headers['x-user-id'] as string, body.reason);
  moderationQueueDepth.inc({ decision: body.decision });
  return { reviewId, status: body.decision };
});

// ------------------------------------------------------------ order consumer

// Verified-purchase status is DERIVED from confirmed orders, never taken from
// the request body. A review service that trusts a `verifiedPurchase: true`
// in the payload is a review service that sells fake reviews to anyone who
// reads the API docs.
const kafka = new Kafka({
  clientId: 'review-service',
  brokers: required('SOUQ_KAFKA_BROKERS').split(','),
});
const consumer = kafka.consumer({ groupId: 'review-service.order-events' });

await consumer.connect();
await consumer.subscribe({ topic: 'souq.order.confirmed.v1', fromBeginning: true });
await consumer.subscribe({ topic: 'souq.order.events.v1', fromBeginning: true });

void consumer.run({
  eachMessage: async ({ message }) => {
    if (!message.value) return;
    try {
      const envelope = JSON.parse(message.value.toString());
      if (envelope.type !== 'souq.order.confirmed.v1') return;

      const { userId, orderId } = envelope.data ?? {};
      for (const line of envelope.data?.items ?? []) {
        // Idempotent by construction: the record's id is a hash of
        // (userId, productId), so a redelivery is an upsert onto itself.
        await repo.recordPurchase(userId, line.productId, orderId);
      }
    } catch (err) {
      // Throwing here would stall the partition. A missed purchase record
      // costs a customer their "verified" badge, which is worth far less than
      // blocking every order event behind it.
      logger.error({ err }, 'failed to record a purchase for verified-review status');
    }
  },
});

// ------------------------------------------------------------------ lifecycle

const shutdown = async (signal: string) => {
  logger.info({ signal }, 'shutting down');
  await consumer.disconnect().catch(() => {});
  await app.close();
  await mongo.close();
  process.exit(0);
};
process.on('SIGTERM', () => void shutdown('SIGTERM'));
process.on('SIGINT', () => void shutdown('SIGINT'));

const addr = process.env.SOUQ_HTTP_ADDR ?? '0.0.0.0:8090';
const [host, port] = addr.split(':');
await app.listen({ host: host || '0.0.0.0', port: Number(port ?? 8090) });

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
  return time + Array.from(bytes, (b) => CROCKFORD[b % 32]!).join('');
}
