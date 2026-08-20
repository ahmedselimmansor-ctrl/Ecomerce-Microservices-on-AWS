import Fastify from 'fastify';
import { Client as CassandraClient } from 'cassandra-driver';
import { SESv2Client } from '@aws-sdk/client-sesv2';
import { SNSClient } from '@aws-sdk/client-sns';
import { Kafka } from 'kafkajs';
import { z } from 'zod';
import { randomUUID } from 'node:crypto';

import { DeliveryService, type NotificationCommand, type Recipient } from './delivery.js';
import { templateExists } from './templates.js';
import { logger, registry, notificationsFailed } from './telemetry.js';

const required = (key: string): string => {
  const v = process.env[key];
  if (!v) throw new Error(`required environment variable ${key} is not set`);
  return v;
};

const cassandra = new CassandraClient({
  contactPoints: required('SOUQ_CASSANDRA_HOSTS').split(','),
  localDataCenter: process.env.SOUQ_CASSANDRA_DC ?? 'local',
  keyspace: process.env.SOUQ_CASSANDRA_KEYSPACE ?? 'notifications',
  socketOptions: { connectTimeout: 10_000, readTimeout: 5_000 },
  // LOCAL_QUORUM for the dedupe LWT to mean anything. ONE would let two pods
  // both believe they claimed the key.
  queryOptions: { consistency: 6 /* LOCAL_QUORUM */ },
});
await cassandra.connect();

const region = process.env.AWS_DEFAULT_REGION ?? 'eu-west-1';
const endpoint = process.env.SOUQ_SES_ENDPOINT; // LocalStack locally, unset in AWS
const ses = new SESv2Client({ region, ...(endpoint ? { endpoint } : {}) });
const sns = new SNSClient({ region, ...(endpoint ? { endpoint } : {}) });

/**
 * Recipient lookup against identity-service.
 *
 * Cached briefly: a burst of order events for one customer would otherwise
 * issue one lookup per notification, and identity-service is on the critical
 * path for every other service's auth.
 */
const recipientCache = new Map<string, { r: Recipient | null; at: number }>();

async function loadRecipient(userId: string): Promise<Recipient | null> {
  const hit = recipientCache.get(userId);
  if (hit && Date.now() - hit.at < 60_000) return hit.r;

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 2_000);
  try {
    const res = await fetch(
      `${process.env.SOUQ_IDENTITY_URL ?? 'http://identity-service:8081'}/internal/users/${userId}/contact`,
      { signal: controller.signal },
    );
    const r = res.ok ? ((await res.json()) as Recipient) : null;
    recipientCache.set(userId, { r, at: Date.now() });
    return r;
  } catch {
    // Serve stale rather than dropping a transactional notification. An
    // out-of-date email address is better than not telling someone their
    // payment failed.
    return hit?.r ?? null;
  } finally {
    clearTimeout(timer);
  }
}

const delivery = new DeliveryService(
  cassandra, ses, sns, loadRecipient,
  process.env.SOUQ_FROM_ADDRESS ?? 'no-reply@souq.dev',
);

// --------------------------------------------------------------------- http

const app = Fastify({
  logger,
  genReqId: (req) => (req.headers['x-request-id'] as string) ?? randomUUID(),
  trustProxy: true,
});

app.get('/health/live', async () => ({ status: 'UP' }));

app.get('/health/ready', async (_req, reply) => {
  try {
    await cassandra.execute('SELECT release_version FROM system.local');
    return { status: 'UP' };
  } catch {
    return reply.status(503).send({ status: 'DOWN', cassandra: 'unreachable' });
  }
});

app.get('/metrics', async (_req, reply) => {
  reply.type(registry.contentType);
  return registry.metrics();
});

/** What we have sent a customer. Used by support and by the account page. */
app.get('/v1/users/:userId/notifications', async (req) => {
  const { userId } = z.object({ userId: z.string() }).parse(req.params);
  const { limit } = z.object({ limit: z.coerce.number().int().min(1).max(100).default(20) })
    .parse(req.query);

  // One partition, clustered newest-first, so this is a single sequential
  // read with no sort.
  const rs = await cassandra.execute(
    `SELECT sent_at, channel, template, status, provider_id, error
       FROM delivery_log WHERE user_id = ? LIMIT ?`,
    [userId, limit],
    { prepare: true },
  );

  return {
    items: rs.rows.map((r) => ({
      sentAt: r['sent_at'], channel: r['channel'], template: r['template'],
      status: r['status'], providerId: r['provider_id'], error: r['error'],
    })),
  };
});

/**
 * SES bounce and complaint notifications, delivered via SNS.
 *
 * Populating the suppression list matters more than it looks: continuing to
 * send to a hard-bounced address destroys the sending domain's reputation,
 * which then delivers everyone else's mail to spam.
 */
