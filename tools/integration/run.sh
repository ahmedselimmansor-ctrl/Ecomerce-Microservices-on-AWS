#!/usr/bin/env bash
# Cross-service integration tests against the running local stack.
#
# Distinct from `make smoke`, which places orders through the public API and
# checks the outcome. This exercises the seams between services directly:
# reserve then release, reserve then commit, and the tombstone race that
# docs/DESIGN-INVARIANTS.md §2 is about — the one no unit test can reach
# because it needs two services disagreeing about time.
set -uo pipefail
cd "$(dirname "$0")/../.."

INVENTORY="${SOUQ_INVENTORY_URL:-http://localhost:8085}"
PG="docker compose exec -T postgres psql -U souq -tA"
G=$'\033[0;32m'; R=$'\033[0;31m'; D=$'\033[2m'; O=$'\033[0m'
fails=0
pass() { echo "  ${G}ok${O}   $1"; }
fail() { echo "  ${R}FAIL${O} $1"; fails=$((fails+1)); }

command -v jq >/dev/null || { echo "jq is required"; exit 127; }
curl -fsS --max-time 3 "$INVENTORY/health/ready" >/dev/null 2>&1 || {
  echo "${R}inventory-service is not ready at $INVENTORY — run 'make up-services'${O}"; exit 1; }

ulid() { printf '%s_%s' "$1" "$(tr -dc '0-9A-HJKMNP-TV-Z' </dev/urandom | head -c 26)"; }
sku() { $PG -d inventory -c \
  "SELECT sku FROM stock_levels WHERE status='ACTIVE' AND on_hand-reserved >= 2 ORDER BY random() LIMIT 1;" \
  | tr -d '[:space:]'; }

available() { $PG -d inventory -c "SELECT on_hand-reserved FROM stock_levels WHERE sku='$1';" | tr -d '[:space:]'; }

# ---------------------------------------------------------------- reserve/release
echo "reserve then release returns the stock"
S=$(sku); [ -n "$S" ] || { fail "no SKU with stock — run 'make seed'"; exit 1; }
BEFORE=$(available "$S")
RSV=$(ulid rsv); ORD=$(ulid ord)

curl -fsS -X POST "$INVENTORY/v1/reservations" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD\",\"reservationId\":\"$RSV\",\"items\":[{\"sku\":\"$S\",\"quantity\":2}]}" \
  >/dev/null 2>&1 || fail "reserve was rejected"

MID=$(available "$S")
[ "$MID" -eq $((BEFORE - 2)) ] && pass "available fell by 2 ($BEFORE -> $MID)" \
                               || fail "available is $MID, expected $((BEFORE-2))"

curl -fsS -X POST "$INVENTORY/v1/reservations/$RSV/release" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD\",\"reasonCode\":\"INTEGRATION_TEST\"}" >/dev/null 2>&1 \
  || fail "release was rejected"

AFTER=$(available "$S")
[ "$AFTER" -eq "$BEFORE" ] && pass "release restored it ($AFTER)" \
                           || fail "available is $AFTER, expected $BEFORE"

# ---------------------------------------------------------------- idempotency
echo
echo "a redelivered reserve is a no-op, not a second hold"
RSV2=$(ulid rsv); ORD2=$(ulid ord)
BEFORE=$(available "$S")
for _ in 1 2 3; do
  curl -fsS -X POST "$INVENTORY/v1/reservations" -H 'Content-Type: application/json' \
    -d "{\"orderId\":\"$ORD2\",\"reservationId\":\"$RSV2\",\"items\":[{\"sku\":\"$S\",\"quantity\":1}]}" \
    >/dev/null 2>&1
done
AFTER=$(available "$S")
[ "$AFTER" -eq $((BEFORE - 1)) ] && pass "three identical reserves held 1 unit, not 3" \
                                 || fail "held $((BEFORE-AFTER)) units for three identical requests"
curl -fsS -X POST "$INVENTORY/v1/reservations/$RSV2/release" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD2\",\"reasonCode\":\"CLEANUP\"}" >/dev/null 2>&1

# ---------------------------------------------------------------- the tombstone
echo
echo "a release that arrives BEFORE its reserve (docs/DESIGN-INVARIANTS.md §2)"
RSV3=$(ulid rsv); ORD3=$(ulid ord)

# Release first — the reservation does not exist yet.
RESP=$(curl -fsS -X POST "$INVENTORY/v1/reservations/$RSV3/release" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD3\",\"reasonCode\":\"SAGA_TIMEOUT\"}" 2>/dev/null)

echo "$RESP" | jq -e '.wasTombstone == true' >/dev/null 2>&1 \
  && pass "wrote a tombstone rather than ignoring it" \
  || fail "did not report a tombstone: $RESP"

# Now the late reserve arrives. It must be REFUSED, not honoured.
BEFORE=$(available "$S")
RESP=$(curl -sS -X POST "$INVENTORY/v1/reservations" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD3\",\"reservationId\":\"$RSV3\",\"items\":[{\"sku\":\"$S\",\"quantity\":1}]}" 2>/dev/null)
AFTER=$(available "$S")

echo "$RESP" | jq -e '.duplicate == true and .state == "RELEASED"' >/dev/null 2>&1 \
  && pass "the late reserve found the tombstone and declined" \
  || fail "the late reserve was not declined: $RESP"

[ "$AFTER" -eq "$BEFORE" ] && pass "no stock was held by the late reserve" \
                           || fail "the late reserve held stock nobody will release"

# ---------------------------------------------------------------- commit is final
echo
echo "committed stock cannot be released"
RSV4=$(ulid rsv); ORD4=$(ulid ord)
curl -fsS -X POST "$INVENTORY/v1/reservations" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD4\",\"reservationId\":\"$RSV4\",\"items\":[{\"sku\":\"$S\",\"quantity\":1}]}" >/dev/null 2>&1
curl -fsS -X POST "$INVENTORY/v1/reservations/$RSV4/commit" -H 'Content-Type: application/json' \
  -d "{\"orderId\":\"$ORD4\"}" >/dev/null 2>&1 || fail "commit was rejected"

CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$INVENTORY/v1/reservations/$RSV4/release" \
  -H 'Content-Type: application/json' -d "{\"orderId\":\"$ORD4\",\"reasonCode\":\"SHOULD_FAIL\"}" 2>/dev/null)
[ "$CODE" = "409" ] && pass "releasing a committed reservation returned 409 (§1)" \
                    || fail "releasing a committed reservation returned $CODE, expected 409"

# ---------------------------------------------------------------- invariants
echo
echo "invariants after all of the above"
OVER=$($PG -d inventory -c "SELECT count(*) FROM stock_levels WHERE reserved > on_hand;" | tr -d '[:space:]')
[ "${OVER:-0}" -eq 0 ] && pass "NoOversell" || fail "NoOversell violated on $OVER SKU(s)"

DRIFT=$($PG -d inventory -c "
  SELECT count(*) FROM stock_levels s
   WHERE s.reserved <> COALESCE((SELECT sum(ri.quantity) FROM reservation_items ri
     JOIN reservations r ON r.id = ri.reservation_id
    WHERE ri.sku = s.sku AND r.state='RESERVED'), 0);" | tr -d '[:space:]')
[ "${DRIFT:-0}" -eq 0 ] && pass "Conservation" || fail "Conservation drifted on $DRIFT SKU(s)"

echo
[ "$fails" -eq 0 ] && echo "${G}integration tests passed${O}" \
                   || { echo "${R}$fails check(s) failed${O}"; exit 1; }
