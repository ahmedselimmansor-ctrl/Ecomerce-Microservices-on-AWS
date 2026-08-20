# Runbook: dead-letter queue

**Severity:** ticket · **Alert:** `DeadLetterQueueGrowing`

A message that failed its retry budget is parked rather than blocking its partition. That is
correct behaviour — one bad payload must not stall every order behind it — but a parked message
is work that did not happen.

## Triage

```bash
./scripts/dlq-depth.sh
```

Then read one:

```bash
docker compose exec kafka kafka-console-consumer.sh \
  --bootstrap-server localhost:29092 \
  --topic souq.order.commands.v1.dlq \
  --from-beginning --max-messages 5 \
  --property print.headers=true
```

The `x-dlq-reason` header says why it was parked and `x-dlq-attempts` how many times we tried.

## Common causes

| `x-dlq-reason` | Meaning | Action |
|---|---|---|
| `malformed cloudevents envelope` | A producer emitted something that is not a CloudEvent | Find the producer; it is almost always a hand-written test message |
| `event has no id` | Cannot be deduplicated, so it cannot be safely applied | Fix the producer. Never replay one of these — it would be applied on every redelivery forever |
| `unknown order <id>` | An event for an order this service has never seen | Usually a stale message from a truncated database. Safe to discard |
| `exhausted N attempts` | A downstream dependency was down for longer than the budget | Fix the dependency, then replay |

## Replaying

Only after the cause is fixed. Replaying into the same failure just refills the DLQ.

```bash
kubectl -n souq exec deploy/order-service -- \
  /order-service replay-dlq --topic souq.order.commands.v1.dlq --max 100
```

Replay is safe by construction: every consumer dedups on `event_id` via `processed_events`
(`docs/CONTRACTS.md` §5.2), so replaying a message that *did* get applied is a no-op.

## Discarding

For messages that are genuinely undeliverable and no longer meaningful. Record the count and the
reason on the incident — a silently drained DLQ is indistinguishable from one that was fixed.

## Related
- [`internal/eventbus/outbox_model_test.go`](../../internal/eventbus/outbox_model_test.go) — why duplicates are safe
- [`outbox-backlog.md`](outbox-backlog.md)
