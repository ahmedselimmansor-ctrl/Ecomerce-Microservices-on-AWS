# 2. Orchestration, not choreography

**Status:** Accepted · **Date:** 2026-08

## Context

Two ways to run a saga. In **choreography** each service reacts to events and
emits its own; there is no coordinator. In **orchestration** one service owns
the state machine and sends commands.

Choreography is the fashionable answer and it has real advantages: no central
component, no single point of failure, services stay decoupled.

## Decision

Orchestration, in `order-service`.

Three reasons, in order of weight:

**1. It can be model-checked.** A choreographed saga has no single state
machine to write down — the state is emergent from the interaction of five
services. That is precisely what makes it hard to reason about, and it makes
[ADR-0001](0001-model-before-implementing.md) impossible. Given that the model
found five real bugs, this outweighs everything else.

**2. "Where is this order?" has an answer.** With orchestration it is one row
plus a `saga_steps` table, which is what the admin saga inspector renders. With
choreography, answering it means correlating logs across five services and
inferring what each of them thinks. Support asks this question dozens of times
a day.

**3. The point of no return is expressible.** [ADR-0003](0003-no-rollback-past-commit.md)
depends on one component knowing that `PAID` has no compensating edge. In a
choreographed saga each participant decides its own compensation locally, and
"do not compensate from here" is a convention nobody can enforce.

## Consequences

**Good.** One place to look, one place to change, one thing to prove correct.
The orchestrator is stateless — all saga state is in Postgres — so any replica
can pick up any order after a rebalance or a restart.

**Bad.** `order-service` is on the critical path for every checkout. Mitigated
by keeping it genuinely stateless and running at least three replicas across
zones, but the coupling is real and we accepted it.

**Bad.** The orchestrator knows about the participants' commands, which is a
form of coupling choreography avoids. In practice the command set has changed
twice in the life of the project and both were additive.

**Not chosen but noted:** a workflow engine (Temporal, Step Functions) would
give durable execution for free. Rejected because the state machine is about
two hundred lines and the operational surface of another distributed system is
not.
