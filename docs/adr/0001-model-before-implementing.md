# 1. Model the saga exhaustively before writing it

**Status:** Accepted · **Date:** 2026-08 · **Amended:** 2026-08 (see *Revision*)

## Context

Checkout is a distributed transaction across three independent databases with
no two-phase commit. The failure modes are interleavings, and interleavings are
exactly what code review and unit tests are worst at: a reviewer checks whether
each branch is right, not whether some ordering of two hundred branches produces
a state nobody considered.

The alternative was the usual one — write it, test the paths we thought of, and
find the rest in production over eighteen months.

## Decision

Model the parts where a wrong interleaving costs money, before writing them.
Four models: the order saga, reservation concurrency, payment idempotency, and
outbox delivery.

Two rules make this more than a documentation exercise:

1. **Bugs stay as tests.** Several tests assert that a *known-bad* design still
   fails. A suite where nothing can fail is worse than no suite.

2. **The models drive the real code.** `sagaReact` in the saga model calls
   `saga.Next()` — the same function the production orchestrator calls. There is
   no separate artefact that can drift out of step, because the model *is* the
   implementation plus an adversarial environment.

A third rule came out of using it: **assert coverage, not state counts.** A
green model that explored the wrong thing is worse than a red one, and a guard
that is accidentally always false gives a small, clean, meaningless result. The
tests assert that specific situations *actually occurred* — a timeout firing
mid-flight, a release overtaking its reserve — rather than that some number of
states was reached.

## What it actually found

Five design bugs, before any of them was written
([`DESIGN-INVARIANTS.md`](../DESIGN-INVARIANTS.md)). Two are worth naming,
because neither would have been found by testing:

- **§1** Compensating past the commit loses stock. Needs a ten-step trace with a
  delayed acknowledgement. Nobody writes that test.
- **§3b** A lost update that keeps `reserved <= on_hand` true. It passes the
  obvious invariant and double-sells the stock anyway. We would have shipped it
  and found out in the warehouse.

## Revision: from TLA+ to Go

The original version of this work used TLA+ and TLC. It found the same five
bugs. It was replaced with [`libs/go-modelcheck`](../../libs/go-modelcheck) — a
~250-line breadth-first state-space explorer — for reasons worth recording,
because it is a genuine trade and not a straight improvement.

**What was given up.** TLA+ is more expressive. It has temporal logic,
refinement, fairness, and a mature checker with symmetry reduction and
distributed execution. For a model an order of magnitude larger, none of that is
replaceable.

**What was gained, and why it mattered more here.**

- *No drift.* The TLA+ spec described the saga; `machine.go` implemented it.
  Nothing enforced that they agreed. There was a test parsing the `.tla` file to
  compare state names, which is a weak bridge: two artefacts can agree on names
  and disagree on transitions. The Go model calls `Next()` directly, so a change
  to the code is a change to what is verified.
- *No second toolchain.* CI needed a JVM and a downloaded jar for one job.
- *Everyone can read it.* One person on the team could confidently modify the
  TLA+. Everyone can modify a Go table-driven test, which means the models get
  maintained rather than quietly abandoned.
- *One command.* `go test ./...` runs the models alongside everything else.

**The honest summary:** we traded expressive power we were not using for a
guarantee that the model and the code cannot diverge. On a larger or more
subtle protocol that trade would go the other way.

## Consequences

**Cost.** About three days of modelling, and the models need maintaining
alongside the code. They add a few seconds to CI.

**Limits, stated honestly.** The search is exhaustive over a *finite* model —
one order, three transactions — and says nothing about arbitrary N. It has no
notion of time, so it cannot tell us 30 s is the right `PENDING` timeout, only
that firing it early is safe.

**What we would not do again.** Modelling everything. Catalogue CRUD and review
moderation do not need this and models written for them would rot. Model where
being wrong is expensive *and* being wrong is not obvious.
