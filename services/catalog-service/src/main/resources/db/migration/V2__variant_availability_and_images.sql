-- Two columns the read path needs and V1 did not have.
--
-- A new migration rather than an edit to V1: Flyway checksums applied
-- migrations, so changing one that has run anywhere fails validation on every
-- environment that already has it. `validate-on-migrate: true` in
-- application.yml exists precisely to make that failure loud.

-- ---------------------------------------------------------------------------
-- variants.available
--
-- Denormalised from inventory-service, which owns the number (CONTRACTS §6).
-- It lives here so a product page is one query instead of a fan-out to
-- inventory for every SKU on the page — at twenty products with four variants
-- each, that is eighty synchronous calls to render a listing.
--
-- NULL is meaningful and distinct from 0:
--
--   NULL  we have not heard from inventory for this SKU
--   0     inventory says none are available
--
-- The UI must render them differently. "Out of stock" when the truth is "we do
-- not know" suppresses sales of items that are actually in stock.
--
-- This value is EVENTUALLY CONSISTENT and is a display hint only. The
-- authoritative check happens when the order saga reserves. Gating checkout on
-- this column means accepting orders that cannot be fulfilled.

ALTER TABLE variants ADD COLUMN available INT;

ALTER TABLE variants ADD CONSTRAINT available_not_negative
    CHECK (available IS NULL OR available >= 0);

-- Partial: the only query that filters on it asks for in-stock variants, and
-- indexing the zeros and nulls would double the index for rows never selected.
CREATE INDEX variants_available_idx ON variants (product_id)
    WHERE available IS NOT NULL AND available > 0;

-- ---------------------------------------------------------------------------
-- variants.images
--
-- Variant-level imagery. A red shirt and a blue shirt are one product with two
-- variants, and showing the product's default image for both is the single
-- most common catalogue complaint. Empty array means "fall back to the
-- product's images", which is why the default is '[]' and not NULL.

ALTER TABLE variants ADD COLUMN images JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE variants ADD CONSTRAINT variant_images_is_array
    CHECK (jsonb_typeof(images) = 'array');

-- The same guard on the product columns. JSONB accepts any valid JSON, so
-- without these a bug that writes an object where an array belongs is only
-- discovered by whichever consumer deserialises it first.
ALTER TABLE products ADD CONSTRAINT product_images_is_array
    CHECK (jsonb_typeof(images) = 'array');

ALTER TABLE products ADD CONSTRAINT product_attributes_is_object
    CHECK (jsonb_typeof(attributes) = 'object');

ALTER TABLE variants ADD CONSTRAINT variant_attributes_is_object
    CHECK (jsonb_typeof(attributes) = 'object');
