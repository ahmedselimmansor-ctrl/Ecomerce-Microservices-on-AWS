# Design invariants

Five design decisions that are not obvious, each of which exists because an
exhaustive state-space search produced a counterexample for the obvious
alternative.

Each one has a **regression test that reproduces the original bug**. Those tests
assert the bad design still fails; if one of them ever passes, the protection
described here is gone and the invariant has been silently weakened.

| § | Invariant | Regression test |
|---|-----------|-----------------|
| 1 | No compensation past the point of no return | `order-service`: `TestRollbackAfterCommitIsUnsafe` |
| 2 | Compensation must tombstone | `order-service`: `TestWithoutTombstonesTheSagaCanWedge` |
| 3 | Reservation is one conditional statement | `inventory-service`: `TestNaiveRelativeOversells`, `TestNaiveAbsoluteLosesUpdatesWithoutOverselling` |
| 4 | The provider idempotency key is derived, never random | `payment-service`: `TestSameLogicalPaymentAlwaysYieldsTheSameKey`, `TestDuplicateOrderDoesNotChargeTwice` |
| 5 | Outbox, relay and inbox are each individually load-bearing | `order-service`: `TestOutboxDeliveryIsEffectivelyOnce` and its three negative cases |

## How these are checked

[`libs/go-modelcheck`](../libs/go-modelcheck) is a breadth-first state-space
explorer. Given an initial state and a set of actions it enumerates every
reachable state, checks every invariant at each one, and reports the **shortest**
action sequence that produced a violation.

It also checks a liveness property that matters for sagas: from every reachable
state, is a terminal state still reachable? A system that can wedge in some
corner of its state space passes every safety check and still strands customers.

Three properties of this setup are worth stating:

**The models drive the real code.** `sagaReact` in the saga model calls
`saga.Next()` — the same function the production orchestrator calls. There is no
separate specification that can drift, because the specification *is* the
implementation plus an adversarial environment.

**The adversary is explicit.** Messages are never removed from the modelled
network, so any handler can be re-triggered forever (at-least-once). Every
enabled action is explored at every state, so all delivery orders are covered.
Timeouts can fire while a reply is in flight. Participants can simply never be
scheduled.

**Coverage is asserted, not counted.** A green model that explored the wrong
thing is worse than a red one, and a guard that is accidentally always false
gives a small, clean, meaningless result. So the tests assert that specific
situations *actually occurred* — a timeout firing mid-flight, a release
overtaking its reserve — rather than that some number of states was reached.

```bash
cd services/order-service     && go test ./internal/saga/ -v
cd services/inventory-service && go test ./internal/stock/ -v
cd services/payment-service   && go test ./... -v
```

---

## §1 — Compensation after the point of no return loses money

**Reproduced by:** `TestRollbackAfterCommitIsUnsafe`
**Violated invariant:** `NoStockWithoutMoney`

The natural implementation gives every non-terminal saga state a timeout that
rolls back. It is symmetric, easy to reason about, and wrong. The explorer finds
this in ten steps:

```
 1. saga.start                   -> PENDING, Reserve sent
 2. inv.reserve.ok               -> inventory RESERVED
 3. saga.observe:Reserved
 4. saga.react:Reserved          -> STOCK_RESERVED, Authorize sent
 5. pay.authorize.ok             -> payment AUTHORIZED
 6. saga.observe:Authorized
 7. saga.react:Authorized        -> PAID, Commit sent
 8. inv.commit                   -> inventory COMMITTED   <-- stock is gone
 9. saga.timeout.ROLLBACK        -> COMPENSATING, Release + Void sent
10. pay.void                     -> payment VOIDED
```

End state: stock deducted, customer not charged. The `Release` that arrives at
inventory afterwards is rejected, because a committed reservation cannot be
un-committed — fulfilment may already have picked the item.

The trace is not exotic. A consumer-group rebalance during a deploy delays the
acknowledgement by exactly the seconds needed, and deploys happen daily.

