-- order-service schema.
--
-- Owned exclusively by order-service. No other service connects to this
-- database (docs/CONTRACTS.md §6). Cross-service reads go over HTTP or are
-- materialised from Kafka.

BEGIN;

CREATE TABLE orders (
    id                  TEXT PRIMARY KEY,                       -- ord_<ULID>
    user_id             TEXT        NOT NULL,

    -- Mirrors internal/saga/model_test.go SagaStates exactly. The CHECK is not
    -- decoration: it is the last line of defence against a code path that
    -- invents a state the model never reasoned about.
    status              TEXT        NOT NULL
                        CHECK (status IN ('PENDING','STOCK_RESERVED','PAID',
                                          'STOCK_COMMITTED','CONFIRMED',
                                          'COMPENSATING','CANCELLED',
                                          'SHIPPED','DELIVERED','REFUNDED')),

    currency            CHAR(3)     NOT NULL,
    subtotal            BIGINT      NOT NULL,
    discount_total      BIGINT      NOT NULL DEFAULT 0,
    shipping_total      BIGINT      NOT NULL DEFAULT 0,
    tax_total           BIGINT      NOT NULL DEFAULT 0,
    total               BIGINT      NOT NULL CHECK (total >= 0),

    shipping_address    JSONB       NOT NULL,
    billing_address     JSONB,

    payment_id          TEXT,
    reservation_id      TEXT,
    payment_method_token TEXT       NOT NULL,

    cancellation_reason TEXT,
    failed_step         TEXT,
    tracking_number     TEXT,

    -- Pricing rule set the totals were computed against. Lets the order be
    -- re-priced identically at capture time even if a promotion has expired.
    rules_version       TEXT        NOT NULL,

    correlation_id      TEXT        NOT NULL,
    idempotency_key     TEXT        NOT NULL,

    placed_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Optimistic locking for the saga's own writes. Two concurrent event
    -- handlers for the same order (a redelivery racing a timeout sweep) must
    -- not both advance the state machine.
    version             INT         NOT NULL DEFAULT 0,

    -- A cancelled order must carry a reason. Enforced here rather than in Go
    -- because three different code paths cancel orders.
    CONSTRAINT cancelled_has_reason
        CHECK (status <> 'CANCELLED' OR cancellation_reason IS NOT NULL),

    -- A confirmed order must have both a payment and a reservation. This is
    -- the SQL restatement of ConsistentTerminalState from the state-space model.
    CONSTRAINT confirmed_is_settled
        CHECK (status <> 'CONFIRMED' OR (payment_id IS NOT NULL AND reservation_id IS NOT NULL))
);

CREATE INDEX orders_user_placed_idx ON orders (user_id, placed_at DESC);
CREATE INDEX orders_status_idx      ON orders (status) WHERE status NOT IN ('CONFIRMED','CANCELLED','DELIVERED','REFUNDED');
CREATE INDEX orders_correlation_idx ON orders (correlation_id);

CREATE TABLE order_items (
    order_id    TEXT        NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    line_no     INT         NOT NULL,
    sku         TEXT        NOT NULL,
    product_id  TEXT        NOT NULL,
    title       TEXT        NOT NULL,
    image_url   TEXT,
    quantity    INT         NOT NULL CHECK (quantity > 0),
    unit_price  BIGINT      NOT NULL,
    line_total  BIGINT      NOT NULL,
    PRIMARY KEY (order_id, line_no)
);

-- ---------------------------------------------------------------------------
-- Saga step tracking.
--
-- One row per (order, step). This is what the admin saga inspector renders and
-- what the timeout sweeper scans. Keeping it separate from `orders` means the
-- hot status column is not rewritten on every retry attempt.

CREATE TABLE saga_steps (
    order_id     TEXT        NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    step         TEXT        NOT NULL
                 CHECK (step IN ('RESERVE','AUTHORIZE','COMMIT','CAPTURE','RELEASE','VOID')),
    state        TEXT        NOT NULL DEFAULT 'PENDING'
                 CHECK (state IN ('PENDING','SENT','ACKED','FAILED','TIMED_OUT','SKIPPED')),
    attempts     INT         NOT NULL DEFAULT 0,
    last_event_id TEXT,
    error        TEXT,
    sent_at      TIMESTAMPTZ,
    acked_at     TIMESTAMPTZ,
    -- When the orchestrator should give up waiting. NULL for terminal steps
    -- and for the two steps past the point of no return, which retry forever
    -- rather than time out (docs/DESIGN-INVARIANTS.md §1).
    deadline_at  TIMESTAMPTZ,
    PRIMARY KEY (order_id, step)
);

-- The sweeper's only query. Partial index keeps it tiny even at 10M orders.
CREATE INDEX saga_steps_deadline_idx
    ON saga_steps (deadline_at)
    WHERE state = 'SENT' AND deadline_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Transactional outbox (docs/CONTRACTS.md §5.1, internal/eventbus/outbox_model_test.go).
--
-- Every event this service publishes is written here in the SAME transaction
-- as the business change. Publishing directly to Kafka after COMMIT loses
-- events on crash; the model proves it in two steps.

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

-- The relay's only query: oldest unpublished first. Partial index means the
-- index stays the size of the backlog, not the size of history.
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;

-- ---------------------------------------------------------------------------
-- Consumer inbox (docs/CONTRACTS.md §5.2).
--
-- Kafka is at-least-once. Without this table the relay's
-- crash-after-publish-before-mark path applies every side effect twice.

CREATE TABLE processed_events (
    event_id     TEXT        NOT NULL,
    consumer     TEXT        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);

CREATE INDEX processed_events_age_idx ON processed_events (processed_at);

-- ---------------------------------------------------------------------------
-- HTTP idempotency (docs/CONTRACTS.md §5.3).

CREATE TABLE idempotency_keys (
    key           TEXT PRIMARY KEY,
    user_id       TEXT        NOT NULL,
    endpoint      TEXT        NOT NULL,
    -- Hash of the canonicalised request body. Same key + different body is a
    -- client bug and must be rejected loudly, not silently replayed.
    request_hash  TEXT        NOT NULL,
    state         TEXT        NOT NULL DEFAULT 'IN_PROGRESS'
                  CHECK (state IN ('IN_PROGRESS','COMPLETED')),
    response_code INT,
    response_body JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours'
);

CREATE INDEX idempotency_expiry_idx ON idempotency_keys (expires_at);

COMMIT;
