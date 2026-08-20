# SOUQ Platform — Binding Interface Contracts (v1)

> **This document is normative.** Every service in this monorepo MUST conform to it.
> If code and this document disagree, the code is wrong. Changes require an ADR in `docs/adr/`.

Platform codename: **SOUQ**. Cloud target: **AWS (eu-west-1 primary, eu-central-1 DR)**.

---

## 1. Service Registry

| # | Service | Language / Runtime | Framework | HTTP | gRPC | Metrics | Owns (datastore) |
|---|---------|-------------------|-----------|------|------|---------|------------------|
| 1 | `identity-service` | Java 21 | Spring Boot 3.3 | 8081 | — | 8081/actuator/prometheus | PostgreSQL `identity` |
| 2 | `catalog-service` | Java 21 | Spring Boot 3.3 | 8082 | — | 8082/actuator/prometheus | PostgreSQL `catalog` |
| 3 | `cart-service` | Node 22 / TypeScript | Fastify 5 | 8083 | — | 8083/metrics | Redis (ElastiCache) |
| 4 | `order-service` | Go 1.23+ | chi + pgx | 8084 | — | 8084/metrics | PostgreSQL `orders` (+ outbox) |
| 5 | `inventory-service` | Go 1.23+ | chi + pgx | 8085 | — | 8085/metrics | PostgreSQL `inventory` |
| 6 | `payment-service` | Go 1.23+ | chi + pgx | 8086 | — | 8086/metrics | PostgreSQL `payments` |
| 7 | `search-service` | Python 3.12 | FastAPI | 8087 | — | 8087/metrics | OpenSearch/Elasticsearch |
| 8 | `recommendation-service` | Python 3.12 | FastAPI | 8088 | — | 8088/metrics | Amazon Personalize + Redis cache |
| 9 | `pricing-engine` | C++20 | gRPC (protobuf) | 8089 (health) | **9089** | 8089/metrics | In-memory rules + Redis |
| 10 | `review-service` | Node 22 / TypeScript | Fastify 5 | 8090 | — | 8090/metrics | MongoDB / DocumentDB `reviews` |
| 11 | `notification-service` | Node 22 / TypeScript | Fastify 5 | 8091 | — | 8091/metrics | Cassandra / Keyspaces `notifications` |

Frontends:

| App | Port | Stack |
|-----|------|-------|
| `apps/storefront` | 3000 | Next.js 15 (App Router), Tailwind v4, shadcn/ui, Zod, TanStack Query |
| `apps/admin` | 3001 | Next.js 15 (App Router), Tailwind v4, shadcn/ui, Zod, TanStack Table, Recharts |

**Database-per-service is absolute.** No service may open a connection to another service's
schema. Cross-service reads happen over HTTP/gRPC or via Kafka-materialised read models.

---

## 2. Cross-Cutting HTTP Conventions

### 2.1 Required request headers

| Header | Required | Meaning |
|--------|----------|---------|
| `Authorization: Bearer <jwt>` | on protected routes | RS256 JWT issued by `identity-service` |
| `X-Request-Id` | injected by gateway if absent | UUIDv4, echoed in every response and log line |
| `X-Correlation-Id` | optional | Ties a whole saga together; defaults to `X-Request-Id` |
| `Idempotency-Key` | **required on every non-GET mutation of money or stock** | Client-generated UUIDv4 |
| `traceparent` | injected | W3C Trace Context (OpenTelemetry) |

### 2.2 Error envelope (RFC 9457 Problem Details, extended)

Every 4xx/5xx from every service, in every language, MUST serialise to exactly this shape:

```json
{
  "type": "https://errors.souq.dev/inventory/insufficient-stock",
  "title": "Insufficient stock",
  "status": 409,
  "detail": "SKU SKU-4471 has 2 available, 5 requested",
  "instance": "/v1/reservations",
  "code": "INVENTORY_INSUFFICIENT_STOCK",
  "requestId": "6d2b...",
  "timestamp": "2026-08-17T10:00:00Z",
  "errors": [{ "field": "items[0].quantity", "message": "exceeds available stock" }]
}
```

`code` is a stable `SCREAMING_SNAKE_CASE` machine identifier. Frontend switches on `code`, never on `detail`.

### 2.3 Standard endpoints on every service

