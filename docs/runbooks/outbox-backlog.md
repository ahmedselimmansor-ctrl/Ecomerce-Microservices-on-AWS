# Runbook: outbox backlog growing

**Severity:** ticket · **Alert:** `OutboxBacklogGrowing` · **Metric:** `souq_outbox_unpublished`

The alert is on the **derivative**, not the depth. A backlog of 5,000 that is draining is fine;
one of 500 that is growing is not, because it means the relay is losing to the write rate and
every event is getting steadily staler.

Nothing is lost — that is the whole point of the outbox (`internal/eventbus/outbox_model_test.go`). But a saga
whose next command is sitting unpublished is a customer watching a spinner.

## Triage

```sql
SELECT count(*) FILTER (WHERE published_at IS NULL) AS pending,
       min(created_at) FILTER (WHERE published_at IS NULL) AS oldest,
       max(attempts) AS worst_attempts,
       (array_agg(last_error ORDER BY attempts DESC))[1] AS worst_error
  FROM outbox;
```

| Symptom | Cause | Fix |
|---|---|---|
| `worst_attempts` high, `worst_error` set | Kafka is rejecting the publish | Read the error; usually an auth or topic issue |
| `worst_attempts` = 0, `oldest` growing | The relay is not running or not keeping up | Check the pod; scale out |
| A single row with high attempts, rest fine | One poison message | See below |

## The relay is not keeping up

Each replica polls every 200ms in batches of 100, so one replica sustains roughly 500 events/s.
Above that, scale out — the relay uses `FOR UPDATE SKIP LOCKED`, so replicas take disjoint sets
and adding them is safe with no coordination.

```bash
kubectl -n souq scale deploy/order-service --replicas=6
```

If the write rate is genuinely higher than the relay can drain, raise the batch size rather than
shortening the interval — a bigger batch amortises the round trip, a shorter interval just adds
database load.

```yaml
SOUQ_OUTBOX_BATCH_SIZE: "500"
```

## One poison row

```sql
SELECT id, event_type, topic, attempts, last_error, created_at
  FROM outbox WHERE published_at IS NULL AND attempts > 5
 ORDER BY attempts DESC LIMIT 10;
```

A row that will never publish (a topic that does not exist, a payload above the broker's message
limit) blocks nothing — `SKIP LOCKED` means the relay moves past it — but it keeps the backlog
non-zero and hides real growth. Fix the cause; if the event is genuinely undeliverable and no
longer meaningful, mark it published with a note on the incident. **Never delete it** — the
row is the only record that the event existed.

## Related
- [`internal/eventbus/outbox_model_test.go`](../../internal/eventbus/outbox_model_test.go)
- [`dlq.md`](dlq.md)
