# Runbook: oversell detected

**Severity:** page · **Alert:** `OversellDetected`

---

## What this means

A SKU has `reserved > on_hand`. We have promised units that do not exist.

**This should be impossible.** The `no_oversell` CHECK constraint on `stock_levels` is evaluated
by Postgres inside the same row latch as every write, so no application code path — correct or
buggy — can produce this row. `inventory-service internal/stock/model_test.go` proves the reservation strategy
safe, and `scripts/sql-check.sh` asserts in CI that the constraint rejects a direct `UPDATE`.

So if this alert is firing, one of these is true, in rough order of likelihood:

1. **The metric is wrong**, not the data. Check the database before anything else.
2. The constraint was dropped by a migration.
3. Something is writing to `stock_levels` outside the application — a manual fix, a data
   import, a replication artefact.
4. You are looking at a read replica with replication lag.

## Triage

**1. Confirm against the primary, not a replica.**

```sql
SELECT sku, on_hand, reserved, on_hand - reserved AS available, updated_at
  FROM stock_levels
 WHERE reserved > on_hand;
```

Empty? The metric is stale or is reading a replica. Fix the exporter; this is a monitoring bug,
not a commerce one. Downgrade and move on.

**2. Is the constraint still there?**

```sql
SELECT conname, pg_get_constraintdef(oid)
  FROM pg_constraint
 WHERE conrelid = 'stock_levels'::regclass AND conname = 'no_oversell';
```

If it returns nothing, **that is the incident.** Recreate it immediately — it will fail if any
row already violates it, which tells you the scope:

```sql
ALTER TABLE stock_levels ADD CONSTRAINT no_oversell CHECK (reserved <= on_hand);
```

**3. Who wrote the row?**

The ledger is append-only and records every movement:

```sql
SELECT created_at, movement, quantity, on_hand_after, reserved_after, actor, order_id, note
  FROM stock_ledger
 WHERE sku = '$SKU'
 ORDER BY created_at DESC
 LIMIT 50;
```

A jump in `reserved_after` with no matching ledger row means something wrote outside the
application. A row with `actor` set to a human name means a manual adjustment.

## Containment — first, stop selling it

Before investigating further, take the SKU out of sale. Every second it stays live is another
order you cannot fulfil.

```sql
UPDATE stock_levels SET status = 'SUSPENDED' WHERE sku = '$SKU';
```

`SUSPENDED` makes `try_take` refuse — the conditional UPDATE requires `status = 'ACTIVE'`. The
product stays visible in search but cannot be reserved, which is the right trade: hiding it looks
like we do not stock it, and the customer finds out at the point of adding to cart rather than at
checkout.

## Working out who is affected

```sql
-- Open reservations, oldest first. The oldest ones are the orders that
-- legitimately got the stock; the newest are the ones that cannot be filled.
SELECT r.id, r.order_id, ri.quantity, r.state, r.created_at
  FROM reservations r
  JOIN reservation_items ri ON ri.reservation_id = r.id
 WHERE ri.sku = '$SKU' AND r.state IN ('RESERVED','COMMITTED')
 ORDER BY r.created_at;
```

Sum `quantity` down the list until you exceed `on_hand`. Everything past that point is
unfulfillable.

For each unfulfillable order, the action depends on how far the saga got:

| Order status | Action |
|---|---|
| `PENDING`, `STOCK_RESERVED` | Cancel through the API. No money has moved. |
| `PAID`, `STOCK_COMMITTED`, `CONFIRMED` | **The customer has paid.** Refund and apologise; this is a commerce decision. Do not cancel the saga. |

## Correcting the count

Only after you know the real physical count — from the warehouse, not from the system:

```sql
-- Reconciliation, not a guess. `note` and the actor are mandatory: a stock
-- correction nobody can attribute is a correction nobody can audit.
BEGIN;
SET LOCAL souq.actor = 'your.name@souq.dev';
UPDATE stock_levels
   SET on_hand = $REAL_COUNT
 WHERE sku = '$SKU' AND $REAL_COUNT >= reserved;
-- If this affects 0 rows, the physical count is below what is already
-- promised. Cancel or refund the excess reservations FIRST.
COMMIT;
```

## After

An oversell that is real, rather than a metric bug, means a control failed. The postmortem needs
to answer:

- Which of the four causes above was it?
- If the constraint was dropped: why did CI not catch it? `scripts/sql-check.sh` asserts it
  exists, so either it did not run or the migration bypassed review.
- If something wrote outside the application: what, and does it still have write access?

## Related

- [`inventory-service internal/stock/model_test.go`](../../inventory-service internal/stock/model_test.go)
- [`docs/DESIGN-INVARIANTS.md`](../../docs/DESIGN-INVARIANTS.md) §3 — the two distinct oversell bugs
- [`scripts/sql-check.sh`](../../scripts/sql-check.sh) — the CI assertion that should have caught this
