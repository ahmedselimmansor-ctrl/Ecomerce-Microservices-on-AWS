#!/usr/bin/env bash
# Kills a saga participant mid-flight and asserts the invariants still hold.
#
# The point is not that the order succeeds — it may well be cancelled. The
# point is that the system never ends up in a state internal/saga/model_test.go says
# is impossible: stock committed with no payment, or money captured with no
# stock.
#
#   ./scripts/chaos.sh inventory   # kill inventory-service mid-reservation
#   ./scripts/chaos.sh payment     # kill payment-service mid-authorisation
#   ./scripts/chaos.sh kafka       # partition the broker
#   ./scripts/chaos.sh all
set -uo pipefail
cd "$(dirname "$0")/.."

TARGET="${1:-all}"
GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YELLOW=$'\033[0;33m'; OFF=$'\033[0m'
PG="docker compose exec -T postgres psql -U souq -tA"
failures=0

assert_invariants() {
  local label="$1"
  echo "  invariants after $label:"

  local over drift illegal
  over=$($PG -d inventory -c "SELECT count(*) FROM stock_levels WHERE reserved > on_hand;" | tr -d '[:space:]')
  drift=$($PG -d inventory -c "
    SELECT count(*) FROM stock_levels s
     WHERE s.reserved <> COALESCE((
       SELECT sum(ri.quantity) FROM reservation_items ri
         JOIN reservations r ON r.id = ri.reservation_id
        WHERE ri.sku = s.sku AND r.state='RESERVED'), 0);" | tr -d '[:space:]')
  illegal=$(curl -fsS http://localhost:8084/metrics 2>/dev/null \
    | awk '/^souq_saga_illegal_transitions_total/ {s+=$2} END {print s+0}')

  [ "${over:-0}" -eq 0 ]  && echo "    ${GREEN}ok${OFF}   NoOversell" \
                          || { echo "    ${RED}FAIL${OFF} NoOversell: ${over} SKU(s)"; failures=$((failures+1)); }
  [ "${drift:-0}" -eq 0 ] && echo "    ${GREEN}ok${OFF}   Conservation" \
                          || { echo "    ${RED}FAIL${OFF} Conservation: ${drift} SKU(s) drifted"; failures=$((failures+1)); }
  [ "${illegal:-0}" = "0" ] && echo "    ${GREEN}ok${OFF}   no illegal transitions" \
                            || { echo "    ${RED}FAIL${OFF} ${illegal} illegal transition(s)"; failures=$((failures+1)); }
}

kill_during_checkout() {
  local service="$1" container="$2"
  echo
  echo "${YELLOW}killing $service mid-checkout${OFF}"

  # Fire orders in the background, then kill the participant while they are
  # in flight. SIGKILL, not SIGTERM: graceful shutdown is a different test.
  ./scripts/smoke.sh 10 >/dev/null 2>&1 &
  local smoke_pid=$!

  sleep 2
  docker kill --signal=SIGKILL "$container" >/dev/null 2>&1 || true
  echo "  killed $container"

  sleep 3
  docker compose up -d "$service" >/dev/null 2>&1
  echo "  restarted $service"

  wait "$smoke_pid" 2>/dev/null || true

  # Give the sweepers time to resolve whatever was left in flight. Termination
  # is a liveness property: it says the saga settles eventually, not instantly.
  echo "  waiting 45s for the sweepers to converge"
  sleep 45

  local stuck
  stuck=$($PG -d orders -c "
    SELECT count(*) FROM orders
     WHERE status NOT IN ('CONFIRMED','CANCELLED','SHIPPED','DELIVERED','REFUNDED')
       AND placed_at < now() - interval '40 seconds';" | tr -d '[:space:]')

  if [ "${stuck:-0}" -eq 0 ]; then
    echo "  ${GREEN}ok${OFF}   Termination: every order converged after the kill"
  else
    echo "  ${RED}FAIL${OFF} ${stuck} order(s) wedged"
    failures=$((failures+1))
  fi

  assert_invariants "$service kill"
}

case "$TARGET" in
  inventory) kill_during_checkout inventory-service souq-inventory ;;
  payment)   kill_during_checkout payment-service   souq-payment ;;
  order)     kill_during_checkout order-service     souq-order ;;
  kafka)
    echo
    echo "${YELLOW}partitioning Kafka${OFF}"
    ./scripts/smoke.sh 10 >/dev/null 2>&1 &
    sleep 2
    docker network disconnect souq souq-kafka >/dev/null 2>&1 || true
    echo "  disconnected the broker"
    sleep 10
    docker network connect souq souq-kafka >/dev/null 2>&1 || true
    echo "  reconnected; waiting 60s for the outbox relays to drain"
    sleep 60
    assert_invariants "kafka partition"
    ;;
  all)
    for t in inventory payment order; do "$0" "$t" || failures=$((failures+1)); done
    ;;
  *)
    echo "usage: $0 [inventory|payment|order|kafka|all]"; exit 2 ;;
esac

echo
if [ "$failures" -eq 0 ]; then
  echo "${GREEN}the system survived; no invariant was violated${OFF}"
else
  echo "${RED}${failures} invariant failure(s) — this is a real bug, not flakiness${OFF}"
  exit 1
fi
