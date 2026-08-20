import type { Client as CassandraClient } from 'cassandra-driver';
import { SESv2Client, SendEmailCommand } from '@aws-sdk/client-sesv2';
import { SNSClient, PublishCommand } from '@aws-sdk/client-sns';
import { logger, notificationsSent, notificationsSuppressed } from './telemetry.js';
import { render, type TemplateName } from './templates.js';

/**
 * Notification delivery.
 *
 * Cassandra (Amazon Keyspaces) rather than Postgres, because the workload is a
 * textbook fit: wide, append-only, time-series, always read by partition key
 * ("what did we send this customer?"). It is also the one datastore here where
 * losing a row is survivable — a missing delivery-log entry is an audit gap,
 * not a lost order.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * The thing this file is really about: not sending the same email twice.
 * ─────────────────────────────────────────────────────────────────────────
 *
 * There are THREE independent layers, because each covers a gap the others
 * cannot, and internal/eventbus/outbox_model_test.go shows why one is never enough:
 *
 *  1. The Kafka inbox (`processed_events`). Catches a redelivery of the same
 *     event id. Does NOT catch two different services independently deciding
 *     the customer should be told the same thing.
 *
 *  2. `dedupe` keyed on the caller's dedupeKey, with a Cassandra
 *     lightweight transaction (IF NOT EXISTS). Catches (1) and the
 *     two-producers case. Does NOT catch a crash between "SES accepted" and
 *     "we recorded it".
 *
 *  3. SES's own idempotency is nonexistent, so layer 3 is ordering: claim the
 *     dedupe key BEFORE calling SES, not after. A crash then means the
 *     notification is silently dropped rather than sent twice.
 *
 * That last trade is deliberate and worth stating: for notifications we choose
 * at-most-once over at-least-once. A customer who does not get a "your order
 * shipped" email will check the app. A customer who gets four of them at 3am
 * unsubscribes. Payments make the opposite choice, for obvious reasons.
 */

export type Channel = 'EMAIL' | 'SMS' | 'PUSH' | 'WEBHOOK';

export interface NotificationCommand {
  channel: Channel;
  userId?: string;
  to?: string;
  template: TemplateName;
  locale: string;
  params: Record<string, unknown>;
  dedupeKey: string;
  sendAfter?: string;
}

export interface Recipient {
  userId: string;
  email: string | null;
  phone: string | null;
  locale: string;
  /** Per-channel opt-outs. Ignoring these is a legal problem, not a UX one. */
  emailOptOut: boolean;
  smsOptOut: boolean;
  pushOptOut: boolean;
}

export type DeliveryOutcome =
  | { status: 'SENT'; providerId: string }
  | { status: 'SUPPRESSED'; reason: SuppressionReason }
  | { status: 'FAILED'; reason: string; retriable: boolean };

export type SuppressionReason =
  | 'DUPLICATE'
  | 'OPTED_OUT'
  | 'NO_ADDRESS'
  | 'HARD_BOUNCED'
  | 'QUIET_HOURS'
  | 'RATE_LIMITED';

/**
 * Templates that ignore opt-out and quiet hours.
 *
 * Marketing consent does not cover "your payment failed" or "your order was
 * cancelled" — those are transactional, the customer needs them to act, and
 * withholding one because they unsubscribed from a newsletter is worse for
 * them than the interruption. Everything not on this list respects opt-out.
 */
const TRANSACTIONAL: ReadonlySet<TemplateName> = new Set([
  'order_confirmed',
  'order_cancelled',
  'payment_failed',
  'payment_refunded',
  'order_shipped',
  'order_delivered',
  'password_reset',
  'email_verification',
  'security_alert',
]);

export class DeliveryService {
  constructor(
    private readonly cassandra: CassandraClient,
    private readonly ses: SESv2Client,
    private readonly sns: SNSClient,
    private readonly loadRecipient: (userId: string) => Promise<Recipient | null>,
    private readonly fromAddress: string,
  ) {}

