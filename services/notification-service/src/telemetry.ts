import pino from 'pino';
import { Counter, Registry, collectDefaultMetrics } from 'prom-client';

export const logger = pino({
  level: process.env.SOUQ_LOG_LEVEL ?? 'info',
  base: {
    service: 'notification-service',
    version: process.env.SOUQ_VERSION ?? 'dev',
    env: process.env.SOUQ_ENV ?? 'local',
  },
  timestamp: () => `,"timestamp":"${new Date().toISOString()}"`,
  // Everything this service touches is PII: addresses, phone numbers, and
  // template params that carry names and order contents.
  redact: {
    paths: ['*.to', '*.email', '*.phone', '*.params', 'req.headers.authorization'],
    censor: '[REDACTED]',
  },
});

export const registry = new Registry();
collectDefaultMetrics({ register: registry });

export const notificationsSent = new Counter({
  name: 'souq_notifications_sent_total',
  help: 'Notifications delivered.',
  labelNames: ['channel', 'template'] as const,
  registers: [registry],
});

/**
 * Suppressed, by reason. `DUPLICATE` is the interesting one: it is the
 * three-layer dedup working, and a sudden rise means something upstream is
 * producing the same notification twice.
 */
export const notificationsSuppressed = new Counter({
  name: 'souq_notifications_suppressed_total',
  help: 'Notifications deliberately not sent.',
  labelNames: ['reason', 'channel'] as const,
  registers: [registry],
});

export const notificationsFailed = new Counter({
  name: 'souq_notifications_failed_total',
  help: 'Delivery failures, split by whether a retry is safe.',
  labelNames: ['channel', 'retriable'] as const,
  registers: [registry],
});