- `GET /health/live`  → `200 {"status":"UP"}` — process is alive (never touches dependencies)
- `GET /health/ready` → `200`/`503` — dependencies reachable (drives K8s readiness + LB target group)
- `GET /metrics`      → Prometheus text format
- `GET /v1/openapi.json` → the service's own OpenAPI 3.1 document

### 2.4 Pagination

Cursor-based everywhere: `?limit=20&cursor=<opaque-base64>`.
Response: `{ "items": [...], "nextCursor": "..." | null, "hasMore": bool }`.

### 2.5 Money

Money is **never** a float. Every amount crosses the wire as:

```json
{ "amount": 129900, "currency": "EUR" }   // minor units (cents), ISO-4217
```

Java: `long` + `Currency`. Go: `int64`. TS: `number` (safe, < 2^53). Python: `int`.
Postgres: `BIGINT` + `CHAR(3)`. Never `FLOAT`/`DOUBLE`/`NUMERIC` for transport.

### 2.6 Time

RFC 3339 UTC with `Z`, millisecond precision: `2026-08-17T10:00:00.000Z`. Postgres `TIMESTAMPTZ`.

---

## 3. Event Backbone (Amazon MSK / Kafka)

### 3.1 Topics

| Topic | Partitions | Key | Retention | Cleanup | Producer |
|-------|-----------|-----|-----------|---------|----------|
| `souq.order.events.v1` | 12 | `orderId` | 30d | delete | order-service |
| `souq.order.commands.v1` | 12 | `orderId` | 7d | delete | order-service (saga) |
| `souq.inventory.events.v1` | 12 | `sku` | 30d | delete | inventory-service |
| `souq.payment.events.v1` | 12 | `orderId` | 90d | delete | payment-service |
| `souq.catalog.events.v1` | 6 | `productId` | ∞ | **compact** | catalog-service |
| `souq.user.activity.v1` | 24 | `userId` | 7d | delete | storefront BFF, cart, search |
| `souq.notification.commands.v1` | 6 | `userId` | 7d | delete | order/payment/identity |
| `souq.<name>.dlq` | 3 | original key | 90d | delete | any consumer |

**Ordering guarantee:** per-key only. Never assume global order.
**Delivery:** at-least-once. Every consumer MUST be idempotent (§5.3).

### 3.2 CloudEvents envelope (mandatory for all topics)

Every Kafka message value is a JSON CloudEvents 1.0 structured envelope:

```json
{
  "specversion": "1.0",
  "id": "01J8Z3K9S2M4P6R8T0V2X4Y6A8",
  "source": "souq/order-service",
  "type": "souq.order.created.v1",
  "subject": "ord_01J8Z3K9S2",
  "time": "2026-08-17T10:00:00.000Z",
  "datacontenttype": "application/json",
  "traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
  "correlationid": "…",
  "dataschema": "https://schemas.souq.dev/order/created/v1.json",
  "data": { }
}
```

Kafka headers mirror `id`, `type`, `traceparent`, `correlationid` for cheap filtering.

### 3.3 Event type catalogue

**`souq.order.events.v1`**
| `type` | `data` |
|--------|--------|
| `souq.order.created.v1` | `{orderId, userId, items[{sku,quantity,unitPrice{amount,currency}}], total{...}, shippingAddress, idempotencyKey}` |
| `souq.order.confirmed.v1` | `{orderId, userId, paymentId, reservationId, confirmedAt}` |
| `souq.order.cancelled.v1` | `{orderId, userId, reason, failedStep}` |
| `souq.order.shipped.v1` | `{orderId, trackingNumber, carrier}` |
| `souq.order.delivered.v1` | `{orderId, deliveredAt}` |

**`souq.order.commands.v1`** (saga → participants)
| `type` | `data` |
|--------|--------|
| `souq.inventory.reserve.v1` | `{orderId, reservationId, items[{sku,quantity}], ttlSeconds}` |
| `souq.inventory.release.v1` | `{orderId, reservationId, reason}` |
| `souq.inventory.commit.v1` | `{orderId, reservationId}` |
| `souq.payment.authorize.v1` | `{orderId, paymentId, userId, amount{...}, paymentMethodToken}` |
| `souq.payment.capture.v1` | `{orderId, paymentId}` |
| `souq.payment.void.v1` | `{orderId, paymentId, reason}` |

