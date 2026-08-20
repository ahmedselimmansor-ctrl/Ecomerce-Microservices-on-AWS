# Runbook: illegal saga transition

**Severity:** page · **Alert:** `IllegalSagaTransition` · **Metric:** `souq_saga_illegal_transitions_total`

---

## What this means

The order saga attempted a state transition that `internal/saga/model_test.go` proves cannot happen.

This is not a load problem, a network problem, or a flake. It means **the implementation has
diverged from the design that was proved correct**. Every other alert in this repository says
"something is unhealthy"; this one says "an assumption we built on is false".

The state machine refused the transition, so it did *not* corrupt anything on the way through.
What it does mean is that the machine received a trigger it has no rule for, which usually
means one of:

1. A participant emitted an event the orchestrator does not model — a version skew where one
   service deployed ahead of another.
2. A saga state was written by something other than the state machine.
3. A genuine bug: somebody added a transition without adding it to the model.
4. Two orchestrator replicas raced and one applied a transition against a stale read (this
   should produce `ErrVersionStale` instead — if you see it here, the optimistic lock is broken).

## Do not

- **Do not restart order-service.** Restarting loses nothing but tells you nothing either, and
  the metric resets, which destroys the evidence for how widespread this is.
- **Do not manually update `orders.status`.** The CHECK constraint will reject an unmodelled
  state, but a *modelled* state written by hand skips the outbox and leaves a saga with no
  command in flight — a permanently wedged order that looks healthy.
- **Do not roll back before you know the blast radius.** Rolling back to a version that emits
  different events can make a version skew worse.

## Triage — first 5 minutes

**1. How many, and which transition?**

```bash
kubectl -n souq exec deploy/order-service -- \
  wget -qO- localhost:8084/metrics | grep souq_saga_illegal_transitions_total
```

The `from` and `trigger` labels tell you exactly which rule is missing. Match them against the
transition table in `services/order-service/internal/saga/machine.go`.

**2. Find the affected orders.**

The orchestrator logs every refusal at `ERROR` with the message
`ILLEGAL SAGA TRANSITION — see internal/saga/model_test.go`.

```bash
kubectl -n souq logs deploy/order-service --since=1h \
  | grep 'ILLEGAL SAGA TRANSITION' \
  | jq -r '[.orderId, .from, .trigger, .eventId] | @tsv'
```

**3. Is money or stock at risk?**

This is the question that decides whether this is a page or a ticket.

```sql
-- Run against the orders database.
SELECT o.id, o.status, o.total, o.placed_at,
       s.step, s.state, s.attempts
  FROM orders o
  JOIN saga_steps s ON s.order_id = o.id
 WHERE o.id = ANY($1)
 ORDER BY o.placed_at;
```

- Orders in `PENDING` or `STOCK_RESERVED` — **no money has moved.** Compensation is still legal.
- Orders in `PAID`, `STOCK_COMMITTED` or `CONFIRMED` — **past the point of no return**
  (`docs/DESIGN-INVARIANTS.md` §1). Do not compensate. These roll forward or a human settles them.

## Most likely cause: version skew

Check whether the services are on the same build:

```bash
kubectl -n souq get pods -o custom-columns=\
'POD:.metadata.name,IMAGE:.spec.containers[0].image' | sort
```

If a participant is ahead of order-service and is emitting a new event type, order-service
logs `ignoring unmodelled event type` rather than raising this alert — so a skew that produces
*this* alert means the participant is emitting an event type the orchestrator knows but does not
accept in that state.

**Action:** roll the lagging service forward, not the leading one back. Rolling back a
participant mid-saga leaves in-flight orders waiting for a reply that the older build will never
send.

## Resolution

### If it is a missing transition (a genuine bug)

1. Add the case to `machine.go` **and** the corresponding action to `internal/saga/model_test.go`.
2. `./go test ./...` — if the model now finds a counterexample, the transition you were about
   to add is unsafe and the bug is elsewhere.
3. `go test ./internal/saga/` — `TestStatesMatchTheFormalModel` and
   `TestEveryReachableStateCanTerminate` must both pass.
4. Deploy. Stuck orders resume on the next sweeper tick.

### If orders are wedged

Orders that hit an illegal transition stay in their previous state and keep being swept. Once
the fix is deployed they resolve on their own. To confirm:

```sql
SELECT status, count(*) FROM orders
 WHERE placed_at > now() - interval '2 hours'
 GROUP BY status ORDER BY count DESC;
```

`PENDING` and `STOCK_RESERVED` counts should fall to near zero within a few minutes.

### If you must intervene on a specific order

Only for orders **before** the point of no return, and only through the API — never with SQL:

```bash
curl -X POST "https://api.souq.dev/v1/orders/$ORDER_ID/cancel" \
  -H "Authorization: Bearer $OPS_TOKEN"
```

This goes through the state machine, writes to the outbox, and emits the compensation commands.
A direct `UPDATE` does none of that.

## After

This alert firing means the model and the code disagreed and nothing caught it before
production. That is a gap in CI, not just a bug.

- Add the interleaving that produced it as a case in `machine_test.go`.
- If the model needed changing, note it in `docs/DESIGN-INVARIANTS.md` — the next person needs to know
  why the transition exists.
- Ask why `TestStatesMatchTheFormalModel` did not catch it. It compares *states*, not
  *transitions*; if this keeps happening, that test needs to compare the transition table too.

## Related

- [`internal/saga/model_test.go`](../../internal/saga/model_test.go) — the model
- [`docs/DESIGN-INVARIANTS.md`](../../docs/DESIGN-INVARIANTS.md) §1 — why there is no rollback past Commit
- [`stuck-saga.md`](stuck-saga.md) — when an order is wedged but no transition was refused