**Decision.** Emitting `inventory.commit` is the point of no return. `PAID` and
`STOCK_COMMITTED` have **no compensating transition**; they retry forward with
backoff and raise `orders_stuck` after five attempts.

A corollary from the same search: the order of operations must be
**authorize → commit stock → capture**. Authorisation is reversible, capture is
not. Committing stock between them means capture only ever happens against stock
we already hold.

**Enforced in four independent places.** The state machine has no such edge; the
sweeper double-checks and refuses; a `CHECK` constraint rejects the write;
`TestNoCompensationPastPointOfNoReturn` enumerates every state/trigger pair. It
is hard to reintroduce by accident.

---

## §2 — Compensation can overtake the thing it compensates

**Reproduced by:** `TestWithoutTombstonesTheSagaCanWedge`
**Symptom:** a wedged state — reachable, but no terminal state reachable from it

Remove the tombstone actions and the explorer finds this in five steps:

```
1. saga.start        -> Reserve queued, consumer lagging
2. saga.timeout      -> COMPENSATING, Release sent
3. inv: Release arrives, no such reservation, "nothing to do", ignored
4. inv: Reserve finally processed -> RESERVED
5. nothing will ever release it. The saga waits for a Released that
   will never come.
```

**Decision.** `Release` and `Void` against an unknown id must write a
**tombstone**, not no-op. A subsequent `Reserve` or `Authorize` that finds a
tombstone is rejected.

This is why `inventory.reservations` and `payments` both allow a row to be
created directly in `RELEASED` / `VOIDED` state — which looks strange in the
schema until you have read this.

The TTL sweeper still exists, but as defence in depth. The search shows it is
not load-bearing for liveness, and "the saga released it" presumes the saga is
running — a presumption that fails during an incident, which is exactly when
stock is most contended.

---

## §3 — Two different oversell bugs, and only one is caught by the obvious test

**Reproduced by:** `TestNaiveRelativeOversells`, `TestNaiveAbsoluteLosesUpdatesWithoutOverselling`

### §3a — the visible one

`SELECT reserved` … check … `UPDATE SET reserved = reserved + q`. Two buyers, 2
units, 2 each. Both read `reserved = 0`, both pass the check, both increment.
The row lands at 4. **Violates `NoOversell` in four steps.**

### §3b — the one that hides

Same, but `UPDATE SET reserved = <value read> + q`. This one **passes
`NoOversell`**: the column never exceeds `on_hand`, so a test asserting
"reserved ≤ on_hand" goes green. It violates `Conservation` — two committed
reservations, only one counted. The units are double-sold and the row looks
healthy; the failure surfaces days later in the warehouse.

**Decision.** The invariant the service asserts is `Conservation`, not
`NoOversell`. `NoOversell` alone is too weak to catch a lost update.

**Decision.** The shipped implementation is a single conditional statement:

```sql
UPDATE stock_levels
   SET reserved = reserved + $2, updated_at = now()
 WHERE sku = $1 AND status = 'ACTIVE' AND on_hand - reserved >= $2
```

Postgres evaluates the predicate and the increment inside the same row latch, so
there is no window between them at all.

`SELECT ... FOR UPDATE` is also safe but serialises every buyer of a hot SKU
behind one lock — the throughput collapse lands exactly on the flash sale you
built it for. `TestRowLockIsSafeButSerialises` makes the cost visible: the
locked model reaches 31 states where the lock-free one reaches 10, and that
difference is concurrency given up.

**Verified empirically too.** `TestNoOversellUnderRealConcurrency` races 50 real
Postgres connections for 10 units at 2 each: exactly 5 winners, `reserved == 10`,
and the `no_oversell` CHECK rejects even a direct `UPDATE` issued outside the
application.

---

## §4 — Our idempotency table is not what makes payments safe

**Reproduced by:** `TestDuplicateOrderDoesNotChargeTwice`

This is the result that was most surprising. A payment service can have an
atomic `INSERT ... ON CONFLICT DO NOTHING` owning the key, 409s for concurrent
duplicates, and a reaper that only fires on a crashed owner — everything by the
book — and still double-charge:

