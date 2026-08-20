#!/usr/bin/env bash
# Depth of every dead-letter topic, with the most recent reason.
#
# A non-zero DLQ is not automatically an incident — a genuinely malformed
# message from a misbehaving producer belongs there. A GROWING DLQ is.
set -uo pipefail

BROKER="${KAFKA_BROKER:-localhost:29092}"
RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; DIM=$'\033[2m'; OFF=$'\033[0m'

kafka() { docker compose exec -T kafka "$@"; }

topics=$(kafka kafka-topics.sh --bootstrap-server "$BROKER" --list 2>/dev/null | grep '\.dlq$' | sort)
[ -z "$topics" ] && { echo "no DLQ topics found"; exit 0; }

total=0
printf '%-42s %8s\n' "TOPIC" "DEPTH"
printf '%-42s %8s\n' "------------------------------------------" "--------"

for t in $topics; do
  depth=$(kafka kafka-run-class.sh kafka.tools.GetOffsetShell \
    --bootstrap-server "$BROKER" --topic "$t" 2>/dev/null \
    | awk -F: '{s+=$3} END {print s+0}')
  total=$((total + depth))

  colour="$GREEN"; [ "$depth" -gt 0 ] && colour="$RED"
  printf '%-42s %s%8d%s\n' "$t" "$colour" "$depth" "$OFF"

  if [ "$depth" -gt 0 ]; then
    reason=$(kafka kafka-console-consumer.sh --bootstrap-server "$BROKER" \
      --topic "$t" --from-beginning --max-messages 1 --timeout-ms 3000 \
      --property print.headers=true 2>/dev/null \
      | grep -o 'x-dlq-reason:[^,]*' | head -1)
    [ -n "$reason" ] && echo "  ${DIM}${reason}${OFF}"
  fi
done

echo
if [ "$total" -eq 0 ]; then
  echo "${GREEN}all dead-letter topics are empty${OFF}"
else
  echo "${RED}${total} message(s) parked${OFF} — see docs/runbooks/dlq.md"
  exit 1
fi
