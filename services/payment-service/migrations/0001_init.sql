-- payment-service schema.
--
-- The design of this table is the direct output of payment-service internal/psp/paymob_test.go.
-- Two columns exist because the model found a double-charge without them:
--
--   psp_idempotency_key  derived deterministically from our key, computed ONCE
--                        and stored BEFORE the provider call. Without this the
--                        crash-and-reap path charges the customer twice
--                        (FINDINGS §4) — an atomic INSERT and a well-behaved
--                        reaper are not enough on their own.
--
--   state                tracks the gap between "we asked the PSP" and "we know
--                        what it said". A row can be CHARGING with no answer,
--                        and recovery has to reconcile rather than guess.

BEGIN;

CREATE TABLE payments (
    id          TEXT PRIMARY KEY,                       -- pay_<ULID>
    order_id    TEXT        NOT NULL,
    user_id     TEXT        NOT NULL,

    state       TEXT        NOT NULL
                CHECK (state IN ('PENDING','AUTHORIZING','AUTHORIZED','CAPTURING',
                                 'CAPTURED','VOIDED','FAILED','REFUNDED','PARTIALLY_REFUNDED')),

    currency    CHAR(3)     NOT NULL,
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    captured_amount BIGINT  NOT NULL DEFAULT 0 CHECK (captured_amount >= 0),
    refunded_amount BIGINT  NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0),

    -- Cannot capture more than authorised, cannot refund more than captured.
    -- Both are enforced here because both are money leaving the business, and
    -- an application-layer check can be bypassed by the next code path someone
    -- writes at 2am.
    CONSTRAINT capture_within_authorization CHECK (captured_amount <= amount),
    CONSTRAINT refund_within_capture        CHECK (refunded_amount <= captured_amount),

    provider    TEXT        NOT NULL CHECK (provider IN ('stripe','adyen','checkout','mock')),

    -- PSP-side token. The raw PAN never enters this platform: the browser
    -- posts card details straight to the provider and we only ever hold this.
    -- That is what keeps the whole system out of PCI-DSS scope beyond SAQ-A.
    payment_method_token TEXT NOT NULL,

    -- THE column. sha256(idempotency_key || order_id), computed once at
    -- creation and never regenerated. Presented to the provider on every
    -- attempt, including attempts made by a different pod after a crash.
    psp_idempotency_key TEXT NOT NULL UNIQUE,

    psp_authorization_id TEXT,
    psp_capture_id       TEXT,
    auth_code            TEXT,

    -- Authorisations expire, usually in 7 days. A capture after this fails at
    -- the provider and needs a fresh authorisation, so the reconciler watches
    -- it rather than discovering it at capture time.
    authorization_expires_at TIMESTAMPTZ,

    decline_code TEXT,
    reason_code  TEXT,
    retriable    BOOLEAN,

    correlation_id TEXT     NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    version      INT         NOT NULL DEFAULT 0,

    -- One payment per order. A saga restart that mints a new paymentId must
    -- not be able to create a second charge for the same order.
    CONSTRAINT payments_order_uniq UNIQUE (order_id)
);

CREATE INDEX payments_state_idx ON payments (state)
    WHERE state IN ('AUTHORIZING','CAPTURING','AUTHORIZED');
CREATE INDEX payments_user_idx  ON payments (user_id, created_at DESC);
CREATE INDEX payments_auth_expiry_idx ON payments (authorization_expires_at)
    WHERE state = 'AUTHORIZED' AND authorization_expires_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Attempt log.
--
-- Append-only, one row per call to the provider. This is what a support agent
-- and a reconciliation job read when the question is "did we actually charge
-- them?" — a question the `payments` row alone cannot always answer, because
-- the answer may have been lost in transit.

