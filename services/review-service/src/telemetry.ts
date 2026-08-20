import pino from 'pino';
import { Counter, Histogram, Registry, collectDefaultMetrics } from 'prom-client';

export const logger = pino({
  level: process.env.SOUQ_LOG_LEVEL ?? 'info',
  base: {
    service: 'review-service',
    version: process.env.SOUQ_VERSION ?? 'dev',
    env: process.env.SOUQ_ENV ?? 'local',
  },
  timestamp: () => `,"timestamp":"${new Date().toISOString()}"`,
  // A review body is user-generated content and an email address is PII.
  // pino logs whole objects by default.
  redact: {
    paths: ['req.headers.authorization', 'req.headers.cookie', '*.email', '*.body'],
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
  buckets: [0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1],
  registers: [registry],
});

export const reviewsSubmitted = new Counter({
  name: 'souq_reviews_submitted_total',
  help: 'Reviews submitted, split by whether the purchase could be verified.',
  labelNames: ['verified'] as const,
  registers: [registry],
});

/**
 * Reviews auto-flagged for a human. A spike usually means a spam campaign, not
 * a sudden change in how customers write.
 */
export const reviewsFlagged = new Counter({
  name: 'souq_reviews_flagged_total',
  help: 'Reviews flagged by the auto-moderation heuristics.',
  labelNames: ['flag'] as const,
  registers: [registry],
});

export const moderationQueueDepth = new Counter({
  name: 'souq_reviews_moderated_total',
  help: 'Moderation decisions.',
  labelNames: ['decision'] as const,
  registers: [registry],
});
