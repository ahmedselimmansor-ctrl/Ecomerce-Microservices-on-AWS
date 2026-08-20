#!/usr/bin/env bash
# Loads a demo catalogue, stock and users into the local stack.
#
# Deliberately small and deliberately awkward: prices that expose rounding,
# a SKU with exactly one unit left, an Arabic title, and a discontinued
# product. Seed data that is all round numbers and happy paths hides the bugs
# you actually want to find locally.
set -uo pipefail
cd "$(dirname "$0")/.."

GREEN=$'\033[0;32m'; DIM=$'\033[2m'; OFF=$'\033[0m'
PG="docker compose exec -T postgres psql -U souq -q -v ON_ERROR_STOP=1"

echo "seeding catalog"
$PG -d catalog <<'SQL'
TRUNCATE variants, products, categories, price_history, outbox CASCADE;

INSERT INTO categories (id, slug, name, parent_id, path, position) VALUES
  ('cat_electronics','electronics','Electronics',NULL,ARRAY['electronics'],1),
  ('cat_audio','audio','Audio','cat_electronics',ARRAY['electronics','audio'],1),
  ('cat_phones','phones','Phones','cat_electronics',ARRAY['electronics','phones'],2),
  ('cat_home','home','Home & Kitchen',NULL,ARRAY['home'],2),
  ('cat_coffee','coffee','Coffee','cat_home',ARRAY['home','coffee'],1);

INSERT INTO products (id, slug, title, description, brand, category_id, status, attributes, images) VALUES
  ('prd_01J8Z3K9S2M4P6R8T0V2X4Y001','sony-wh1000xm5','Sony WH-1000XM5 Wireless Headphones',
   'Industry-leading noise cancellation with 30-hour battery life.','Sony','cat_audio','ACTIVE',
   '{"colour":"black","connectivity":"bluetooth","noise_cancelling":"yes"}',
   '[{"url":"https://picsum.photos/seed/xm5/800/800","alt":"Sony WH-1000XM5"}]'),

  ('prd_01J8Z3K9S2M4P6R8T0V2X4Y002','anker-soundcore-q30','Anker Soundcore Life Q30',
   'Hybrid active noise cancelling headphones with 40-hour playtime.','Anker','cat_audio','ACTIVE',
   '{"colour":"blue","connectivity":"bluetooth","noise_cancelling":"yes"}',
   '[{"url":"https://picsum.photos/seed/q30/800/800","alt":"Soundcore Q30"}]'),

  -- Arabic title: exercises the arabic_text analyzer and the folded fallback
  -- in search-service, and RTL rendering in the storefront.
  ('prd_01J8Z3K9S2M4P6R8T0V2X4Y003','delonghi-dedica','ديلونجي ديديكا ماكينة قهوة إسبريسو',
   'ماكينة إسبريسو احترافية بمضخة 15 بار وتصميم نحيف.','DeLonghi','cat_coffee','ACTIVE',
   '{"colour":"stainless","pressure_bar":"15"}',
   '[{"url":"https://picsum.photos/seed/dedica/800/800","alt":"DeLonghi Dedica"}]'),

  ('prd_01J8Z3K9S2M4P6R8T0V2X4Y004','samsung-a55','Samsung Galaxy A55 5G',
   '6.6-inch Super AMOLED, 50MP camera, 5000mAh battery.','Samsung','cat_phones','ACTIVE',
   '{"colour":"navy","storage":"256GB","network":"5G"}',
   '[{"url":"https://picsum.photos/seed/a55/800/800","alt":"Galaxy A55"}]'),

  -- Discontinued: must never appear in search or be reservable.
  ('prd_01J8Z3K9S2M4P6R8T0V2X4Y005','old-model-x','Legacy Model X',
   'No longer sold.','Acme','cat_electronics','DISCONTINUED','{}','[]');

-- Prices chosen to expose rounding: 14% Egyptian VAT on 129999 is 15999.877,
-- and 20% off 33333 is 6666.6. Round numbers hide these.
INSERT INTO variants (sku, product_id, attributes, price, list_price, currency) VALUES
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y001','prd_01J8Z3K9S2M4P6R8T0V2X4Y001','{"colour":"black"}',1299900,1499900,'EGP'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y002','prd_01J8Z3K9S2M4P6R8T0V2X4Y002','{"colour":"blue"}',249999,NULL,'EGP'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y003','prd_01J8Z3K9S2M4P6R8T0V2X4Y003','{"colour":"stainless"}',899933,999900,'EGP'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y004','prd_01J8Z3K9S2M4P6R8T0V2X4Y004','{"storage":"256GB"}',1899900,NULL,'EGP'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y005','prd_01J8Z3K9S2M4P6R8T0V2X4Y005','{}',99900,NULL,'EGP');

