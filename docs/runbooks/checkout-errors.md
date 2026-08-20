# Runbook: checkout error budget burning

**Severity:** page (14x burn) / ticket (6x burn) · **Alert:** `CheckoutErrorBudgetBurningFast`

The SLO is 99.95% availability on `POST /v1/orders`. A 14x burn rate exhausts a 30-day budget in
about two days, which is why it pages rather than waiting for the budget to actually run out.

## Triage — narrow it before you touch anything

**1. Is it 5xx or a dependency timing out?**

```promql
sum by (status) (rate(http_server_requests_total{route="/v1/orders"}[5m]))
```

**2. Which dependency?**

```promql
histogram_quantile(0.99, sum by (le, service) (rate(http_client_duration_seconds_bucket[5m])))
```

Checkout's synchronous dependencies, in the order they are called:

| Dependency | Failure looks like | Degrades to |
|---|---|---|
| Postgres (orders) | 500, `INTERNAL_ERROR` | Nothing. This is a hard failure. |
| Kafka (outbox write) | Succeeds — the outbox write is in the same transaction | Backlog, not errors |
| pricing-engine | Handled upstream in cart-service | List price |
| identity-service JWKS | 401s across the board | Cached JWKS for 5 min, then hard failure |

**3. Is it one pod or all of them?**

```promql
sum by (pod) (rate(http_server_requests_total{route="/v1/orders",status="5xx"}[5m]))
```

A single pod means a bad node or a stuck connection pool — delete it. All pods means a shared
dependency.

## Most common causes

### Connection pool exhaustion

```promql
souq_db_connections_in_use / souq_db_connections_max
```

Above 0.9 sustained means requests are queueing for a connection. Usually a slow query holding
connections rather than genuinely high load:

```sql
SELECT pid, now() - query_start AS duration, state, left(query, 120)
  FROM pg_stat_activity
 WHERE state <> 'idle' AND now() - query_start > interval '2 seconds'
 ORDER BY duration DESC;
```

### JWKS unreachable

Every 401 with `TOKEN_EXPIRED` across all users at once means the JWKS cache expired and
identity-service is unreachable. The verifier serves a stale cache on fetch failure precisely to
survive this, so if it is failing, identity-service has been down for longer than the cache TTL.

### A deploy

```bash
kubectl -n souq rollout history deploy/order-service
kubectl -n souq rollout undo deploy/order-service
```

Rolling back checkout is safe: the saga is idempotent and in-flight orders resume. Roll back
first, investigate second.

## What NOT to do

- **Do not disable the idempotency check** to "get orders flowing". That is how a retry storm
  becomes a double-charge storm.
- **Do not raise the rate limit** to clear a queue. If the WAF is rate-limiting checkout, it is
  usually catching card testing, and letting it through costs per-attempt fees.

## Related
- [`docs/CONTRACTS.md`](../CONTRACTS.md) §9 — the SLOs
- [`stuck-saga.md`](stuck-saga.md)
