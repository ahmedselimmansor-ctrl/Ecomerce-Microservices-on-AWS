-- inventory-service schema.
--
-- The stock_levels table is the hottest row in the platform. On a flash sale,
-- hundreds of concurrent checkouts hit one SKU in the same millisecond, and an
-- oversell there is the most expensive bug this system can ship: it is only
-- discovered at fulfilment, after the customer has been charged.
--
-- inventory-service internal/stock/model_test.go models four candidate strategies against
-- exactly that contention. Two of them are wrong. The one this schema is built
-- for is "AtomicUpdate": a single conditional statement, with the CHECK below
-- as a backstop.

BEGIN;

CREATE TABLE stock_levels (
    sku         TEXT PRIMARY KEY,
    product_id  TEXT        NOT NULL,

    -- Physically in the warehouse, including units already promised.
    on_hand     INT         NOT NULL DEFAULT 0 CHECK (on_hand >= 0),

    -- Promised to open reservations but not yet picked.
    reserved    INT         NOT NULL DEFAULT 0 CHECK (reserved >= 0),

    -- THE invariant, in the one place that cannot be bypassed by a code path,
    -- an ORM, a migration script, or a well-meaning manual UPDATE at 3am.
    -- Postgres evaluates it inside the same row latch as the write, so it
    -- holds under every interleaving — which is precisely the property
    -- NoOversell asserts in the model.
    CONSTRAINT no_oversell CHECK (reserved <= on_hand),

    -- Below this, notify merchandising. Not enforced, just reported.
    reorder_point INT       NOT NULL DEFAULT 0,

    -- Warehouse this row describes. Multi-warehouse is out of scope for v1 but
    -- the column exists so adding it later is not a table rewrite.
    location    TEXT        NOT NULL DEFAULT 'DEFAULT',

    status      TEXT        NOT NULL DEFAULT 'ACTIVE'
                CHECK (status IN ('ACTIVE','DISCONTINUED','SUSPENDED')),

    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    version     BIGINT      NOT NULL DEFAULT 0
);

CREATE INDEX stock_levels_product_idx ON stock_levels (product_id);
-- Powers the low-stock report without scanning the table.
CREATE INDEX stock_levels_low_idx
    ON stock_levels (sku)
    WHERE on_hand - reserved <= reorder_point;

-- ---------------------------------------------------------------------------
-- Reservations.
--
-- A reservation is a promise with an expiry. The TTL is what makes the system
-- self-healing: even if every other mechanism fails, an abandoned reservation
-- releases itself and the stock returns to sale.

CREATE TABLE reservations (
    id          TEXT PRIMARY KEY,                       -- rsv_<ULID>
    order_id    TEXT        NOT NULL,

    state       TEXT        NOT NULL
                CHECK (state IN ('RESERVED','COMMITTED','RELEASED','FAILED')),

    -- TOMBSTONE SUPPORT. A row may be created directly in RELEASED state,
    -- with no preceding RESERVED, when a Release command overtakes its Reserve
    -- (docs/DESIGN-INVARIANTS.md §2). Without it, the late Reserve would create a
    -- reservation nobody will ever release and the saga would never terminate.
    -- This column records which happened, for the event payload and for
    -- anyone reading the table later and wondering.
    was_tombstone BOOLEAN   NOT NULL DEFAULT FALSE,

    reason_code TEXT,

    expires_at  TIMESTAMPTZ,     -- NULL once terminal
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT open_reservation_has_expiry
        CHECK (state <> 'RESERVED' OR expires_at IS NOT NULL)
);

-- One reservation per order. This is a second, independent guard against
-- creating two reservations for one order if a Reserve command is redelivered
-- with a different reservationId after a saga restart.
CREATE UNIQUE INDEX reservations_order_uniq ON reservations (order_id);

-- The sweeper's only query.
CREATE INDEX reservations_expiry_idx
    ON reservations (expires_at)
    WHERE state = 'RESERVED';

CREATE TABLE reservation_items (
    reservation_id TEXT NOT NULL REFERENCES reservations(id) ON DELETE CASCADE,
    sku            TEXT NOT NULL,
    quantity       INT  NOT NULL CHECK (quantity > 0),
    PRIMARY KEY (reservation_id, sku)
);

CREATE INDEX reservation_items_sku_idx ON reservation_items (sku);

-- ---------------------------------------------------------------------------
-- Stock ledger.
--
-- Append-only. Every change to on_hand or reserved writes a row here, so the
-- current stock_levels values can always be re-derived and any discrepancy
-- between the system and a physical count can be traced to the movement that
-- caused it. Finance and the warehouse both reconcile against this table.

CREATE TABLE stock_ledger (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sku            TEXT        NOT NULL,
    reservation_id TEXT,
    order_id       TEXT,
    movement       TEXT        NOT NULL
                   CHECK (movement IN ('RESERVATION','RELEASE','COMMIT','RESTOCK',
                                       'ADJUSTMENT','RETURN','SHRINKAGE')),
    quantity       INT         NOT NULL,     -- signed
    on_hand_after  INT         NOT NULL,
    reserved_after INT         NOT NULL,
    actor          TEXT        NOT NULL DEFAULT 'system',
    note           TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX stock_ledger_sku_time_idx ON stock_ledger (sku, created_at DESC);
CREATE INDEX stock_ledger_order_idx    ON stock_ledger (order_id) WHERE order_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Outbox and inbox — identical contract to every other service
-- (docs/CONTRACTS.md §5.1, §5.2).

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

CREATE INDEX processed_events_age_idx ON processed_events (processed_at);

COMMIT;
