# 4. Five languages across eleven services

**Status:** Accepted · **Date:** 2026-08 · **Amended:** 2026-08 (was six; see *Revision*)

## Context

Five languages is a real cost, and it is easy to underestimate: five
toolchains, five dependency ecosystems, five sets of idioms in review, five ways
to get graceful shutdown subtly wrong. The default answer should be one, or two.

## Decision

Five, each with a reason that outweighs the cost:

| Language | Services | Why not the default |
|---|---|---|
| **Go** | order, inventory, payment | The saga and the reservation engine are both goroutine-shaped: HTTP API, Kafka consumer, outbox relay and timeout sweeper in one process sharing a pool. The saga's decision function is pure, which is what lets the state-space model call it directly. |
| **Java** | identity, catalog | Mature, transaction-heavy CRUD with deep validation needs. These are also the services most likely to be extended by the most people, and Spring is the shortest path for that. |
| **TypeScript** | cart, review, notification | High I/O, low CPU, shape-shifting JSON. They import `@souq/contracts` **verbatim** from the frontends, so a schema change breaks both sides in the same build. |
| **Python** | search, recommendation | Where the ecosystem is. The Elasticsearch DSL client and boto3's Personalize surface are first-class here and awkward everywhere else. |
| **C++** | pricing | The only synchronous dependency on the checkout path with a sub-millisecond budget, evaluating a rule set against a whole cart under a 250 ms deadline. |

## Revision: inventory moved from Rust to Go

`inventory-service` was originally Rust, and the argument for it was good: the
type system made the most dangerous mistake in the platform unrepresentable.
`events::enqueue` took `&mut Transaction`, so publishing an event before
committing the stock change *did not compile*.

Moving it to Go keeps most of that guarantee and loses some:

- **Kept.** `events.Enqueue` takes a `pgx.Tx`, not a pool. Publishing outside a
  transaction is still a compile error, because there is no pool-shaped
  overload to reach for.
- **Kept, and improved.** The reservation concurrency is now modelled in the
  same package as the code that implements it, and the oversell race runs
  against a real Postgres in the same `go test` invocation.
- **Lost.** Rust's borrow checker gave stronger guarantees about aliasing and
  lifetimes than Go's type system does. For this service — whose critical
  section is a single SQL statement, not in-process memory — that difference
  turned out to buy very little.
- **Gained.** One fewer toolchain, one fewer CI job with a 25-minute timeout,
  and a service every engineer on the team can review. The `platform`, `store`
  and `eventbus` packages are now the same shape as order-service's, which makes
  a fix in one obviously applicable to the other.

The safety property was never really in the language. It is in the conditional
`UPDATE` that Postgres evaluates inside a row latch, in the `no_oversell` CHECK
constraint, and in the tests that prove both. Those are unchanged.

## What makes five languages survivable

Not discipline. Three structural things:

1. **[`CONTRACTS.md`](../CONTRACTS.md) is normative.** Ports, event schemas, the
   error envelope, retry budgets and timeouts are specified once, in prose, and
   every language reimplements the same contract. Code that disagrees with it is
   wrong by definition.

2. **One error envelope.** Every 4xx and 5xx from every service in every
   language serialises to the same RFC 9457 shape. The storefront has one error
   path, not eleven.

3. **The same three reliability primitives everywhere.** Transactional outbox,
   consumer inbox, idempotency keys — in each language's idiom, but the same
   design, because [`DESIGN-INVARIANTS.md`](../DESIGN-INVARIANTS.md) §5 shows
   each is individually load-bearing.

## Consequences

**Bad, and unresolved.** Nobody is fluent in all five. A C++ change waits for
someone who can review it. Acceptable because pricing changes rarely and is a
place where being slow and careful is correct.

**Would we do it again?** C++ for pricing, yes. Java for identity and catalog is
the one worth revisiting — Go would have worked and would have made it four.
