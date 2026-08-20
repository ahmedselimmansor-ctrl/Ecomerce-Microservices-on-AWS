-- notification-service schema for Cassandra / Amazon Keyspaces.
--
-- Every table here is designed around the query that reads it, which is how
-- Cassandra has to be modelled: there are no joins and a secondary index on a
-- high-cardinality column is a distributed scan wearing a disguise.

CREATE KEYSPACE IF NOT EXISTS notifications
  WITH REPLICATION = {'class': 'SimpleStrategy', 'replication_factor': 1};
-- In Keyspaces the replication is managed by AWS and this clause is ignored;
-- it is here so the same file works against a local Cassandra.

USE notifications;

-- Query: "what have we sent this customer, newest first?"
-- One partition per user, clustered descending on time, so the answer is a
-- single sequential read with no sort.
CREATE TABLE IF NOT EXISTS delivery_log (
    user_id         text,
    sent_at         timestamp,
    notification_id uuid,
    channel         text,
    template        text,
    status          text,       -- SENT | SUPPRESSED | FAILED
    dedupe_key      text,
    provider_id     text,
    error           text,
    PRIMARY KEY ((user_id), sent_at, notification_id)
) WITH CLUSTERING ORDER BY (sent_at DESC, notification_id ASC)
  -- 90 days. Delivery history is operational, not archival: it answers
  -- support questions and nothing else needs it.
  AND default_time_to_live = 7776000
  AND comment = 'Append-only delivery history, partitioned by user';

-- Query: "have we already sent this exact notification?"
-- The partition key IS the idempotency mechanism. Written with a lightweight
-- transaction (IF NOT EXISTS) so two concurrent producers cannot both win.
CREATE TABLE IF NOT EXISTS dedupe (
    dedupe_key text PRIMARY KEY,
    sent_at    timestamp,
    channel    text
) WITH default_time_to_live = 2592000   -- 30 days
  AND comment = 'Claim-before-send idempotency keys. Written with LWT.';

-- Query: "is this address suppressed?"
-- Populated from SES bounce and complaint notifications via SNS. Sending to a
-- hard-bounced address destroys the sending domain's reputation, which then
-- delivers everyone else's mail to spam.
CREATE TABLE IF NOT EXISTS suppression_list (
    address    text PRIMARY KEY,
    reason     text,            -- HARD_BOUNCE | COMPLAINT | MANUAL
    added_at   timestamp,
    detail     text
) WITH comment = 'Never send to these. No TTL: a hard bounce is permanent.';

-- Query: "what has this user done recently?" — the activity feed on the
-- account page, and the input to the win-back campaign.
CREATE TABLE IF NOT EXISTS user_activity_feed (
    user_id     text,
    occurred_at timestamp,
    event_id    uuid,
    event_type  text,
    order_id    text,
    product_id  text,
    payload     text,           -- JSON, kept opaque
    PRIMARY KEY ((user_id), occurred_at, event_id)
) WITH CLUSTERING ORDER BY (occurred_at DESC, event_id ASC)
  AND default_time_to_live = 15552000   -- 180 days
  AND comment = 'Denormalised activity stream from souq.user.activity.v1';

-- Query: "which notifications are scheduled to go out now?"
-- Bucketed by hour so the scan is bounded: without the bucket this would be
-- an ALLOW FILTERING scan across the whole cluster every minute.
CREATE TABLE IF NOT EXISTS scheduled (
    send_hour   text,           -- '2026-08-17T10'
    send_at     timestamp,
    dedupe_key  text,
    payload     text,
    PRIMARY KEY ((send_hour), send_at, dedupe_key)
) WITH CLUSTERING ORDER BY (send_at ASC, dedupe_key ASC)
  AND default_time_to_live = 604800
  AND comment = 'Delayed sends, bucketed by hour to bound the sweep';