  async deliver(cmd: NotificationCommand): Promise<DeliveryOutcome> {
    // ---- LAYER 2: claim the dedupe key, before anything is sent ----
    //
    // A Cassandra lightweight transaction. It costs a Paxos round and is
    // roughly four times slower than a plain insert, which is precisely the
    // situation LWTs exist for: a correctness requirement that a normal
    // quorum write cannot express.
    const claimed = await this.claimDedupeKey(cmd.dedupeKey, cmd.channel);
    if (!claimed) {
      notificationsSuppressed.inc({ reason: 'DUPLICATE', channel: cmd.channel });
      logger.debug({ dedupeKey: cmd.dedupeKey }, 'suppressed a duplicate notification');
      return { status: 'SUPPRESSED', reason: 'DUPLICATE' };
    }

    const recipient = cmd.userId ? await this.loadRecipient(cmd.userId) : null;
    const transactional = TRANSACTIONAL.has(cmd.template);

    const address = cmd.to ?? this.addressFor(cmd.channel, recipient);
    if (!address) {
      await this.log(cmd, recipient, 'SUPPRESSED', null, 'NO_ADDRESS');
      notificationsSuppressed.inc({ reason: 'NO_ADDRESS', channel: cmd.channel });
      return { status: 'SUPPRESSED', reason: 'NO_ADDRESS' };
    }

    if (!transactional && recipient && this.optedOut(cmd.channel, recipient)) {
      await this.log(cmd, recipient, 'SUPPRESSED', null, 'OPTED_OUT');
      notificationsSuppressed.inc({ reason: 'OPTED_OUT', channel: cmd.channel });
      return { status: 'SUPPRESSED', reason: 'OPTED_OUT' };
    }

    // Quiet hours: no marketing SMS between 22:00 and 08:00 in the
    // recipient's locale. An SMS at 3am is a complaint and, in several
    // markets, a fine.
    if (!transactional && cmd.channel === 'SMS' && this.inQuietHours(recipient?.locale ?? cmd.locale)) {
      await this.log(cmd, recipient, 'SUPPRESSED', null, 'QUIET_HOURS');
      notificationsSuppressed.inc({ reason: 'QUIET_HOURS', channel: cmd.channel });
      return { status: 'SUPPRESSED', reason: 'QUIET_HOURS' };
    }

    if (await this.hasHardBounced(address)) {
      // Continuing to send to a hard-bounced address destroys the sending
      // domain's reputation, which then delivers everyone else's mail to spam.
      await this.log(cmd, recipient, 'SUPPRESSED', null, 'HARD_BOUNCED');
      notificationsSuppressed.inc({ reason: 'HARD_BOUNCED', channel: cmd.channel });
      return { status: 'SUPPRESSED', reason: 'HARD_BOUNCED' };
    }

    const locale = recipient?.locale ?? cmd.locale;
    const content = render(cmd.template, locale, cmd.params);

    try {
      const providerId = await this.send(cmd.channel, address, content);
      await this.log(cmd, recipient, 'SENT', providerId, null);
      notificationsSent.inc({ channel: cmd.channel, template: cmd.template });
      return { status: 'SENT', providerId };
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      const retriable = this.isRetriable(err);

      await this.log(cmd, recipient, 'FAILED', null, message);

      if (retriable) {
        // Release the dedupe claim so a retry can proceed. Safe ONLY because
        // we know the provider never accepted it — a throw from the SDK on a
        // connection error means the request did not land. An ambiguous
        // failure (a timeout after the bytes went out) is treated as
        // non-retriable below, precisely so we do not send twice.
        await this.releaseDedupeKey(cmd.dedupeKey);
      }

      logger.error({ err: message, template: cmd.template, channel: cmd.channel, retriable },
        'notification delivery failed');
      return { status: 'FAILED', reason: message, retriable };
    }
  }

  // -------------------------------------------------------------------------

  /**
   * INSERT ... IF NOT EXISTS. Returns false when the key was already taken.
   *
   * `[applied]` is Cassandra's LWT result column. Reading it, rather than
   * assuming success, is the whole point — a plain INSERT would silently
   * overwrite and every duplicate would be sent.
   */
  private async claimDedupeKey(dedupeKey: string, channel: Channel): Promise<boolean> {
    const result = await this.cassandra.execute(
      `INSERT INTO dedupe (dedupe_key, sent_at, channel)
       VALUES (?, toTimestamp(now()), ?)
       IF NOT EXISTS`,
      [dedupeKey, channel],
      { prepare: true },
    );
    return result.rows[0]?.['[applied]'] === true;
  }

  private async releaseDedupeKey(dedupeKey: string): Promise<void> {
    try {
      await this.cassandra.execute(
        `DELETE FROM dedupe WHERE dedupe_key = ?`, [dedupeKey], { prepare: true },
      );
    } catch (err) {
      // Failing to release means the retry is suppressed as a duplicate: the
      // notification is dropped. Log it loudly — at-most-once is the chosen
      // trade, but a silent drop still deserves a line.
      logger.warn({ dedupeKey, err }, 'could not release a dedupe claim; the retry will be suppressed');
    }
  }