CREATE TABLE payment_attempts (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id   TEXT        NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
    operation    TEXT        NOT NULL CHECK (operation IN ('AUTHORIZE','CAPTURE','VOID','REFUND')),
    attempt_no   INT         NOT NULL,
    psp_idempotency_key TEXT NOT NULL,
    outcome      TEXT        NOT NULL CHECK (outcome IN ('SUCCESS','DECLINED','ERROR','TIMEOUT','UNKNOWN')),
    psp_reference TEXT,
    http_status  INT,
    latency_ms   INT,
    -- Provider response with card data and PII already stripped by the
    -- adapter. Never store a raw provider payload: it can contain the last
    -- four, the cardholder name, and a full billing address.
    redacted_response JSONB,
    error_message TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX payment_attempts_payment_idx ON payment_attempts (payment_id, created_at);
-- The reconciler's query: attempts whose outcome we never learned.
CREATE INDEX payment_attempts_unknown_idx ON payment_attempts (created_at)
    WHERE outcome IN ('TIMEOUT','UNKNOWN');

CREATE TABLE refunds (
    id          TEXT PRIMARY KEY,
    payment_id  TEXT        NOT NULL REFERENCES payments(id),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    currency    CHAR(3)     NOT NULL,
    reason_code TEXT        NOT NULL,
    psp_refund_id TEXT,
    psp_idempotency_key TEXT NOT NULL UNIQUE,
    state       TEXT        NOT NULL CHECK (state IN ('PENDING','SUCCEEDED','FAILED')),
    requested_by TEXT       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Double-entry ledger.
--
-- Finance reconciles against this, not against `payments`. Every movement of
-- money produces balanced debit and credit rows, so the books can be proved
-- to balance with a single query rather than by trusting application logic.

CREATE TABLE ledger_entries (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id  TEXT        NOT NULL,
    order_id    TEXT        NOT NULL,
    account     TEXT        NOT NULL
                CHECK (account IN ('customer_receivable','merchant_payable',
                                   'psp_clearing','revenue','refunds','fees')),
    direction   TEXT        NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    currency    CHAR(3)     NOT NULL,
    entry_group UUID        NOT NULL,      -- ties the two sides of one movement
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ledger_group_idx   ON ledger_entries (entry_group);
CREATE INDEX ledger_payment_idx ON ledger_entries (payment_id);

-- The books balance iff this view is empty. Checked by a scheduled job and
-- alerted on; a non-empty result is a page, not a ticket.
CREATE VIEW unbalanced_entry_groups AS
SELECT entry_group,
       currency,
       sum(CASE WHEN direction = 'DEBIT' THEN amount ELSE -amount END) AS imbalance
  FROM ledger_entries
 GROUP BY entry_group, currency
HAVING sum(CASE WHEN direction = 'DEBIT' THEN amount ELSE -amount END) <> 0;

-- ---------------------------------------------------------------------------
-- Outbox / inbox / idempotency — identical contract to every other service.

CREATE TABLE outbox (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_type TEXT        NOT NULL,
    aggregate_id   TEXT        NOT NULL,
    event_id       TEXT        NOT NULL UNIQUE,
    event_type     TEXT        NOT NULL,
    topic          TEXT        NOT NULL,
    partition_key  TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    headers        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    attempts       INT         NOT NULL DEFAULT 0,
    last_error     TEXT
);
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;

CREATE TABLE processed_events (
    event_id     TEXT        NOT NULL,
    consumer     TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE TABLE idempotency_keys (
    key           TEXT PRIMARY KEY,
    user_id       TEXT        NOT NULL,
    endpoint      TEXT        NOT NULL,
    request_hash  TEXT        NOT NULL,
    state         TEXT        NOT NULL DEFAULT 'IN_PROGRESS'
                  CHECK (state IN ('IN_PROGRESS','COMPLETED')),
    response_code INT,
    response_body JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Longer than the 24h other services use. The reaper here is genuinely
    -- dangerous (FINDINGS §4): releasing a key whose owner crashed mid-charge
    -- allows a second attempt, and only the deterministic PSP key stops that
    -- becoming a second charge. A long TTL narrows the window where that
    -- second layer is load-bearing.
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '7 days'
);

COMMIT;