**`souq.inventory.events.v1`**
`souq.inventory.reserved.v1` `{orderId, reservationId, items[], expiresAt}` ·
`souq.inventory.reservation_failed.v1` `{orderId, reservationId, reason, unavailable[{sku,requested,available}]}` ·
`souq.inventory.released.v1` · `souq.inventory.committed.v1` ·
`souq.inventory.stock_changed.v1` `{sku, available, reserved, onHand}`

**`souq.payment.events.v1`**
`souq.payment.authorized.v1` `{orderId, paymentId, amount{}, authCode, provider}` ·
`souq.payment.failed.v1` `{orderId, paymentId, reason, declineCode, retriable}` ·
`souq.payment.captured.v1` · `souq.payment.voided.v1` · `souq.payment.refunded.v1`

**`souq.catalog.events.v1`** (compacted; drives search index + read models)
`souq.catalog.product_upserted.v1` `{productId, sku, title, description, brand, categoryPath[], attributes{}, images[], price{}, status, updatedAt}` ·
`souq.catalog.product_deleted.v1` `{productId}` ·
`souq.catalog.price_changed.v1` `{productId, sku, oldPrice{}, newPrice{}}`

**`souq.user.activity.v1`** (Amazon Personalize ingest)
`souq.activity.viewed.v1` · `souq.activity.added_to_cart.v1` · `souq.activity.purchased.v1` · `souq.activity.searched.v1`
`data`: `{userId|anonymousId, sessionId, itemId?, query?, occurredAt, deviceType, locale}`

**`souq.notification.commands.v1`**
`souq.notify.email.v1` `{userId, template, locale, params{}, dedupeKey}` · `souq.notify.sms.v1` · `souq.notify.push.v1`

---

## 4. The Order Saga (orchestration, owned by `order-service`)

Chosen over choreography because it is centrally observable and **formally verifiable**
(see `internal/saga/model_test.go`).

```
  POST /v1/orders
        │
        ▼
   [PENDING] ──Reserved──► [STOCK_RESERVED] ──Authorized──► [PAID]
        │                        │                            │
   ReserveFailed             AuthFailed                    Commit
   or timeout                or timeout                       │
        │                        │                            ▼
        │                        │                   [STOCK_COMMITTED]
        ▼                        ▼                            │
   [CANCELLED] ◄──Released── [COMPENSATING]                 Capture
                  + Voided                                    │
                                                              ▼
                                                        [CONFIRMED]
   ◄────────── roll back ──────────┤├────────── roll forward only ──────────►
                    (the "point of no return" is sending Commit)
```

| State | Entered on | Timeout | On timeout |
|-------|-----------|---------|------------|
| `PENDING` | order accepted | 30 s | → `COMPENSATING` (send Release) |
| `STOCK_RESERVED` | `inventory.reserved` | 120 s | → `COMPENSATING` (send Release **and** Void) |
| `PAID` | `payment.authorized` | — | **no rollback**; retry `Commit` with backoff, alert after 5 |
| `STOCK_COMMITTED` | `inventory.committed` | — | **no rollback**; retry `Capture`, alert after 5 |
| `CONFIRMED` | `payment.captured` | terminal | — |
| `COMPENSATING` | any failure before Commit | 300 s | escalate to `orders_stuck` alert |
| `CANCELLED` | compensation acknowledged | terminal | — |

> **The point of no return.** Once the saga emits `inventory.commit`, compensation is
> *forbidden* — it must roll forward. `internal/saga/model_test.go` produces a counterexample if
> you allow a timeout to compensate from `PAID`: inventory commits, the `Committed` event is
> delayed, the saga times out and voids the payment, and you end with stock deducted and no
> money. See `docs/DESIGN-INVARIANTS.md` §1.

**Order of operations is load-bearing:** authorize → commit stock → capture. Authorization is
reversible (void), capture is not; committing stock between them means capture can only ever
happen on stock we already own.

**Invariants (proved by exhaustive state-space search, asserted in integration tests):**
1. **No money without stock** — `payment.captured` ⇒ `inventory.committed` for the same `orderId`.
2. **No stock without money** — `inventory.committed` ⇒ `payment.authorized`.
3. **No oversell** — Σ active reservations per SKU ≤ `on_hand`.
4. **No dangling reservation** — every reservation reaches `committed` or `released` (TTL guarantees liveness).
5. **Termination** — every order reaches `CONFIRMED` or `CANCELLED`.