  private async send(channel: Channel, address: string, content: { subject: string; html: string; text: string }): Promise<string> {
    switch (channel) {
      case 'EMAIL': {
        const res = await this.ses.send(new SendEmailCommand({
          FromEmailAddress: this.fromAddress,
          Destination: { ToAddresses: [address] },
          Content: {
            Simple: {
              Subject: { Data: content.subject, Charset: 'UTF-8' },
              Body: {
                Html: { Data: content.html, Charset: 'UTF-8' },
                // Always both. A text/plain part improves deliverability and
                // is what a screen reader and a smartwatch actually render.
                Text: { Data: content.text, Charset: 'UTF-8' },
              },
            },
          },
          // Lets SES bounce and complaint notifications be attributed back to
          // a specific campaign in the reputation dashboard.
          ConfigurationSetName: 'souq-transactional',
        }));
        return res.MessageId ?? 'unknown';
      }

      case 'SMS': {
        const res = await this.sns.send(new PublishCommand({
          PhoneNumber: address,
          Message: content.text,
          MessageAttributes: {
            'AWS.SNS.SMS.SMSType': { DataType: 'String', StringValue: 'Transactional' },
            'AWS.SNS.SMS.SenderID': { DataType: 'String', StringValue: 'SOUQ' },
          },
        }));
        return res.MessageId ?? 'unknown';
      }

      case 'PUSH':
      case 'WEBHOOK':
        throw new Error(`${channel} delivery is not implemented`);
    }
  }

  /** Append-only delivery log. Partitioned by user, newest first. */
  private async log(
    cmd: NotificationCommand,
    recipient: Recipient | null,
    status: string,
    providerId: string | null,
    error: string | null,
  ): Promise<void> {
    try {
      await this.cassandra.execute(
        `INSERT INTO delivery_log
           (user_id, sent_at, notification_id, channel, template, status, dedupe_key, provider_id, error)
         VALUES (?, toTimestamp(now()), uuid(), ?, ?, ?, ?, ?, ?)`,
        [
          recipient?.userId ?? cmd.userId ?? 'anonymous',
          cmd.channel, cmd.template, status, cmd.dedupeKey, providerId, error,
        ],
        { prepare: true },
      );
    } catch (err) {
      // The log is an audit trail, not the delivery mechanism. Failing to
      // write it must never fail a notification the customer needs.
      logger.warn({ err, dedupeKey: cmd.dedupeKey }, 'could not write the delivery log entry');
    }
  }

  private addressFor(channel: Channel, r: Recipient | null): string | null {
    if (!r) return null;
    return channel === 'EMAIL' ? r.email : channel === 'SMS' ? r.phone : null;
  }

  private optedOut(channel: Channel, r: Recipient): boolean {
    return channel === 'EMAIL' ? r.emailOptOut
         : channel === 'SMS' ? r.smsOptOut
         : channel === 'PUSH' ? r.pushOptOut
         : false;
  }

  private inQuietHours(locale: string): boolean {
    // Egypt is UTC+2 with no DST, which keeps this honest for the primary
    // market. Other locales fall back to UTC and are deliberately
    // conservative — a suppressed marketing SMS costs nothing.
    const offsetHours = locale.endsWith('-EG') || locale === 'ar' ? 2 : 0;
    const hour = (new Date().getUTCHours() + offsetHours + 24) % 24;
    return hour >= 22 || hour < 8;
  }

  private async hasHardBounced(address: string): Promise<boolean> {
    const res = await this.cassandra.execute(
      `SELECT address FROM suppression_list WHERE address = ?`, [address], { prepare: true },
    );
    return res.rowLength > 0;
  }

  /**
   * Retriable means "we are certain the provider did not accept it".
   *
   * A throttle or a connection refusal qualifies. A timeout does NOT: the
   * request may have landed, and retrying would send a second email. Ambiguity
   * resolves against sending.
   */
  private isRetriable(err: unknown): boolean {
    if (!(err instanceof Error)) return false;
    const name = (err as { name?: string }).name ?? '';
    const message = err.message.toLowerCase();

    if (name === 'ThrottlingException' || name === 'TooManyRequestsException') return true;
    if (message.includes('econnrefused') || message.includes('enotfound')) return true;
    if (message.includes('timeout') || message.includes('etimedout')) return false; // ambiguous

    const status = (err as { $metadata?: { httpStatusCode?: number } }).$metadata?.httpStatusCode;
    return status !== undefined && status >= 500;
  }
}