```
1. r1 wins the key         -> IN_PROGRESS
2. r1 calls the provider   -> money moves
3. r1 crashes before writing COMPLETED
4. the reaper expires the abandoned key -> ABSENT
5. r2 wins the key         -> IN_PROGRESS
6. r2 calls the provider with a FRESH key -> money moves again
```

The reaper cannot distinguish "crashed before charging" from "crashed after" —
that information exists only at the provider. And the reaper cannot be deleted:
without it, a customer whose payment crashed can never retry.

**Decision.** The provider idempotency key is **derived deterministically** from
ours: `HMAC-SHA256(salt, operation ‖ orderId ‖ idempotencyKey)`, computed once
and stored on the row *before* the call. Never `uuid.New()` at the call site.
See [`psp_key.go`](../services/payment-service/internal/payment/psp_key.go).

**Paymob has no idempotency header**, which is where this gets interesting.
`merchant_order_id` — the one field Paymob enforces uniqueness on — carries the
derived key instead, and a duplicate rejection triggers a lookup that returns the
*real* outcome rather than charging again. See
[ADR-0006](adr/0006-paymob-merchant-order-id.md).

**The mirror image.** A sloppy SELECT-then-INSERT with a *deterministic* provider
key does **not** double-charge — the provider catches what our table missed.
Neither layer is individually sufficient and each covers the other's gap, which
is the argument for keeping both rather than "simplifying" one away later.

---

## §5 — Commit-then-publish loses events, and dedup does not help

**Reproduced by:** the three negative cases in `TestOutboxDeliveryIsEffectivelyOnce`

### §5a — the dual write

`COMMIT; then producer.send()`. Violated in two steps: the business row is
durable and the event never existed. The consumer in that model is *perfectly*
well behaved and dedups correctly; it changes nothing. No amount of
consumer-side engineering fixes a producer-side gap.

### §5b — the missing inbox

Correct outbox, but no `processed_events` table. The relay's
crash-after-publish-before-mark path is a **guarantee** of the design, not an
edge case, so the side effect runs twice: two reservations for one order, or two
"your order shipped" emails.

### §5c — the missing relay

An outbox row that nothing drains is a durable event nobody ever receives.

**Decision.** All three parts are mandatory, in every service, in every
language. This is why [`CONTRACTS.md`](CONTRACTS.md) §5.1 and §5.2 are written as
absolutes rather than recommendations — each is individually load-bearing and
the failure is invisible in testing.

In the Go services this is enforced by the type system: `events.Enqueue` takes
a `pgx.Tx`, not a pool, so publishing outside a transaction does not compile.
There is no pool-shaped overload to reach for.

---

## What these models do not cover

Honest limits, so nobody reads a green run as more than it is.

- **Finite instances only.** One order in the saga model; three transactions in
  the inventory model. The search is exhaustive *for those sizes* and says
  nothing about arbitrary N. Orders do not interact, so the small-scope
  hypothesis is strong for the saga. The inventory model *does* have
  cross-transaction interaction, and three concurrent transactions is the point
  past which no new interleaving class appears for a single row.
- **No time, only ordering.** Timeouts are "may fire", never durations. The model
  cannot tell you 30 s is the right `PENDING` timeout — only that firing it early
  is safe.
- **No Byzantine behaviour.** Participants may be slow, crash, or duplicate.
  They do not lie.
- **Design, not code — except where noted.** The saga model closes that gap by
  calling `Next()` directly. The inventory model reasons about SQL semantics
  rather than calling the function, because the property is a property of the
  database's row latch; the empirical counterpart is the 50-connection race
  against real Postgres.
- **A previous version of this work used TLA+.** It found the same five bugs. It
  was replaced with Go for a specific reason: a separate specification language
  proves more and proves it better, but it needs its own toolchain in CI, its own
  reviewers, and a bridge to the code that nothing enforces. Expressing the model
  in Go against the real decision function means it cannot drift, every engineer
  can read and change it, and it runs in the same `go test` as everything else.
  That is a real trade — less expressive power for a guarantee that the model
  and the code stay in step.
