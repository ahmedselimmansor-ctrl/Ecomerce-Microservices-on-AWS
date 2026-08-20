#!/bin/bash
# Creates every topic with the partition count and cleanup policy from
# docs/CONTRACTS.md §3.1.
#
# Auto-creation is disabled on the broker on purpose. A typo'd topic name would
# otherwise be created silently with one partition and delete-retention, and
# you would find out under load, in staging, weeks later.
#
# Partition counts are 1/4 of production here: the ratios that matter (order
# and inventory wide, catalog narrow) are preserved, without asking a laptop to
# manage 100 partitions.
set -euo pipefail

BROKER="${KAFKA_BROKER:-kafka:29092}"

echo "waiting for $BROKER"
for _ in $(seq 1 60); do
  kafka-topics.sh --bootstrap-server "$BROKER" --list >/dev/null 2>&1 && break
  sleep 2
done

create() {
  local topic="$1" partitions="$2" policy="$3" retention="$4"
  local extra=""
  [ "$policy" = "compact" ] && extra="--config min.cleanable.dirty.ratio=0.1 --config segment.ms=60000"

  kafka-topics.sh --bootstrap-server "$BROKER" --create --if-not-exists \
    --topic "$topic" --partitions "$partitions" --replication-factor 1 \
    --config cleanup.policy="$policy" \
    --config retention.ms="$retention" \
    $extra >/dev/null
  printf '  %-40s %2s partitions  %s\n' "$topic" "$partitions" "$policy"
}

D30=2592000000
D7=604800000
D90=7776000000
FOREVER=-1

echo "creating topics"
create souq.order.events.v1           3 delete  $D30
create souq.order.commands.v1         3 delete  $D7
create souq.inventory.events.v1       3 delete  $D30
create souq.payment.events.v1         3 delete  $D90
# Compacted: the newest message per productId is the whole product, so a new
# consumer can rebuild full catalogue state from offset 0. This is how
# search-service reindexes without calling catalog-service 50,000 times.
create souq.catalog.events.v1         2 compact $FOREVER
create souq.user.activity.v1          6 delete  $D7
create souq.notification.commands.v1  2 delete  $D7

echo "creating dead-letter topics"
for t in souq.order.events.v1 souq.order.commands.v1 souq.inventory.events.v1 \
         souq.payment.events.v1 souq.catalog.events.v1 souq.user.activity.v1 \
         souq.notification.commands.v1; do
  create "${t}.dlq" 1 delete $D90
done

echo
kafka-topics.sh --bootstrap-server "$BROKER" --list | sort
echo "topics ready"
