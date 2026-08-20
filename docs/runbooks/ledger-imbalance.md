# Runbook: ledger imbalance

**Severity:** page · **Alert:** `LedgerImbalance`

The double-entry ledger does not balance. Every movement of money writes a debit and a credit
sharing an `entry_group`; the `unbalanced_entry_groups` view is empty iff every group sums to
zero.

Finance reconciles against this table, not against `payments`. A non-empty view means the books
are wrong and month-end will fail.

## Triage

```sql
SELECT * FROM unbalanced_entry_groups;
```

Then look at the offending group:

```sql
SELECT id, payment_id, order_id, account, direction, amount, currency, description, created_at
  FROM ledger_entries
 WHERE entry_group = '$GROUP'
 ORDER BY created_at;
```

Almost always one of:

1. **A half-written pair.** One side committed and the other did not — which should be
   impossible, since both are inserted in the same transaction. If you see this, look for code
   writing to `ledger_entries` outside `WriteLedger`.
2. **A currency mismatch within a group.** The view groups by `(entry_group, currency)`, so a
   debit in EGP and a credit in USD shows as two unbalanced groups.
3. **A manual insert.** Check whether anyone has been correcting data by hand.

## Do not

- **Do not delete the offending rows.** The ledger is append-only; deleting evidence of a
  discrepancy does not fix it, it hides it.
- **Do not "balance" it with a plug entry** until you know what actually happened to the money.

## Resolution

Establish the truth from the provider first — Paymob's transaction list is authoritative for
whether money moved, not our tables. Then write a **correcting pair** with a description saying
what it corrects:

```sql
INSERT INTO ledger_entries (payment_id, order_id, account, direction, amount, currency, entry_group, description)
VALUES
  ('$PAYMENT','$ORDER','psp_clearing','DEBIT',  $AMOUNT,'EGP', gen_random_uuid(), 'correction for group $GROUP — INC-1234'),
  ('$PAYMENT','$ORDER','revenue',     'CREDIT', $AMOUNT,'EGP', <same uuid>,       'correction for group $GROUP — INC-1234');
```

Correcting entries, never edits. That is what makes the ledger auditable.

## Related
- [`services/payment-service/migrations/0001_init.sql`](../../services/payment-service/migrations/0001_init.sql)
- [`scripts/sql-check.sh`](../../scripts/sql-check.sh) — asserts the view is empty for a balanced pair
