# 6. Use `merchant_order_id` as the Paymob idempotency mechanism

**Status:** Accepted · **Date:** 2026-08

## Context

`payment-service internal/psp/paymob_test.go` §4 showed that our own idempotency table is not
sufficient to prevent a double charge. An atomic `INSERT ... ON CONFLICT`, 409s
for concurrent duplicates, and a reaper that only fires on a crashed owner —
all correct — still produce this:

```
r1 wins the key -> IN_PROGRESS
r1 calls the provider -> MONEY MOVES
r1 crashes before writing COMPLETED
reaper expires the abandoned key -> ABSENT
r2 wins the key -> presents a FRESH provider key -> MONEY MOVES AGAIN
```

The reaper cannot distinguish "crashed before charging" from "crashed after",
and it cannot be removed — without it a customer whose payment crashed can
never retry. The fix has to be that the retry presents the **same key to the
provider**.

Stripe and Adyen accept an `Idempotency-Key` header. **Paymob does not.**

## Options considered

1. **Do nothing and rely on our table.** Rejected: the model shows it fails.
2. **A distributed lock held across the provider call.** Rejected: a lock held
   across a network call to a third party either expires mid-call (and does
   nothing) or outlives a crashed holder (and blocks the retry). It moves the
   problem rather than solving it.
3. **Use `merchant_order_id`.** Paymob enforces uniqueness on it at order
   registration and rejects a duplicate. Chosen.

## Decision

`merchant_order_id` carries our deterministic key
(`sha256`-derived, `internal/payment/psp_key.go`). On a duplicate rejection we
**look the order up and return its real outcome** instead of charging again.

That lookup is not error handling. It is the safety property, and
`TestDuplicateOrderDoesNotChargeTwice` fails loudly if it stops working.

## Consequences

**Weaker than a real idempotency header, and we say so.** The window between
"order registered" and "transaction created" is not covered by
`merchant_order_id`. A reconciler closes it by querying Paymob for anything
left in `AUTHORIZING` or `CAPTURING`.

**One place does string matching on an error message.** `isDuplicateOrder`
matches on Paymob's several wordings for "already exists". It is fragile and
it is deliberate: the alternative is treating a duplicate as a generic failure,
which makes the saga retry into a rejection it can never clear. A false
negative is caught by the reconciler; a false positive only causes a harmless
lookup.

**The derivation is pinned.** `TestDerivationIsPinned` fixes the output shape.
Changing it changes a value Paymob has already seen for every in-flight
payment, so it needs a migration plan, not a golden-value update.
