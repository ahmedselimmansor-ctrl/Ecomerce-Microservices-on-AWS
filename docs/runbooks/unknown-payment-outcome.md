# Runbook: unknown payment outcome

**Severity:** page · **Alert:** `UnknownPaymentOutcome` · **Metric:** `souq_payment_unknown_outcome_total`

---

## What this means

We asked Paymob to move money and **we do not know whether it did**.

This is not a decline. A decline is a clear answer. This is the request timing out, the
connection dropping mid-response, or a 5xx after the bytes went out — cases where the charge may
well have succeeded and only the answer was lost.

The service handles this correctly on its own: the payment row stays in `AUTHORIZING` or
`CAPTURING`, **no event is emitted**, and the saga waits. That is deliberate. Emitting a failure
here would make the saga compensate against money that may have actually moved.

You are paged because a human has to ask Paymob what the truth is.

## Do not

- **Do not retry the authorisation by hand.** The service will do it with the same deterministic
  provider key (`internal/payment/psp_key.go`), which is what makes a retry safe. A manual retry
  through a different path can present a different key and charge the customer twice —
  `docs/DESIGN-INVARIANTS.md` §4 is precisely this failure.
- **Do not mark the payment `FAILED`.** If the charge did land, you have just told the saga to
  cancel an order the customer paid for.
- **Do not refund yet.** Refunding something that was never captured fails confusingly and
  muddies the reconciliation.

## Triage

**1. Which payments?**

```sql
SELECT p.id, p.order_id, p.state, p.amount, p.currency,
       p.psp_idempotency_key, p.created_at,
       a.outcome, a.error_message, a.created_at AS attempted_at
  FROM payments p
  JOIN payment_attempts a ON a.payment_id = p.id
 WHERE a.outcome IN ('UNKNOWN','TIMEOUT')
   AND p.state IN ('AUTHORIZING','CAPTURING')
 ORDER BY p.created_at DESC;
```

`psp_idempotency_key` is the value that was sent to Paymob as `merchant_order_id`. It is how you
look the payment up on their side.

**2. Ask Paymob what actually happened.**

```bash
TOKEN=$(curl -sS -X POST https://accept.paymob.com/api/auth/tokens \
  -H 'Content-Type: application/json' \
  -d "{\"api_key\":\"$PAYMOB_API_KEY\"}" | jq -r .token)

curl -sS -X POST https://accept.paymob.com/api/ecommerce/orders/transaction_inquiry \
  -H 'Content-Type: application/json' \
  -d "{\"auth_token\":\"$TOKEN\",\"merchant_order_id\":\"$PSP_KEY\"}" \
  | jq '{id, transactions: [.transactions[] | {id, success, pending, is_voided, is_refunded, amount_cents}]}'
```

Also check the Paymob dashboard — the transaction list, filtered by your merchant order id.

**3. Decide from what Paymob says.**

| Paymob says | Reality | Action |
|---|---|---|
| No order exists | The request never landed | Safe to let the service retry. It will. |
| Order exists, no transaction | Registered but never charged | Safe. The service re-issues a payment key on retry. |
| Transaction `success: true` | **The customer was charged** | Reconcile forward — see below |
| Transaction `pending: true` | Still in flight (wallet approval, 3-D Secure) | Wait. The callback will arrive. |
| Transaction `success: false` | Declined | Reconcile as failed — see below |

## Resolution

### The charge succeeded

The money is with Paymob. The saga is waiting for an event that was never emitted. Replay it
rather than writing state by hand — the reconciler exists for exactly this:

```bash
kubectl -n souq exec deploy/payment-service -- \
  /payment-service reconcile --payment-id "$PAYMENT_ID"
```

This re-queries Paymob, writes the outcome through the normal code path, and emits
`souq.payment.authorized.v1` through the outbox. The saga picks it up and continues.

If the reconciler is unavailable, the manual equivalent must go through the same transaction
boundary — a bare `UPDATE payments SET state=...` skips the outbox and leaves the saga waiting
forever, which converts one stuck order into one stuck order that *looks* fine.

### The charge was declined

```bash
kubectl -n souq exec deploy/payment-service -- \
  /payment-service reconcile --payment-id "$PAYMENT_ID"
```

Same command. It emits `souq.payment.failed.v1` and the saga compensates: inventory is released
and the order is cancelled. The customer sees "your payment was declined" and can retry.

### Paymob is unreachable

If you cannot get an answer at all, leave the payments in `AUTHORIZING`. That state is not
harmful:

- No stock is committed (the saga has not reached `PAID`).
- The reservation TTL releases the held stock after 15 minutes, so nothing is stranded.
- The customer sees checkout still in progress and, after 90 seconds, the storefront tells them
  it is taking longer than usual and that they will be emailed.

Escalate to Paymob support with the merchant order ids. Do not guess.

## How often is acceptable?

Non-zero but rare. A handful a week at meaningful volume is the cost of talking to a payment
network over the internet. A **spike** means Paymob is degraded — check
`souq_psp_latency_seconds` and their status page before treating each one individually.

## Why the alert has no `for:` duration

Every other alert waits to confirm a condition is sustained. This one fires immediately, because
a single unknown outcome is a customer who may have been charged for an order that is about to
be cancelled. One is worth looking at.

## Related

- [`payment-service internal/psp/paymob_test.go`](../../payment-service internal/psp/paymob_test.go)
- [`docs/DESIGN-INVARIANTS.md`](../../docs/DESIGN-INVARIANTS.md) §4 — why the provider key is derived, not random
- [`services/payment-service/README.md`](../../services/payment-service/README.md) — the Paymob integration
