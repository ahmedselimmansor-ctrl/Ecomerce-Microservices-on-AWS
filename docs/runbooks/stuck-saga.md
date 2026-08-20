# Runbook: saga stuck past the point of no return

**Severity:** page · **Alert:** `SagaStuckPastPointOfNoReturn` · **Metric:** `souq_saga_stuck_orders`

---

## What this means

One or more orders are in `PAID` or `STOCK_COMMITTED` and have not moved for five minutes.

These states are special. `docs/DESIGN-INVARIANTS.md` §1 proves that **compensating from them loses
money**: inventory commits, the acknowledgement is delayed, the saga times out and voids the
payment, and you end with stock deducted and nothing charged for it. So the state machine has no
rollback edge from either state, the sweeper double-checks and refuses, and a CHECK constraint
backs both up.

The consequence is that a stuck order here cannot resolve itself by giving up. It rolls forward
or a human settles it.

## Do not

- **Do not cancel the order.** Not through the API, not in SQL. The API will refuse
  (`ORDER_NOT_CANCELLABLE`); SQL will not, and that is the failure mode the whole design exists
  to prevent.
- **Do not release the inventory reservation.** The stock may already be picked.
- **Do not void the payment.** If stock is committed, voiding leaves you having given away goods.

## Triage

**1. Which orders, and what are they waiting on?**

```sql
SELECT o.id, o.status, o.total, o.currency, o.placed_at,
       s.step, s.state, s.attempts, s.deadline_at, s.error
  FROM orders o
  JOIN saga_steps s ON s.order_id = o.id
 WHERE o.status IN ('PAID','STOCK_COMMITTED')
   AND s.state = 'SENT'
   AND s.deadline_at < now() - interval '5 minutes'
 ORDER BY o.placed_at;
```

`step` tells you which command is unacknowledged:

- `COMMIT` — inventory has not confirmed the stock deduction
- `CAPTURE` — payment has not confirmed taking the money

**2. Is the participant alive?**

```bash
kubectl -n souq get pods -l app.kubernetes.io/name=inventory-service
kubectl -n souq get pods -l app.kubernetes.io/name=payment-service
kubectl -n souq logs deploy/inventory-service --since=10m | grep -i error | head -20
```

**3. Did the command actually get published?**

A stuck saga is often an outbox that is not draining, not a participant that is failing.

```sql
-- In the orders database.
SELECT count(*) AS pending,
       min(created_at) AS oldest,
       max(attempts) AS worst_attempts
  FROM outbox WHERE published_at IS NULL;
```

If `oldest` is more than a minute or two ago, the problem is the relay, not the saga. Go to
[`outbox-backlog.md`](outbox-backlog.md).

**4. Did the participant receive it but fail?**

```bash
./scripts/dlq-depth.sh
```

A command in the DLQ means the participant rejected it permanently. The `x-dlq-reason` header
says why.

## Resolution

### The participant was down and is now back

Nothing to do. The sweeper resends `COMMIT` or `CAPTURE` every 5 seconds with backoff, and the
participant is idempotent on both. Watch it drain:

```bash
watch -n5 'kubectl -n souq exec deploy/order-service -- \
  wget -qO- localhost:8084/metrics | grep souq_saga_stuck_orders'
```

### The command is in the DLQ

Read the reason, fix the cause, then replay:

```bash
kubectl -n souq exec deploy/order-service -- \
  /order-service replay-dlq --topic souq.order.commands.v1.dlq --order-id "$ORDER_ID"
```

### Inventory says the reservation is already committed

The `Committed` event was lost, not the commit. Confirm, then let the saga learn about it by
replaying the event rather than editing the order:

```sql
-- inventory database
SELECT id, order_id, state, updated_at FROM reservations WHERE order_id = '$ORDER_ID';
```

If `state = 'COMMITTED'`, the work is done. Re-emit:

```bash
kubectl -n souq exec deploy/inventory-service -- \
  /inventory-service reemit --reservation-id "$RESERVATION_ID"
```

### Payment cannot be captured — the authorisation expired

This is the genuinely bad case: stock is committed and the authorisation window has passed, so
the money cannot be taken with the customer's original consent.

```sql
SELECT id, order_id, state, authorization_expires_at
  FROM payments WHERE order_id = '$ORDER_ID';
```

There is no automated recovery. Escalate to the payments on-call and commerce operations. The
options are a fresh authorisation (requires contacting the customer) or writing the order off and
restocking. **Both are business decisions, not engineering ones.** Record which was chosen on the
incident.

## Why five minutes

The saga retries `COMMIT` and `CAPTURE` five times with backoff before the sweeper stops
hot-looping and pushes the deadline out. Five minutes is comfortably past that, so this alert
only fires once automatic recovery has genuinely been exhausted.

## Related

- [`docs/DESIGN-INVARIANTS.md`](../../docs/DESIGN-INVARIANTS.md) §1
- [`internal/saga/model_test.go`](../../internal/saga/model_test.go) — `SagaTimeoutAfterCommit` is the modelled bug
- [`outbox-backlog.md`](outbox-backlog.md), [`dlq.md`](dlq.md)
