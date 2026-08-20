# 3. No compensation past the point of no return

**Status:** Accepted · **Date:** 2026-08 · **Supersedes:** the original saga design

## Context

The obvious way to build a saga is to give every non-terminal state a timeout
that compensates. It is symmetric, it is easy to reason about, and every
tutorial does it that way.

We wrote `internal/saga/model_test.go` before the Go, and configured a variant with
that design (`OrderSagaBug.cfg`). the explorer produced this in nine steps:

```
saga PAID          -> emits inventory.commit
inventory COMMITTED -> stock is physically deducted
   (the Committed ack is delayed behind a consumer-group rebalance)
saga times out in PAID -> COMPENSATING -> emits Release, Void
payment VOIDED
Release arrives at inventory in state COMMITTED -> rejected
```

End state: stock deducted, customer not charged. The inventory row cannot be
un-committed because fulfilment may already have picked the item.

The trace is not exotic. A consumer-group rebalance during a deploy delays
acknowledgements by exactly the seconds needed, and deploys happen daily.

## Decision

**Emitting `inventory.commit` is the point of no return.** `PAID` and
`STOCK_COMMITTED` have no compensating transition. They retry forward with
backoff and, after five attempts, raise `orders_stuck` for a human.

A corollary fell out of the same model: the order of operations must be
**authorize → commit stock → capture**. Authorisation is reversible; capture is
not. Committing stock between them means capture only ever happens against
stock we already hold.

## Consequences

**Good.** The invariant is now enforced in four independent places — the state
machine has no such edge, the sweeper double-checks and refuses, a CHECK
constraint rejects the write, and `TestNoCompensationPastPointOfNoReturn`
enumerates every state/trigger pair. It is very hard to reintroduce by accident.

**Bad.** Some orders need a human. An order wedged in `STOCK_COMMITTED` with an
expired payment authorisation has no automated recovery, and
[`docs/runbooks/stuck-saga.md`](../runbooks/stuck-saga.md) says so plainly
rather than pretending otherwise. We accepted that: a rare manual settlement is
cheaper than a systematic way of giving stock away.

**Visible in the UI.** The admin saga inspector removes the cancel control past
the boundary rather than disabling it, and the storefront tells the customer
that the order is still processing rather than offering a cancel that would be
refused.

`OrderSagaBug.cfg` is kept and CI asserts it still **fails**. If it ever starts
passing, the model has been weakened and this decision is no longer protected.
