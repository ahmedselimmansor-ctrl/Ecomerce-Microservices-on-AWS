-- catalog-service schema.
--
-- The source of truth for what SOUQ sells. Everything downstream — the search
-- index, the storefront read model, the recommendation item catalogue — is a
-- projection of the compacted Kafka topic this service produces, never a query
-- against these tables.

CREATE TABLE categories (
    id          TEXT PRIMARY KEY,
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    parent_id   TEXT REFERENCES categories(id),
    -- Materialised root-to-leaf path. Denormalised deliberately: the
    -- alternative is a recursive CTE on every product page, and the tree
    -- changes a few times a month while it is read millions of times a day.
    path        TEXT[]      NOT NULL,
    position    INT         NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A category cannot be its own parent. Deeper cycles are caught in the
    -- application; this catches the common typo.
    CONSTRAINT no_self_parent CHECK (parent_id IS DISTINCT FROM id)
);

CREATE INDEX categories_parent_idx ON categories (parent_id);
CREATE INDEX categories_path_idx   ON categories USING GIN (path);

CREATE TABLE products (
    id          TEXT PRIMARY KEY,                        -- prd_<ULID>
    slug        TEXT        NOT NULL UNIQUE,
    title       TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    brand       TEXT,
    category_id TEXT REFERENCES categories(id),
    status      TEXT        NOT NULL DEFAULT 'DRAFT'
                CHECK (status IN ('DRAFT','ACTIVE','ARCHIVED','DISCONTINUED')),
    attributes  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    images      JSONB       NOT NULL DEFAULT '[]'::jsonb,

    -- Denormalised from review-service via Kafka. EVENTUALLY CONSISTENT and
    -- display-only; the review service owns the truth.
    rating_average NUMERIC(2,1),
    rating_count   INT       NOT NULL DEFAULT 0,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    version     INT         NOT NULL DEFAULT 0,

    -- A product cannot go on sale without a category: the storefront's
    -- navigation, the search facets and the recommendation model all key off
    -- it, and an uncategorised ACTIVE product is invisible in all three.
    CONSTRAINT active_needs_category CHECK (status <> 'ACTIVE' OR category_id IS NOT NULL)
);

CREATE INDEX products_status_idx   ON products (status) WHERE status = 'ACTIVE';
CREATE INDEX products_category_idx ON products (category_id) WHERE status = 'ACTIVE';
CREATE INDEX products_brand_idx    ON products (brand) WHERE status = 'ACTIVE';
CREATE INDEX products_updated_idx  ON products (updated_at DESC);
-- Full-text fallback for when OpenSearch is unavailable. search-service
-- degrades to this rather than returning nothing.
CREATE INDEX products_fts_idx ON products
    USING GIN (to_tsvector('simple', title || ' ' || coalesce(brand,'') || ' ' || description));

CREATE TABLE variants (
    sku         TEXT PRIMARY KEY,                        -- sku_<ULID>
    product_id  TEXT        NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    attributes  JSONB       NOT NULL DEFAULT '{}'::jsonb, -- colour, size, ...
    -- Money is minor units, always (docs/CONTRACTS.md §2.5).
    price       BIGINT      NOT NULL CHECK (price >= 0),
    list_price  BIGINT      CHECK (list_price IS NULL OR list_price >= 0),
    currency    CHAR(3)     NOT NULL DEFAULT 'EGP',
    barcode     TEXT,
    weight_grams INT,
    position    INT         NOT NULL DEFAULT 0,
    active      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A "was 999, now 1299" strikethrough is a regulator's problem in most
    -- markets. Making it impossible in the schema is cheaper than a policy.
    CONSTRAINT list_price_not_below_price CHECK (list_price IS NULL OR list_price >= price)
);

CREATE INDEX variants_product_idx ON variants (product_id);
CREATE UNIQUE INDEX variants_barcode_uniq ON variants (barcode) WHERE barcode IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Price history.
--
-- Append-only. Required to answer "what did this cost on the day they bought
-- it?" during a dispute, and to prove a reference price was genuine when a
-- consumer authority asks.

CREATE TABLE price_history (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    sku         TEXT        NOT NULL,
    old_price   BIGINT,
    new_price   BIGINT      NOT NULL,
    old_list_price BIGINT,
    new_list_price BIGINT,
    currency    CHAR(3)     NOT NULL,
    changed_by  TEXT        NOT NULL,
    reason      TEXT,
    effective_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX price_history_sku_idx ON price_history (sku, effective_at DESC);

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

-- Automatically records every price change. A trigger rather than application
-- code because three different paths update a price (admin UI, bulk import,
-- promotion expiry) and a history with gaps is worse than no history.
CREATE OR REPLACE FUNCTION record_price_change() RETURNS TRIGGER AS $$
BEGIN
    IF (TG_OP = 'UPDATE' AND (OLD.price IS DISTINCT FROM NEW.price
                              OR OLD.list_price IS DISTINCT FROM NEW.list_price)) THEN
        INSERT INTO price_history (sku, old_price, new_price, old_list_price, new_list_price,
                                   currency, changed_by)
        VALUES (NEW.sku, OLD.price, NEW.price, OLD.list_price, NEW.list_price,
                NEW.currency, coalesce(current_setting('souq.actor', true), 'system'));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER variants_price_history
    AFTER UPDATE ON variants
    FOR EACH ROW EXECUTE FUNCTION record_price_change();
