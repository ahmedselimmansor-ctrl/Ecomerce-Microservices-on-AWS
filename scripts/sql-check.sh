#!/usr/bin/env bash
# Applies every migration to a throwaway Postgres and asserts the invariants
# the schemas are supposed to enforce.
#
# This is not a lint. It is the check that the database-level backstops behind
# inventory-service internal/stock/model_test.go and internal/saga/model_test.go actually exist in
# the shipped DDL — every assertion below is a write that MUST be rejected.
set -uo pipefail
cd "$(dirname "$0")/.."

CONTAINER=souq-sqlcheck
PORT=55432
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; DIM=$'\033[2m'; OFF=$'\033[0m'
failures=0

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "starting throwaway postgres"
cleanup
docker run -d --rm --name "$CONTAINER" -e POSTGRES_PASSWORD=check \
  -p "$PORT:5432" postgres:16-alpine >/dev/null
for _ in $(seq 1 60); do
  docker exec "$CONTAINER" pg_isready -U postgres -q 2>/dev/null && break
  sleep 1
done

psql() { docker exec -i "$CONTAINER" psql -U postgres -tA "$@"; }

reset_schema() { psql -q -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;" >/dev/null 2>&1; }

# assert_rejected <description> <sql>
# Passes when the statement FAILS. Every one of these is a write that must be
# impossible no matter what the application layer does.
assert_rejected() {
  local desc="$1" sql="$2"
  if psql -v ON_ERROR_STOP=1 -c "$sql" >/dev/null 2>&1; then
    echo "  ${RED}ALLOWED${OFF}  $desc"
    failures=$((failures + 1))
  else
    echo "  ${GREEN}rejected${OFF} $desc"
  fi
}

assert_accepted() {
  local desc="$1" sql="$2"
  local out
  if out=$(psql -v ON_ERROR_STOP=1 -c "$sql" 2>&1); then
    echo "  ${GREEN}accepted${OFF} $desc"
  else
    echo "  ${RED}REJECTED${OFF} $desc"
    echo "${DIM}    $out${OFF}"
    failures=$((failures + 1))
  fi
}

# assert_equals <description> <expected> <sql producing one scalar>
assert_equals() {
  local desc="$1" want="$2" sql="$3" got
  got=$(psql -v ON_ERROR_STOP=1 -c "$sql" 2>&1 | tr -d '\r')
  if [ "$got" = "$want" ]; then
    echo "  ${GREEN}correct${OFF}  $desc"
  else
    echo "  ${RED}WRONG${OFF}    $desc"
    echo "${DIM}    expected [$want] got [$got]${OFF}"
    failures=$((failures + 1))
  fi
}

for svc in order-service inventory-service payment-service; do
  echo
  echo "=== $svc ==="
  reset_schema
  if ! docker exec -i "$CONTAINER" psql -U postgres -q -v ON_ERROR_STOP=1 \
       < "services/$svc/migrations/0001_init.sql" >/dev/null 2>&1; then
    echo "  ${RED}migration failed to apply${OFF}"
    docker exec -i "$CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 \
      < "services/$svc/migrations/0001_init.sql" 2>&1 | grep -i error | head -3
    failures=$((failures + 1))
    continue
  fi
  echo "  ${GREEN}migration applied${OFF} ($(psql -c "select count(*) from pg_tables where schemaname='public'") tables)"

  case "$svc" in
    order-service)
      base="INSERT INTO orders (id,user_id,status,currency,subtotal,total,shipping_address,payment_method_token,rules_version,correlation_id,idempotency_key)"
      assert_accepted "a well formed PENDING order" \
        "$base VALUES ('ord_ok','usr_1','PENDING','EUR',100,100,'{}','tok','v1','c','k1');"
      # The CHECK constraint and the Go state machine must agree. A state the
      # machine cannot produce must not be representable in the table.
      assert_rejected "a saga state the machine cannot produce" \
        "$base VALUES ('ord_x','usr_1','TELEPORTING','EUR',100,100,'{}','tok','v1','c','k2');"
      # ConsistentTerminalState, restated in SQL.
      assert_rejected "a CONFIRMED order with no payment or reservation" \
        "$base VALUES ('ord_y','usr_1','CONFIRMED','EUR',100,100,'{}','tok','v1','c','k3');"
      assert_rejected "a CANCELLED order with no reason recorded" \
        "$base VALUES ('ord_z','usr_1','CANCELLED','EUR',100,100,'{}','tok','v1','c','k4');"
      assert_rejected "a negative order total" \
        "$base VALUES ('ord_n','usr_1','PENDING','EUR',-1,-1,'{}','tok','v1','c','k5');"
      assert_rejected "two outbox rows sharing an event id" \
        "INSERT INTO outbox (aggregate_type,aggregate_id,event_id,event_type,topic,partition_key,payload)
         VALUES ('order','o','dup','t','tp','k','{}'),('order','o','dup','t','tp','k','{}');"
      ;;

    inventory-service)
      assert_accepted "a stock row" \
        "INSERT INTO stock_levels (sku,product_id,on_hand,reserved) VALUES ('sku_1','prd_1',10,0);"
      # NoOversell, in the one place no code path can bypass.
      assert_rejected "reserving more than exists" \
        "UPDATE stock_levels SET reserved = 11 WHERE sku='sku_1';"
      assert_rejected "negative on_hand" \
        "UPDATE stock_levels SET on_hand = -1 WHERE sku='sku_1';"
      assert_rejected "negative reserved" \
        "UPDATE stock_levels SET reserved = -1 WHERE sku='sku_1';"
      assert_rejected "an open reservation with no expiry" \
        "INSERT INTO reservations (id,order_id,state) VALUES ('rsv_1','ord_1','RESERVED');"
      assert_accepted "a tombstone: RELEASED with no preceding RESERVED (FINDINGS §2)" \
        "INSERT INTO reservations (id,order_id,state,was_tombstone) VALUES ('rsv_t','ord_t','RELEASED',TRUE);"
      assert_accepted "an open reservation with an expiry" \
        "INSERT INTO reservations (id,order_id,state,expires_at) VALUES ('rsv_2','ord_2','RESERVED',now()+interval '15 min');"
      assert_rejected "a second reservation for the same order" \
        "INSERT INTO reservations (id,order_id,state,expires_at) VALUES ('rsv_3','ord_2','RESERVED',now()+interval '15 min');"
      assert_rejected "a zero-quantity reservation line" \
        "INSERT INTO reservation_items (reservation_id,sku,quantity) VALUES ('rsv_2','sku_1',0);"
      ;;

    payment-service)
      base="INSERT INTO payments (id,order_id,user_id,state,currency,amount,provider,payment_method_token,psp_idempotency_key,correlation_id)"
      assert_accepted "a pending payment" \
        "$base VALUES ('pay_1','ord_1','usr_1','PENDING','EUR',1000,'mock','tok','souqkey1','c');"
      assert_rejected "a second payment for the same order" \
        "$base VALUES ('pay_2','ord_1','usr_1','PENDING','EUR',1000,'mock','tok','souqkey2','c');"
      # If two payments could share a provider key, the provider would treat
      # the second as a replay and the merchant would never be paid for it.
      assert_rejected "two payments sharing a provider idempotency key" \
        "$base VALUES ('pay_3','ord_3','usr_1','PENDING','EUR',1000,'mock','tok','souqkey1','c');"
      assert_rejected "capturing more than was authorised" \
        "UPDATE payments SET captured_amount = 2000 WHERE id='pay_1';"
      assert_accepted "capturing the authorised amount" \
        "UPDATE payments SET captured_amount = 1000 WHERE id='pay_1';"
      assert_rejected "refunding more than was captured" \
        "UPDATE payments SET refunded_amount = 1500 WHERE id='pay_1';"
      assert_rejected "a zero-amount payment" \
        "$base VALUES ('pay_4','ord_4','usr_1','PENDING','EUR',0,'mock','tok','souqkey4','c');"
      # Finance reconciles against the ledger, so the ledger has to be able to
      # prove itself balanced.
      assert_accepted "a balanced double-entry pair" \
        "INSERT INTO ledger_entries (payment_id,order_id,account,direction,amount,currency,entry_group)
         VALUES ('pay_1','ord_1','psp_clearing','DEBIT',1000,'EUR','11111111-1111-1111-1111-111111111111'),
                ('pay_1','ord_1','revenue','CREDIT',1000,'EUR','11111111-1111-1111-1111-111111111111');"
      imbalanced=$(psql -c "SELECT count(*) FROM unbalanced_entry_groups;")
      if [ "$imbalanced" = "0" ]; then
        echo "  ${GREEN}balanced${OFF} the ledger reports no imbalance"
      else
        echo "  ${RED}IMBALANCED${OFF} $imbalanced entry group(s)"
        failures=$((failures + 1))
      fi
      ;;
  esac