app.post('/v1/webhooks/ses', async (req, reply) => {
  const body = req.body as Record<string, unknown>;

  // SNS subscription confirmation. Logged rather than auto-confirmed: an
  // endpoint that confirms any subscription can be subscribed to by anyone.
  if (body?.Type === 'SubscriptionConfirmation') {
    logger.warn({ subscribeUrl: body.SubscribeURL },
      'SNS subscription confirmation received — confirm it deliberately, not automatically');
    return reply.status(200).send({ status: 'received' });
  }

  try {
    const message = JSON.parse(String(body?.Message ?? '{}'));
    const type = message.notificationType ?? message.eventType;

    if (type === 'Bounce' && message.bounce?.bounceType === 'Permanent') {
      for (const r of message.bounce.bouncedRecipients ?? []) {
        await cassandra.execute(
          `INSERT INTO suppression_list (address, reason, added_at, detail)
           VALUES (?, 'HARD_BOUNCE', toTimestamp(now()), ?)`,
          [r.emailAddress, r.diagnosticCode ?? ''],
          { prepare: true },
        );
        logger.warn({ reason: 'HARD_BOUNCE' }, 'address added to the suppression list');
      }
    } else if (type === 'Complaint') {
      for (const r of message.complaint?.complainedRecipients ?? []) {
        await cassandra.execute(
          `INSERT INTO suppression_list (address, reason, added_at, detail)
           VALUES (?, 'COMPLAINT', toTimestamp(now()), ?)`,
          [r.emailAddress, message.complaint.complaintFeedbackType ?? ''],
          { prepare: true },
        );
      }
    }
  } catch (err) {
    logger.error({ err }, 'could not process an SES notification');
  }

  // Always 200. SNS retries a non-200 aggressively, and a malformed
  // notification is not something a retry will fix.
  return reply.status(200).send({ status: 'ok' });
});

// ----------------------------------------------------------------- consumer

const kafka = new Kafka({
  clientId: 'notification-service',
  brokers: required('SOUQ_KAFKA_BROKERS').split(','),
});
const consumer = kafka.consumer({ groupId: 'notification-service.commands' });

await consumer.connect();
await consumer.subscribe({ topic: 'souq.notification.commands.v1', fromBeginning: false });
// Order events are turned into notifications here rather than every service
// having to remember to emit a notify command.
await consumer.subscribe({ topic: 'souq.order.events.v1', fromBeginning: false });
await consumer.subscribe({ topic: 'souq.payment.events.v1', fromBeginning: false });

/** Maps an event onto the notification it should produce, if any. */
function toCommand(type: string, data: Record<string, any>): NotificationCommand | null {
  const base = { channel: 'EMAIL' as const, locale: 'en-GB', userId: data.userId };

  switch (type) {
    case 'souq.order.confirmed.v1':
      return { ...base, template: 'order_confirmed',
        params: { orderRef: data.orderId, total: data.total?.amount, name: 'there' },
        // Deterministic: two producers deciding the same customer should be
        // told the same thing collapse into one send.
        dedupeKey: `order_confirmed:${data.orderId}` };

    case 'souq.order.cancelled.v1':
      return { ...base, template: 'order_cancelled',
        params: { orderRef: data.orderId, reason: data.reasonCode, name: 'there' },
        dedupeKey: `order_cancelled:${data.orderId}` };

    case 'souq.order.shipped.v1':
      return { ...base, template: 'order_shipped',
        params: { orderRef: data.orderId, carrier: data.carrier, trackingNumber: data.trackingNumber, name: 'there' },
        dedupeKey: `order_shipped:${data.orderId}` };

    case 'souq.payment.failed.v1':
      // Only tell the customer about a HARD decline. A retriable provider
      // outage is going to be retried and usually succeeds; emailing about it
      // is alarming and wrong.
      if (data.retriable) return null;
      return { ...base, template: 'payment_failed',
        params: { orderRef: data.orderId, name: 'there' },
        dedupeKey: `payment_failed:${data.paymentId}` };

    case 'souq.payment.refunded.v1':
      return { ...base, template: 'payment_refunded',
        params: { orderRef: data.orderId, amount: data.amount?.amount, name: 'there' },
        dedupeKey: `payment_refunded:${data.refundId}` };

    case 'souq.notify.v1':
      return {
        channel: data.channel, userId: data.userId, to: data.to,
        template: data.template, locale: data.locale ?? 'en-GB',
        params: data.params ?? {}, dedupeKey: data.dedupeKey, sendAfter: data.sendAfter,
      };
  }
  return null;
}

void consumer.run({
  eachMessage: async ({ message }) => {
    if (!message.value) return;

    let cmd: NotificationCommand | null = null;
    try {
      const envelope = JSON.parse(message.value.toString());
      cmd = toCommand(envelope.type, envelope.data ?? {});
    } catch (err) {
      logger.error({ err }, 'malformed event; skipping');
      return;
    }
    if (!cmd) return;

    if (!templateExists(cmd.template)) {
      logger.error({ template: cmd.template }, 'unknown template; skipping');
      return;
    }

    const outcome = await delivery.deliver(cmd);

    if (outcome.status === 'FAILED') {
      notificationsFailed.inc({ channel: cmd.channel, retriable: String(outcome.retriable) });
      if (outcome.retriable) {
        // Throwing makes kafkajs retry the message. Safe only because
        // `deliver` released the dedupe claim on a retriable failure, having
        // established the provider never accepted it.
        throw new Error(`retriable delivery failure: ${outcome.reason}`);
      }
    }
  },
});

// ---------------------------------------------------------------- lifecycle

const shutdown = async (signal: string) => {
  logger.info({ signal }, 'shutting down');
  await consumer.disconnect().catch(() => {});
  await app.close();
  await cassandra.shutdown().catch(() => {});
  process.exit(0);
};
process.on('SIGTERM', () => void shutdown('SIGTERM'));
process.on('SIGINT', () => void shutdown('SIGINT'));

const addr = process.env.SOUQ_HTTP_ADDR ?? '0.0.0.0:8091';
const [host, port] = addr.split(':');
await app.listen({ host: host || '0.0.0.0', port: Number(port ?? 8091) });