Reservation TTL: **900 s**, swept by `inventory-service` every 30 s.

---

## 5. Reliability Primitives (every service implements all three)

### 5.1 Transactional Outbox

Any service publishing an event as part of a DB write MUST use:

```sql
CREATE TABLE outbox (
  id             BIGSERIAL PRIMARY KEY,
  aggregate_type TEXT        NOT NULL,
  aggregate_id   TEXT        NOT NULL,
  event_id       UUID        NOT NULL UNIQUE,
  event_type     TEXT        NOT NULL,
  topic          TEXT        NOT NULL,
  partition_key  TEXT        NOT NULL,
  payload        JSONB       NOT NULL,
  headers        JSONB       NOT NULL DEFAULT '{}',
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at   TIMESTAMPTZ,
  attempts       INT         NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unpublished_idx ON outbox (created_at) WHERE published_at IS NULL;
```

A background relay (`FOR UPDATE SKIP LOCKED`, batch 100, 200 ms tick) publishes and stamps
`published_at`. Business write + outbox insert are in **one** transaction. See `internal/eventbus/outbox_model_test.go`.

### 5.2 Inbox / consumer dedup

```sql
CREATE TABLE processed_events (
  event_id     UUID PRIMARY KEY,
  consumer     TEXT        NOT NULL,
  processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```
Insert-first, `ON CONFLICT DO NOTHING`; zero rows affected ⇒ already handled ⇒ ack and skip.
Retention 30 d via partition drop.

### 5.3 Idempotency keys (HTTP)

```sql
CREATE TABLE idempotency_keys (
  key            TEXT PRIMARY KEY,
  user_id        TEXT        NOT NULL,
  endpoint       TEXT        NOT NULL,
  request_hash   TEXT        NOT NULL,
  response_code  INT,
  response_body  JSONB,
  state          TEXT        NOT NULL DEFAULT 'IN_PROGRESS',  -- IN_PROGRESS|COMPLETED
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at     TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '24 hours'
);
```
Same key + same `request_hash` → replay stored response. Same key + different hash → `409 IDEMPOTENCY_KEY_REUSE`.
`IN_PROGRESS` on a concurrent duplicate → `409 REQUEST_IN_PROGRESS`.

### 5.4 Timeouts, retries, breakers (defaults — override only with a comment justifying it)

| Concern | Default |
|---------|---------|
| HTTP client connect / total timeout | 1 s / 3 s |
| gRPC deadline | 250 ms (pricing), 2 s (other) |
| DB statement timeout | 3 s (OLTP), 30 s (admin/report) |
| Retries | 3 attempts, exponential backoff base 100 ms, factor 2, **full jitter**, cap 2 s |
| Retry only on | connect errors, 502/503/504, gRPC `UNAVAILABLE`/`DEADLINE_EXCEEDED` |
| Never retry on | 400/401/403/404/409/422 |
| Circuit breaker | open after 50 % failures over 20-request rolling window; half-open probe after 10 s |
| Bulkhead | max 50 concurrent calls per downstream dependency |
| Kafka consumer | 5 retries → DLQ with `x-dlq-reason`, `x-dlq-attempts`, `x-dlq-original-topic` headers |
| Graceful shutdown | drain 20 s, `SIGTERM` → fail readiness immediately, finish in-flight |

---

## 6. Data Ownership Matrix

| Store | AWS service | Owner | Contents |
|-------|------------|-------|----------|
| PostgreSQL `identity` | Aurora PostgreSQL Serverless v2 | identity | users, credentials, roles, refresh tokens, MFA |
| PostgreSQL `catalog` | Aurora PostgreSQL | catalog | products, variants, categories, media, price history |
| PostgreSQL `orders` | Aurora PostgreSQL | order | orders, order_items, saga_state, outbox, idempotency |
| PostgreSQL `inventory` | Aurora PostgreSQL | inventory | stock_levels, reservations, stock_ledger, outbox |
| PostgreSQL `payments` | Aurora PostgreSQL | payment | payments, payment_attempts, refunds, ledger, outbox |
| MySQL `analytics_ops` | Aurora MySQL | admin/BI | denormalised reporting marts, merchant settlements |
| MongoDB `reviews` | Amazon DocumentDB | review | reviews, ratings aggregates, Q&A, moderation queue |
| Cassandra `notifications` | Amazon Keyspaces | notification | delivery log, user activity feed (wide, time-series) |
| Redis | ElastiCache Serverless | cart / pricing / search | carts, sessions, rate limits, hot-product cache, price cache |
| OpenSearch | Amazon OpenSearch Service | search | `products-v*` index behind `products` alias |
| S3 | S3 | catalog / admin | product media, invoices, exports, Personalize datasets |