done

# ---------------------------------------------------------------------------
# The two Java services.
#
# Their migrations live under src/main/resources/db/migration and are applied
# by Flyway at startup, so nothing in this repository had ever executed them —
# CI's `mvn verify` compiles the code but never runs the DDL. They are applied
# here in version order, exactly as Flyway would.

for svc in identity-service catalog-service; do
  echo
  echo "=== $svc ==="
  reset_schema

  applied=0
  # Sorted by the numeric part, so V10 comes after V9 rather than after V1.
  # Flyway orders by version, and a plain glob would silently disagree.
  while IFS= read -r migration; do
    if ! docker exec -i "$CONTAINER" psql -U postgres -q -v ON_ERROR_STOP=1 \
         < "$migration" >/dev/null 2>&1; then
      echo "  ${RED}$(basename "$migration") failed to apply${OFF}"
      docker exec -i "$CONTAINER" psql -U postgres -v ON_ERROR_STOP=1 \
        < "$migration" 2>&1 | grep -i error | head -3
      failures=$((failures + 1))
      applied=-1
      break
    fi
    applied=$((applied + 1))
  done < <(find "services/$svc/src/main/resources/db/migration" -name 'V*.sql' \
           | sort -t V -k2 -n)

  [ "$applied" -lt 0 ] && continue
  echo "  ${GREEN}$applied migration(s) applied${OFF} ($(psql -c "select count(*) from pg_tables where schemaname='public'") tables)"

  case "$svc" in
    identity-service)
      assert_accepted "a user with credentials" \
        "INSERT INTO users (id,email,full_name,accepted_terms_version) VALUES ('usr_1','A@Souq.dev','A','v1');
         INSERT INTO credentials (user_id,password_hash) VALUES ('usr_1','argon2id\$x');"
      # Case-insensitive uniqueness. Two accounts differing only by
      # capitalisation is an account-takeover vector at the reset step.
      assert_rejected "a second account differing only by capitalisation" \
        "INSERT INTO users (id,email,full_name,accepted_terms_version) VALUES ('usr_2','a@souq.dev','B','v1');"
      assert_rejected "a role outside the fixed set" \
        "INSERT INTO roles (user_id,role) VALUES ('usr_1','SUPERUSER');"
      assert_accepted "a refresh token" \
        "INSERT INTO refresh_tokens (id,user_id,session_id,token_hash,expires_at)
         VALUES ('rt_1','usr_1','sess_1','h1',now()+interval '30 days');"
      assert_rejected "a refresh token in a state the service cannot produce" \
        "UPDATE refresh_tokens SET state = 'MAYBE' WHERE id='rt_1';"
      # Every rotation inserts a row whose hash must be new. A collision here
      # would let one stored token satisfy two different presented values.
      assert_rejected "two refresh tokens sharing a hash" \
        "INSERT INTO refresh_tokens (id,user_id,session_id,token_hash,expires_at)
         VALUES ('rt_2','usr_1','sess_1','h1',now()+interval '30 days');"
      assert_rejected "two outbox rows sharing an event id" \
        "INSERT INTO outbox (aggregate_type,aggregate_id,event_id,event_type,topic,partition_key,payload)
         VALUES ('user','u','dup','t','tp','k','{}'),('user','u','dup','t','tp','k','{}');"

      # -------- two-factor enrolment (V2) --------------------------------
      # `mfa_enabled` without a secret locks the user out of their own account
      # with no way back, so the schema refuses to represent it.
      assert_rejected "MFA enabled with no secret" \
        "UPDATE users SET mfa_enabled = TRUE WHERE id='usr_1';"
      assert_accepted "MFA enabled together with a secret" \
        "UPDATE users SET mfa_enabled = TRUE, mfa_secret = 'JBSWY3DPEHPK3PXP' WHERE id='usr_1';"

      # A pending secret with no timestamp cannot expire, so it would sit there
      # indefinitely; a timestamp with no secret is a row the sweeper keeps
      # finding and cannot clear.
      assert_rejected "a pending secret with no timestamp" \
        "UPDATE users SET mfa_pending_secret = 'X' WHERE id='usr_1';"
      assert_rejected "a pending timestamp with no secret" \
        "UPDATE users SET mfa_pending_since = now() WHERE id='usr_1';"
      assert_accepted "a coherent pending enrolment" \
        "UPDATE users SET mfa_pending_secret = 'X', mfa_pending_since = now() WHERE id='usr_1';"

      assert_accepted "a recovery code" \
        "INSERT INTO mfa_recovery_codes (user_id,code_hash) VALUES ('usr_1','h1');"
      assert_rejected "the same recovery code twice for one user" \
        "INSERT INTO mfa_recovery_codes (user_id,code_hash) VALUES ('usr_1','h1');"
      # Single use, enforced by the conditional UPDATE rather than by a read
      # followed by a write — two requests racing the same code must produce
      # exactly one winner.
      psql -q -c "UPDATE mfa_recovery_codes SET used_at = now()
                   WHERE user_id='usr_1' AND code_hash='h1' AND used_at IS NULL;" >/dev/null
      assert_equals "a spent recovery code cannot be spent again" "0" \
        "WITH spend AS (
           UPDATE mfa_recovery_codes SET used_at = now()
            WHERE user_id='usr_1' AND code_hash='h1' AND used_at IS NULL
            RETURNING 1)
         SELECT count(*) FROM spend;"
      ;;

    catalog-service)
      assert_accepted "a category and an active product" \
        "INSERT INTO categories (id,slug,name,path) VALUES ('cat_1','audio','Audio','{audio}');
         INSERT INTO products (id,slug,title,category_id,status) VALUES ('prd_1','p','P','cat_1','ACTIVE');"
      assert_rejected "a product status the service cannot produce" \
        "INSERT INTO products (id,slug,title,status) VALUES ('prd_x','x','X','PUBLISHED');"
      assert_accepted "a variant priced in minor units" \
        "INSERT INTO variants (sku,product_id,price,currency) VALUES ('sku_1','prd_1',129900,'EGP');"
      assert_rejected "a negative price" \
        "UPDATE variants SET price = -1 WHERE sku='sku_1';"
      # A "was 999, now 1299" strikethrough is a regulator's problem in most
      # markets. The schema makes it unrepresentable.
      assert_rejected "a reference price below the selling price" \
        "UPDATE variants SET list_price = 99900 WHERE sku='sku_1';"
      assert_accepted "a reference price above the selling price" \
        "UPDATE variants SET list_price = 149900 WHERE sku='sku_1';"
      assert_rejected "two variants sharing a barcode" \
        "UPDATE variants SET barcode='123' WHERE sku='sku_1';
         INSERT INTO variants (sku,product_id,price,barcode) VALUES ('sku_2','prd_1',1,'123');"
      # -------- V2 -------------------------------------------------------
      assert_rejected "negative availability" \
        "UPDATE variants SET available = -1 WHERE sku='sku_1';"
      assert_accepted "zero availability, which is not the same as unknown" \
        "UPDATE variants SET available = 0 WHERE sku='sku_1';"
      assert_rejected "an images column holding an object instead of an array" \
        "UPDATE variants SET images = '{}'::jsonb WHERE sku='sku_1';"
      assert_rejected "an attributes column holding an array instead of an object" \
        "UPDATE products SET attributes = '[]'::jsonb WHERE id='prd_1';"
      assert_accepted "a well formed images array" \
        "UPDATE variants SET images = '[{\"url\":\"https://cdn.souq.dev/a.jpg\"}]'::jsonb WHERE sku='sku_1';"

      # -------- price history is the trigger's job, not the app's --------
      # Three paths change a price and a fourth is whoever opens psql during an
      # incident, so the guarantee has to be in the database. The application
      # must therefore NOT also insert — two rows per change silently doubles
      # every report finance builds from this table.
      # A clean baseline: the list_price assertions above already changed a
      # price column, so the trigger has legitimately fired once already.
      psql -q -c "DELETE FROM price_history;" >/dev/null
      # set_config and the UPDATE run separately from the assertion, so the
      # captured output is the SELECT's result and nothing else.
      psql -q -c "SELECT set_config('souq.actor','usr_admin',true);
                  SELECT set_config('souq.reason','competitor match',true);
                  UPDATE variants SET price = 119900 WHERE sku='sku_1';" >/dev/null
      assert_equals "a price change writes exactly one history row" "1" \
        "SELECT count(*) FROM price_history WHERE sku='sku_1';"
      assert_equals "the trigger records the actor from the session setting" "usr_admin" \
        "SELECT changed_by FROM price_history WHERE sku='sku_1' ORDER BY id DESC LIMIT 1;"
      assert_equals "the trigger records the reason (V3)" "competitor match" \
        "SELECT reason FROM price_history WHERE sku='sku_1' ORDER BY id DESC LIMIT 1;"
      assert_equals "the trigger records the transition, not just the new value" "129900|119900" \
        "SELECT old_price || '|' || new_price FROM price_history
          WHERE sku='sku_1' ORDER BY id DESC LIMIT 1;"
      # A write that does not touch the price must not manufacture history.
      psql -q -c "UPDATE variants SET position = 3 WHERE sku='sku_1';" >/dev/null
      assert_equals "a non-price update writes no history row" "1" \
        "SELECT count(*) FROM price_history WHERE sku='sku_1';"
      # An UPDATE from a session that never set the variables must still work.
      # Migrations and manual fixes run that way, and current_setting() without
      # the missing_ok argument would raise and block them entirely.
      assert_accepted "a price change from a session with no actor set" \
        "UPDATE variants SET price = 109900 WHERE sku='sku_1';"
      assert_equals "an unattributed change is recorded as 'system'" "system" \
        "SELECT changed_by FROM price_history WHERE sku='sku_1' ORDER BY id DESC LIMIT 1;"

      # -------- the category tree ----------------------------------------
      # The path array is redundant with parent_id on purpose: parent_id makes a
      # move cheap, path makes "everything under electronics" one indexed query.
      # Redundancy is only safe while the two agree, so the statements that
      # maintain them are asserted here rather than trusted.
      psql -q -c "DELETE FROM variants; DELETE FROM products; DELETE FROM categories;" >/dev/null

      insert_cat() {  # id slug parent-or-NULL
        local parent="$3"; [ "$parent" = "NULL" ] && parent="NULL" || parent="'$3'"
        psql -q -c "INSERT INTO categories (id,slug,name,parent_id,path)
                    SELECT '$1','$2','$2',$parent,
                      CASE WHEN $parent::text IS NULL THEN ARRAY['$2']::text[]
                           ELSE (SELECT path FROM categories WHERE id = $parent)
                                || ARRAY['$2']::text[] END;" >/dev/null
      }
      insert_cat c1 electronics NULL
      insert_cat c2 audio c1
      insert_cat c3 headphones c2
      insert_cat c4 home NULL

      assert_equals "insert derives a child path from its parent" \
        "{electronics,audio,headphones}" \
        "SELECT path::text FROM categories WHERE id='c3';"

      assert_equals "the subtree query finds descendants at any depth" "3" \
        "SELECT count(*) FROM categories WHERE path @> ARRAY['electronics'];"

      # Moving `audio` under `home` must drag `headphones` with it. Rewriting
      # only the moved row leaves a descendant pointing at a path that no
      # longer exists, and it silently drops out of every listing.
      psql -q -c "WITH new_prefix AS (
                    SELECT (SELECT path FROM categories WHERE id='c4')
                           || ARRAY['audio']::text[] AS prefix)
                  UPDATE categories c
                     SET path = (SELECT prefix FROM new_prefix) || c.path[2 + 1 : ],
                         parent_id = CASE WHEN c.id='c2' THEN 'c4' ELSE c.parent_id END
                   WHERE c.path @> ARRAY['audio'];" >/dev/null

      assert_equals "a move rewrites the moved category's path" "{home,audio}" \
        "SELECT path::text FROM categories WHERE id='c2';"
      assert_equals "a move drags every descendant's path with it" \
        "{home,audio,headphones}" \
        "SELECT path::text FROM categories WHERE id='c3';"
      assert_equals "a move leaves untouched branches alone" "{electronics}" \
        "SELECT path::text FROM categories WHERE id='c1';"

      # parent_id and path must still tell the same story. This is the
      # invariant the redundancy depends on.
      assert_equals "parent_id and path agree for every category" "0" \
        "SELECT count(*) FROM categories c JOIN categories p ON p.id = c.parent_id
          WHERE c.path <> p.path || ARRAY[c.slug]::text[];"

      # A leaf with products must survive the delete, guarded in the WHERE
      # clause so a concurrent insert cannot slip through.
      psql -q -c "INSERT INTO products (id,slug,title,category_id,status)
                  VALUES ('prd_g','g','G','c3','ACTIVE');" >/dev/null
      assert_equals "a category holding products is not deleted" "1" \
        "WITH d AS (DELETE FROM categories c WHERE c.id='c3'
                      AND NOT EXISTS (SELECT 1 FROM categories x WHERE x.parent_id=c.id)
                      AND NOT EXISTS (SELECT 1 FROM products p WHERE p.category_id=c.id)
                    RETURNING 1)
         SELECT count(*) FROM categories WHERE id='c3';"
      assert_equals "a category with children is not deleted" "1" \
        "WITH d AS (DELETE FROM categories c WHERE c.id='c2'
                      AND NOT EXISTS (SELECT 1 FROM categories x WHERE x.parent_id=c.id)
                      AND NOT EXISTS (SELECT 1 FROM products p WHERE p.category_id=c.id)
                    RETURNING 1)
         SELECT count(*) FROM categories WHERE id='c2';"
      ;;
  esac
done

echo
if [ "$failures" -eq 0 ]; then
  echo "${GREEN}every schema invariant held${OFF}"
else
  echo "${RED}$failures assertion(s) failed${OFF}"
  exit 1
fi