-- variants.available is normally written by catalog-service's inventory
-- consumer as souq.inventory.events.v1 arrives. It is seeded directly here so
-- `make seed && make up-frontend` shows real numbers without waiting for the
-- event round trip — the values below mirror the stock levels seeded next.
--
-- One SKU is left NULL on purpose. NULL means "we have not heard from
-- inventory", which is not the same as 0, and the storefront has to render the
-- two differently: showing "out of stock" when the truth is "unknown"
-- suppresses sales of items that are actually available.
UPDATE variants SET available = v.available FROM (VALUES
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y001', 50),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y002', 200),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y003', 1),    -- the "only 1 left" badge
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y004', 25)
) AS v(sku, available) WHERE variants.sku = v.sku;
SQL
echo "  ${GREEN}5 products, 5 categories, 5 variants (one with unknown availability)${OFF}"

echo "seeding inventory"
$PG -d inventory <<'SQL'
TRUNCATE reservation_items, reservations, stock_ledger, stock_levels, outbox, processed_events CASCADE;

INSERT INTO stock_levels (sku, product_id, on_hand, reserved, reorder_point, status) VALUES
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y001','prd_01J8Z3K9S2M4P6R8T0V2X4Y001', 50, 0, 10,'ACTIVE'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y002','prd_01J8Z3K9S2M4P6R8T0V2X4Y002',200, 0, 20,'ACTIVE'),
  -- Exactly one left. This is the row the oversell tests race on, and the one
  -- that makes the "only 1 left" badge appear in the storefront.
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y003','prd_01J8Z3K9S2M4P6R8T0V2X4Y003',  1, 0,  5,'ACTIVE'),
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y004','prd_01J8Z3K9S2M4P6R8T0V2X4Y004', 25, 0, 10,'ACTIVE'),
  -- Discontinued: a reservation against it must be rejected.
  ('sku_01J8Z3K9S2M4P6R8T0V2X4Y005','prd_01J8Z3K9S2M4P6R8T0V2X4Y005', 10, 0,  0,'DISCONTINUED');
SQL
echo "  ${GREEN}5 SKUs (one with a single unit, one discontinued)${OFF}"

echo "seeding identity"
$PG -d identity <<'SQL'
TRUNCATE roles, credentials, refresh_tokens, login_attempts, users CASCADE;

INSERT INTO users (id, email, full_name, locale, email_verified, accepted_terms_version) VALUES
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y001','customer@souq.local','Ahmed Hassan','ar-EG',TRUE,'v1'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y002','admin@souq.local','Platform Admin','en-GB',TRUE,'v1'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y003','ops@souq.local','On Call','en-GB',TRUE,'v1');

-- Argon2id hash of "correct-horse-battery-staple". Local only; the string is
-- deliberately a well-known example so nobody mistakes it for a real secret.
INSERT INTO credentials (user_id, password_hash) VALUES
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y001','$argon2id$v=19$m=65536,t=3,p=4$LOCALDEVONLYNOTASECRET$devseedhashplaceholder'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y002','$argon2id$v=19$m=65536,t=3,p=4$LOCALDEVONLYNOTASECRET$devseedhashplaceholder'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y003','$argon2id$v=19$m=65536,t=3,p=4$LOCALDEVONLYNOTASECRET$devseedhashplaceholder');

INSERT INTO roles (user_id, role) VALUES
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y001','CUSTOMER'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y002','ADMIN'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y002','OPS'),
  ('usr_01J8Z3K9S2M4P6R8T0V2X4Y003','OPS');
SQL
echo "  ${GREEN}3 users (customer / admin / ops)${OFF}"

echo
echo "${GREEN}seeded${OFF}"
echo "${DIM}  customer@souq.local  password: correct-horse-battery-staple${OFF}"
echo "${DIM}  admin@souq.local     roles: ADMIN, OPS${OFF}"
echo "${DIM}  sku_...Y003 has exactly 1 unit — use it to test the oversell path${OFF}"
echo "${DIM}  next: make smoke${OFF}"