**Consistency posture:** strong (single-row/single-partition ACID) inside a service;
**eventual** across services — target convergence p99 < 2 s. The UI is written to expect
staleness (see §8).

---

## 7. AuthN / AuthZ

- **Access token**: JWT RS256, 15 min TTL, keys from AWS KMS, JWKS at `GET /v1/.well-known/jwks.json`.
- **Refresh token**: opaque 256-bit, hashed (Argon2id) in Postgres, 30 d TTL, **rotating with reuse detection**.
- Claims: `sub`, `iss:"https://auth.souq.dev"`, `aud:["souq-api"]`, `exp`, `iat`, `jti`, `roles[]`, `scope`, `sid`, `ver`.
- Roles: `CUSTOMER`, `MERCHANT`, `SUPPORT`, `OPS`, `ADMIN`.
- Every service verifies the JWT **locally** against cached JWKS (5 min TTL). No auth round-trip per request.
- Service-to-service inside the mesh: mTLS (SPIFFE IDs) + `X-Service-Identity`.
- Admin dashboard requires `ADMIN`/`OPS` **and** an `amr` containing `mfa`.

---

## 8. Frontend Contract

- `apps/storefront` talks **only** to the BFF layer in its own Next.js Route Handlers
  (`/api/bff/*`), which fan out to services. Browsers never call services directly.
- Every BFF response is parsed with a **Zod** schema in `apps/storefront/src/lib/schemas/`
  before it reaches a component. A parse failure is a 502, logged with `requestId`.
- Zod schemas mirror this document; they are the runtime enforcement of it.
- Optimistic UI + eventual consistency: after a mutation the UI shows a `pending` badge and
  reconciles via polling/SSE rather than assuming the write is globally visible.
- shadcn/ui components live in `src/components/ui/`, never edited by feature code.

---

## 9. Observability

- **Traces**: OpenTelemetry OTLP → ADOT collector (DaemonSet) → AWS X-Ray. Trace context
  propagates through Kafka via the `traceparent` header.
- **Metrics**: Prometheus scrape → AMP. Every service exports RED (`http_server_requests_seconds`)
  plus its own domain metrics (`souq_orders_total{status}`, `souq_reservation_conflicts_total`, …).
- **Logs**: JSON to stdout → Fluent Bit → CloudWatch Logs. Mandatory fields:
  `timestamp, level, service, version, requestId, correlationId, traceId, spanId, userId?, msg`.
- **SLOs**: checkout availability 99.95 %; `POST /v1/orders` p99 < 800 ms; search p99 < 200 ms;
  saga convergence p99 < 2 s. Error budget burn alerts at 2 % / 5 % / 10 %.

---

## 10. Naming & Layout Rules

- IDs: prefixed ULIDs — `usr_`, `prd_`, `sku_`, `ord_`, `pay_`, `rsv_`, `crt_`, `rev_`.
- Kafka consumer groups: `<service>.<purpose>` e.g. `search-service.catalog-indexer`.
- Env vars: `SOUQ_<AREA>_<NAME>` e.g. `SOUQ_DB_URL`, `SOUQ_KAFKA_BROKERS`, `SOUQ_REDIS_URL`.
- Container images: `<registry>/souq/<service>:<git-sha>`.
- K8s: namespace `souq`, labels `app.kubernetes.io/{name,version,component,part-of=souq}`.
- Every service directory contains: `Dockerfile`, `README.md`, `Makefile`, tests, and
  a `k8s/` folder with `deployment.yaml`, `service.yaml`, `hpa.yaml`, `pdb.yaml`.

---

## 11. Local Development

`docker compose up` at the repo root starts: Postgres, MySQL, MongoDB, Cassandra, Redis,
Kafka (KRaft), Elasticsearch, Kibana, LocalStack, Jaeger, Prometheus, Grafana, and all
11 services + 2 frontends. Ports as per §1. Seed with `make seed`.
