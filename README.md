# SOUQ — E-Commerce Microservices on AWS

A distributed commerce platform. **Eleven backend services across five languages**, two Next.js
frontends, an event backbone, and a checkout flow whose correctness was established by exhaustive
state-space search *before* any of it was written.

**417 files · ~63,800 lines · 275 automated assertions passing · all 13 container images build.**

```bash
make check    # the whole gate: models → contracts → tests → lint → frontend → images → k8s
```

---

## Contents

- [The part that matters](#the-part-that-matters)
- [System architecture](#system-architecture)
- [Checkout: the distributed transaction](#checkout-the-distributed-transaction)
- [Database architecture](#database-architecture)
- [Entity relationships](#entity-relationships)
- [Event flow](#event-flow)
- [Verified, not asserted](#verified-not-asserted)
- [Quick start](#quick-start)
- [Why each service is in the language it is in](#why-each-service-is-in-the-language-it-is-in)
- [Failure-aware by construction](#failure-aware-by-construction)
- [Repository layout](#repository-layout)

---

## The part that matters

Checkout is a distributed transaction across three independent databases with no two-phase commit.
It is not obviously correct, so it was **modelled and model-checked before it was implemented**.

Four exhaustive state-space models in [`libs/go-modelcheck`](libs/go-modelcheck/) cover the places
where a wrong interleaving costs money. **Five design decisions in this repository exist because a
model produced a counterexample** — written up in
[`docs/DESIGN-INVARIANTS.md`](docs/DESIGN-INVARIANTS.md), each with a config that still reproduces
the original bug.

The most consequential:

> **You must not compensate after the point of no return.**
>
> The natural implementation gives every non-terminal saga state a rollback timeout. The explorer
> finds the trace in nine steps: inventory commits, the acknowledgement is delayed behind a
> consumer rebalance, the saga times out and voids the payment — and you end with stock deducted
> and nothing charged for it.
>
> So `PAID` and `STOCK_COMMITTED` have **no compensating transition**. They retry forward or page a
> human. That rule is enforced in three independent places: the
> [state machine](services/order-service/internal/saga/machine.go), the
> [sweeper](services/order-service/internal/orchestrator/sweeper.go), and a
> [CHECK constraint](services/order-service/migrations/0001_init.sql).

```bash
make models
```

Several of these tests assert that a **known-bad design still fails**. If one ever passes, an
invariant has been silently weakened and the build breaks — which is the point.

---

## System architecture

```mermaid
graph TB
    subgraph edge["🌐 Edge — CloudFront + WAF"]
        CF["CloudFront CDN<br/><i>static assets, product pages</i>"]
        WAF["AWS WAF<br/><i>rate limits, bot rules</i>"]
    end

    subgraph fe["Frontend — Next.js 15 · App Router"]
        SF["storefront :3000<br/>Tailwind · shadcn/ui · Zod<br/><b>11 pages · 13 BFF routes</b>"]
        AD["admin :3001<br/><b>KPIs · saga inspector</b><br/>catalogue CRUD · DLQ"]
    end

    subgraph mesh["EKS — Kubernetes, 13 workloads"]
        direction TB

        subgraph tx["Transaction path — where being wrong costs money"]
            ORD["order-service :8084<br/><b>Go</b> · saga orchestrator"]
            INV["inventory-service :8085<br/><b>Go</b> · reservations"]
            PAY["payment-service :8086<br/><b>Go</b> · Paymob + ledger"]
        end

        subgraph crud["CRUD & session"]
            IDN["identity-service :8081<br/><b>Java 21</b> · JWT + MFA"]
            CAT["catalog-service :8082<br/><b>Java 21</b> · products"]
            CRT["cart-service :8083<br/><b>Node 22</b> · Redis CAS"]
            REV["review-service :8090<br/><b>Node 22</b>"]
            NTF["notification-service :8091<br/><b>Node 22</b>"]
        end

        subgraph disc["Discovery & pricing"]
            SRCH["search-service :8087<br/><b>Python 3.12</b>"]
            RECO["recommendation-service :8088<br/><b>Python 3.12</b>"]
            PRC["pricing-engine :9089<br/><b>C++20</b> · gRPC"]
        end
    end

    subgraph bus["Amazon MSK — Kafka event backbone"]
        K[("7 topics · 24 message types<br/>CloudEvents 1.0 envelopes")]
    end

    subgraph data["Data plane"]
        PG[("Aurora PostgreSQL ×5<br/><i>database per service</i>")]
        MY[("Aurora MySQL<br/><i>analytics marts</i>")]
        DOC[("DocumentDB<br/><i>reviews</i>")]
        KEY[("Keyspaces<br/><i>delivery log</i>")]
        RDS[("ElastiCache Redis<br/><i>carts · sessions</i>")]
        OS[("OpenSearch<br/><i>products alias</i>")]
        S3[("S3<br/><i>media · exports</i>")]
        PZ[("Personalize<br/><i>recommendations</i>")]
    end

    WAF --> CF
    CF --> SF & AD

    SF -.->|"BFF only — the browser<br/>never calls a service"| IDN & CAT & CRT & ORD & SRCH & RECO & REV
    AD -.-> IDN & CAT & ORD & INV & PAY

    ORD <-->|"saga commands<br/>+ replies"| K
    INV <--> K
    PAY <--> K
    CAT --> K
    K --> SRCH
    K --> NTF
    K --> RECO

    CRT -->|"gRPC · 250 ms deadline<br/>falls back to list price"| PRC

    IDN --> PG
    CAT --> PG
    ORD --> PG
    INV --> PG
    PAY --> PG
    CRT --> RDS
    REV --> DOC
    NTF --> KEY
    SRCH --> OS
    RECO --> PZ
    CAT --> S3
    AD --> MY

    classDef go fill:#00ADD8,stroke:#007d9c,color:#fff
    classDef java fill:#f89820,stroke:#b36b00,color:#fff
    classDef node fill:#3c873a,stroke:#2b5f29,color:#fff
    classDef py fill:#3776ab,stroke:#245078,color:#fff
    classDef cpp fill:#659ad2,stroke:#41699b,color:#fff
    classDef fe fill:#111,stroke:#444,color:#fff
    classDef store fill:#f1f3f5,stroke:#adb5bd,color:#212529

    class ORD,INV,PAY go
    class IDN,CAT java
    class CRT,REV,NTF node
    class SRCH,RECO py
    class PRC cpp
    class SF,AD fe
    class PG,MY,DOC,KEY,RDS,OS,S3,PZ store
```

**The dashed lines are the boundary that matters.** The browser never calls a service directly
([`docs/CONTRACTS.md`](docs/CONTRACTS.md) §8). Every request goes through a Route Handler in the
Next.js app, which fans out and **validates every response with a Zod schema before a component
sees it**. That buys three things: tokens stay out of JavaScript, a service that starts returning
`available: null` produces one clean 502 with a request id, and eleven services in five languages
become one origin with no CORS surface.

---

## Checkout: the distributed transaction

`POST /v1/orders` returns **202**, not 201. The saga has started; it has not finished.

```mermaid
sequenceDiagram
    autonumber
    participant C as Storefront
    participant O as order-service
    participant I as inventory-service
    participant P as payment-service
    participant M as Paymob

    C->>O: POST /v1/orders (Idempotency-Key)
    O->>O: write order + outbox row<br/>ONE transaction
    O-->>C: 202 Accepted — poll or subscribe

    rect rgba(46,160,67,0.10)
        note over O,I: Reversible. A timeout here compensates safely.
        O->>I: inventory.reserve
        I->>I: UPDATE … WHERE on_hand - reserved >= qty
        I-->>O: reserved (or INSUFFICIENT_STOCK)

        O->>P: payment.authorize
        P->>M: authorise (deterministic idempotency key)
        M-->>P: authorised
        P-->>O: authorised
    end

    rect rgba(248,81,73,0.10)
        note over O,P: PAST THE POINT OF NO RETURN.<br/>No compensating transition exists —<br/>the saga rolls forward or pages a human.
        O->>I: inventory.commit
        I-->>O: committed
        O->>P: payment.capture
        P->>M: capture
        M-->>P: captured
        P-->>O: captured
    end

    O->>O: status = CONFIRMED
    O-->>C: SSE / poll → confirmed
```

Everything in the green band is reversible: a timeout releases the reservation and voids the
authorisation. Everything in the red band is not, and
[`DESIGN-INVARIANTS.md`](docs/DESIGN-INVARIANTS.md) §1 is the proof that treating it as reversible
loses money.

### The saga state machine

```mermaid
stateDiagram-v2
    [*] --> PENDING

    PENDING --> STOCK_RESERVED: reserved
    PENDING --> COMPENSATING: timeout / rejected

    STOCK_RESERVED --> PAID: authorised
    STOCK_RESERVED --> COMPENSATING: timeout / declined

    PAID --> STOCK_COMMITTED: committed
    STOCK_COMMITTED --> CONFIRMED: captured

    COMPENSATING --> CANCELLED: released + voided

    CONFIRMED --> SHIPPED
    SHIPPED --> DELIVERED
    CONFIRMED --> REFUNDED

    CANCELLED --> [*]
    DELIVERED --> [*]
    REFUNDED --> [*]

    note right of PAID
        No edge back to COMPENSATING
        from PAID, STOCK_COMMITTED or
        CONFIRMED. Enforced in the state
        machine, the sweeper, AND a
        CHECK constraint — three places,
        independently.
    end note
```

---

## Database architecture

**Database per service, not schema per service.** Five separate Aurora clusters. A shared cluster
means a runaway query in review moderation degrades checkout, and it means the boundary is a
convention that one `JOIN` at 2am erases.

```mermaid
graph LR
    subgraph own["Each service owns its store outright"]
        direction TB

        subgraph s1[" "]
            IDN2["identity-service"] --> DB1[("Aurora PG<br/><b>identity</b><br/>8 tables")]
        end
        subgraph s2[" "]
            CAT2["catalog-service"] --> DB2[("Aurora PG<br/><b>catalog</b><br/>6 tables")]
        end
        subgraph s3[" "]
            ORD2["order-service"] --> DB3[("Aurora PG<br/><b>orders</b><br/>6 tables")]
        end
        subgraph s4[" "]
            INV2["inventory-service"] --> DB4[("Aurora PG<br/><b>inventory</b><br/>6 tables")]
        end
        subgraph s5[" "]
            PAY2["payment-service"] --> DB5[("Aurora PG<br/><b>payments</b><br/>7 tables")]
        end
    end

    subgraph poly["Polyglot where the access pattern demands it"]
        direction TB
        CRT2["cart-service"] --> R[("Redis<br/><i>CAS via Lua<br/>TTL-expiring carts</i>")]
        REV2["review-service"] --> D[("DocumentDB<br/><i>nested, schema-flexible</i>")]
        NTF2["notification-service"] --> K2[("Keyspaces<br/><i>wide time-series<br/>delivery log</i>")]
        SRCH2["search-service"] --> O2[("OpenSearch<br/><i>inverted index<br/>+ facets</i>")]
    end

    DB1 -.->|"events only —<br/>never a cross-database JOIN"| BUS(("Kafka"))
    DB2 -.-> BUS
    DB3 -.-> BUS
    DB4 -.-> BUS
    DB5 -.-> BUS
    BUS -.-> O2
    BUS -.-> K2

    classDef store fill:#f1f3f5,stroke:#adb5bd,color:#212529
    class DB1,DB2,DB3,DB4,DB5,R,D,K2,O2 store
```

| Store | AWS service | Owner | Why this store |
|---|---|---|---|
| PostgreSQL `identity` | Aurora Serverless v2 | identity | Transactional, low volume, spiky. Serverless suits a login curve |
| PostgreSQL `catalog` | Aurora PostgreSQL | catalog | Relational with JSONB attributes; GIN indexes for the category tree |
| PostgreSQL `orders` | Aurora PostgreSQL | order | The saga needs single-row ACID and a `CHECK` that outlives the code |
| PostgreSQL `inventory` | Aurora PostgreSQL | inventory | One conditional `UPDATE` in one row latch is the oversell guarantee |
| PostgreSQL `payments` | Aurora PostgreSQL | payment | Double-entry ledger. Finance reconciles against it |
| MySQL `analytics_ops` | Aurora MySQL | admin/BI | Denormalised marts; separate so a report cannot slow a checkout |
| MongoDB `reviews` | DocumentDB | review | Nested, schema-flexible documents with per-product aggregates |
| Cassandra `notifications` | Keyspaces | notification | Wide, append-only time series. Wrong shape for a relational store |
| Redis | ElastiCache | cart / pricing | Carts expire. A TTL and a compare-and-set are the whole access pattern |
| OpenSearch | OpenSearch Service | search | An inverted index and facets are not something SQL does well |

---

## Entity relationships

Relationships **within** a service are foreign keys. Relationships **across** services are ids with
no constraint behind them — because there cannot be one, and pretending otherwise is how a
distributed system acquires a hidden monolith.

```mermaid
erDiagram
    USERS ||--|| CREDENTIALS : "argon2id hash"
    USERS ||--o{ ROLES : has
    USERS ||--o{ REFRESH_TOKENS : "rotating, reuse-detected"
    USERS ||--o{ MFA_RECOVERY_CODES : "hashed, single use"

    CATEGORIES ||--o{ CATEGORIES : "parent_id + path[]"
    CATEGORIES ||--o{ PRODUCTS : contains
    PRODUCTS ||--o{ VARIANTS : "one row per SKU"
    VARIANTS ||--o{ PRICE_HISTORY : "written by a TRIGGER"

    ORDERS ||--o{ ORDER_ITEMS : contains
    ORDERS ||--o{ SAGA_STEPS : "one row per participant call"

    STOCK_LEVELS ||--o{ STOCK_LEDGER : "append-only audit"
    RESERVATIONS ||--o{ RESERVATION_ITEMS : holds

    PAYMENTS ||--o{ PAYMENT_ATTEMPTS : "one per PSP call"
    PAYMENTS ||--o{ REFUNDS : "capped at captured"
    PAYMENTS ||--o{ LEDGER_ENTRIES : "double-entry, must balance"

    USERS {
        text id PK "usr_ULID"
        text email UK "UNIQUE on lower(email)"
        bool mfa_enabled "CHECK: implies a secret"
    }
    VARIANTS {
        text sku PK "sku_ULID"
        bigint price "minor units, CHECK >= 0"
        bigint list_price "CHECK >= price"
        int available "NULL = unknown, not zero"
    }
    ORDERS {
        text id PK "ord_ULID"
        text user_id "cross-service, no FK"
        text status "CHECK: no rollback past PAID"
        text idempotency_key UK
    }
    STOCK_LEVELS {
        text sku PK
        int on_hand
        int reserved "CHECK: on_hand - reserved >= 0"
    }
    PAYMENTS {
        text id PK "pay_ULID"
        text order_id UK "one payment per order"
        bigint captured_amount "CHECK <= amount"
        text psp_idempotency_key UK "HMAC-derived, never random"
    }
```

**The three constraints worth reading twice**, each verified against real Postgres by
`make sql-check`:

```sql
-- inventory: the oversell guarantee. One statement, one row latch.
UPDATE stock_levels SET reserved = reserved + $2
 WHERE sku = $1 AND status = 'ACTIVE' AND on_hand - reserved >= $2;

-- catalog: a "was 999, now 1299" strikethrough is a regulator's problem
-- in most markets. The schema makes it unrepresentable.
CHECK (list_price IS NULL OR list_price >= price)

-- orders: the point of no return, restated where no code path can bypass it.
CHECK (status NOT IN ('PAID','STOCK_COMMITTED','CONFIRMED') OR compensated_at IS NULL)
```

> **50 concurrent Postgres connections racing 10 units at 2 each → exactly 5 winners,
> `reserved == 10`**, and the `no_oversell` CHECK rejected a direct `UPDATE` issued outside the
> application. Not asserted — executed, by `make sql-check`.

---

## Event flow

Every service writes events through a **transactional outbox**, and each of the three parts is
individually load-bearing —
[`outbox_model_test.go`](services/order-service/internal/eventbus/outbox_model_test.go) shows what
breaks when any one is removed.

```mermaid
flowchart LR
    A["Business write<br/><i>UPDATE orders</i>"] --> T{{"ONE transaction"}}
    B["Outbox row<br/><i>INSERT outbox</i>"] --> T
    T --> DB[("Postgres")]

    DB --> R["Relay<br/><i>FOR UPDATE SKIP LOCKED</i><br/>publishes, then marks"]
    R --> K(("Kafka"))
    K --> C["Consumer"]
    C --> IB{{"Inbox<br/><i>INSERT … ON CONFLICT<br/>DO NOTHING</i>"}}
    IB --> H["Handler runs<br/><b>exactly once in effect</b>"]

    R -.->|"can crash between<br/>publish and mark"| DUP["duplicate delivery<br/><i>a guarantee, not an edge case</i>"]
    DUP -.->|"made harmless by"| IB

    style T fill:#d3f9d8,stroke:#2f9e44
    style IB fill:#d3f9d8,stroke:#2f9e44
    style DUP fill:#ffe3e3,stroke:#e03131
```

| Topic | Partitions | Key | Retention | Produced by |
|---|---|---|---|---|
| `souq.order.events.v1` | 12 | `orderId` | 7d | order |
| `souq.inventory.events.v1` | 12 | `sku` | 7d | inventory |
| `souq.payment.events.v1` | 6 | `orderId` | 30d | payment |
| `souq.catalog.events.v1` | 6 | `productId` | ∞ **compacted** | catalog |
| `souq.notification.commands.v1` | 6 | `userId` | 7d | order / payment / identity |
| `souq.activity.v1` | 12 | `sessionId` | 30d | storefront |
| `souq.dlq.v1` | 3 | original key | 30d | every consumer |

The catalogue topic is **compacted**, and that single fact shapes its payloads: Kafka keeps only
the newest message per key, so every product event carries **full current state, not a delta** — a
consumer rebuilding its index sees exactly one message per product. A delete emits a real Kafka
tombstone (a genuinely null payload) so the key eventually leaves the topic entirely.

---

## Verified, not asserted

Everything below was executed in this repository. The command is given so you can re-run it.

| Check | Result | Command |
|---|---|---|
| Saga state machine: exhaustive reachability, idempotency of every transition, no-rollback-past-commit | ✅ | `make models` |
| **50 concurrent buyers, 2 units each, 10 in stock → exactly 5 winners** | ✅ | `make sql-check` |
| 66 schema invariants against real Postgres — every one a write that must be rejected | ✅ | `make sql-check` |
| **Paymob**: 40 assertions — 7 forged-callback variants rejected, duplicate order does not double-charge, transport failure maps to UNKNOWN | ✅ | `cd services/payment-service && go test ./internal/psp/` |
| Payment idempotency: 11 assertions on the deterministic provider-key derivation | ✅ | `go test ./internal/payment/` |
| C++ pricing engine: 55 assertions, clean under `-Wall -Wextra -Wpedantic -Werror` | ✅ | `cd services/pricing-engine && ./build/rules_test` |
| Zod contracts: 29 tests, including one that parses `machine.go` and asserts the Go and TypeScript saga states agree | ✅ | `make contracts` |
| TypeScript ↔ Python contract parity: 11 models, 23 error codes, field by field | ✅ | `make contracts` |
| **All 13 container images build** — ten of them did not before | ✅ | `make images` |
| Storefront and admin typecheck, produce a production build, and have no dead internal links | ✅ | `make frontend` |
| Java cross-reference: 57 files, 217 types, every call resolves against a declared type | ✅ | `make java-check` |
| Kubernetes: 13 workloads pass immutable-tag, memory-limit, distinct-probe, non-root, read-only-rootfs, PDB and NetworkPolicy checks | ✅ | `make k8s-check` |
| Go builds, vets and `gofmt`s clean across all four modules | ✅ | `make lint` |

**Every check in this repository was verified by deliberately breaking the thing it exists to catch
and confirming that it fails.** Two of them were silently passing while doing nothing until that
was done — a check that always passes is not a check, it is a claim.

---

## Quick start

```bash
make up          # datastores, Kafka, LocalStack — waits for health
make seed        # demo catalogue, stock, users
make up-services # all 11 backend services
make up-frontend # storefront :3000, admin :3001
make smoke       # end-to-end checkout
```

`make` on its own lists every target.

**Toolchains.** Node 22+, Go 1.25+, Docker, Python 3.12 and a C++20 compiler cover everything
except `mvn verify` and `make tf-plan`, which need a JDK and Terraform. Targets skip cleanly when a
toolchain is absent rather than failing.

Without a JDK, `make java-check` still resolves every type name and call in the Java sources
against the types this repository declares. It is not a compiler, and
[`scripts/java-check.py`](scripts/java-check.py) states exactly what it does and does not catch —
but it exists because CI once went green on a controller calling a method that was never written.

**The admin app** requires an `ADMIN` or `OPS` role **and** a session with MFA
([`docs/CONTRACTS.md`](docs/CONTRACTS.md) §7). A role survives a stolen password; a second factor
does not.

---

## Why each service is in the language it is in

Five languages is a real cost — five toolchains, five dependency ecosystems, five sets of idioms in
review. Each is here for a reason that outweighs it:

| Service | Language | Reason |
|---|---|---|
| **order** | Go | The saga orchestrator. Goroutines map cleanly onto "HTTP API + Kafka consumer + outbox relay + timeout sweeper in one process", and the state machine is a pure function that is exhaustively testable |
| **inventory** | Go | The hottest row in the platform and the most expensive bug. [`events.Enqueue`](services/inventory-service/internal/events/events.go) takes a `pgx.Tx`, not a pool, so **publishing before committing does not compile** |
| **payment** | Go | Same concurrency shape, and the [deterministic PSP key derivation](services/payment-service/internal/payment/psp_key.go) is 60 lines that must be readable in full by anyone reviewing them |
| **identity, catalog** | Java 21 / Spring Boot 3.3 | Heavy transactional and validation needs. Both use **JDBC, not JPA** — every write is a single conditional statement whose exact SQL is the correctness argument, and an ORM that splits one into a select-then-update reintroduces the race the `WHERE` clause closes |
| **cart, review, notification** | Node 22 / TypeScript | Shape-shifting JSON, high I/O, low CPU. They share [`@souq/contracts`](libs/ts-contracts/) verbatim with the frontends, so a schema change breaks the build on both sides at once |
| **search, recommendation** | Python 3.12 | Where the ecosystem is: the Elasticsearch DSL client and boto3's Personalize surface |
| **pricing** | C++20 | The only synchronous dependency on the checkout hot path with a sub-millisecond budget. Evaluates a rule set against a whole cart under a 250 ms gRPC deadline, and falls back to list price rather than ever failing a cart |

---

## Failure-aware by construction

Bounded retries with **full jitter**, circuit breakers, bulkheads, DLQs carrying `x-dlq-reason`
headers, readiness-before-drain on `SIGTERM`, and liveness probes that never touch a dependency —
because a probe a database blip can fail restarts every pod at once and turns a brownout into an
outage.

**Degradation is explicit, not implicit.** `pricingDegraded`, `fallback` and `degraded` are fields
in the contract:

| When | What happens | What the user sees |
|---|---|---|
| pricing-engine unreachable | Cart falls back to list price | Promotional messaging hidden; prices still chargeable |
| OpenSearch unavailable | Postgres `LIKE` fallback | "Search is running in a reduced mode" — facets gone, everything still purchasable |
| Personalize cold | Bestseller ranking | Heading changes from "Recommended for you" to "Popular right now" |
| inventory has not reported | `available` is `null`, not `0` | **Add to cart stays enabled.** The reservation is the authority; refusing a sale on a stale null costs more than a rejected reservation |

Selling at list price is a bad day. Failing checkout is worse.

### Eventual consistency, stated honestly

Strong consistency holds inside a service. Across services it is eventual, targeting p99 < 2 s. The
UI is written to expect that:

- `ProductVariant.available` is denormalised from inventory and is a **display hint only**.
- Checkout returns **202**, not 201. The client polls `/v1/orders/{id}/status` or subscribes to SSE.
- Every order records the `rulesVersion` its totals were priced against, so it can be re-priced
  identically at capture even if a promotion expired mid-checkout.

---

## Repository layout

| Path | What is in it |
|---|---|
| [`libs/go-modelcheck/`](libs/go-modelcheck/) | Exhaustive state-space explorer, the four models, and the configs that reproduce each original counterexample |
| [`docs/CONTRACTS.md`](docs/CONTRACTS.md) | **Normative.** Ports, event schemas, error envelope, retry budgets, data ownership. Code that disagrees with it is wrong |
| [`docs/DESIGN-INVARIANTS.md`](docs/DESIGN-INVARIANTS.md) | The five design decisions that came out of counterexamples, each with its regression test |
| [`docs/runbooks/`](docs/runbooks/) | Ten runbooks. Every alert links to one, and every link is verified to resolve |
| [`contracts/`](contracts/) | AsyncAPI 3.0 for every topic; protobuf for the pricing gRPC API |
| [`libs/ts-contracts/`](libs/ts-contracts/) · [`libs/py-contracts/`](libs/py-contracts/) | The contract as Zod schemas and as Pydantic models, kept in step by `scripts/contract-parity.py` |
| [`services/`](services/) | The eleven backend services |
| [`apps/`](apps/) | storefront and admin |
| [`infra/terraform/`](infra/terraform/) | VPC, EKS, Aurora, MSK, OpenSearch, ElastiCache, DocumentDB, Keyspaces, CloudFront, Personalize, KMS |
| [`infra/k8s/`](infra/k8s/) | Kustomize base and dev/prod overlays covering all 13 workloads |
| [`scripts/`](scripts/) | The checks: `sql-check`, `k8s-check`, `image-check`, `java-check`, `link-check`, `contract-parity` |

---

## Status

Read [`STATUS.md`](STATUS.md) for the honest breakdown — what is complete, what is verified with
the command to re-run each check, and what is not built.

The short version: the formal layer, the contracts, all eleven services, both frontends, the AWS
infrastructure, Kubernetes, CI and the runbooks are complete and verified. Three things are
deliberately absent, and `STATUS.md` names each: the **browser half** of the Paymob integration
(which must be Paymob's own iframe so a card number never reaches this origin), recovery-code
sign-in, and a caller for `GetPrices`.
