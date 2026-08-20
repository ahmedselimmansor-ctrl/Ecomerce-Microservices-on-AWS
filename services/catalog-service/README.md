# catalog-service

Java 21 · Spring Boot 3.3 · port 8082 · PostgreSQL `catalog`

Owns products, variants, categories and price history. The only writer to the `catalog` database.
Search, recommendations and every storefront read model are built from the events it emits.

---

## Compaction shapes the events

`souq.catalog.events.v1` is a **compacted** topic: Kafka keeps only the newest message per key.
Three consequences follow, and all three are load-bearing.

**Every product event carries full current state, not a delta.** A consumer rebuilding its index
from the topic sees exactly one message per product, so that message has to be sufficient to
reconstruct the entity. A delta would rebuild an index holding whichever field happened to be
edited last.

**The partition key is the product id and nothing else.** Keying by category would leave several
surviving messages per product with no defined order between them.

**A delete emits two messages.** The first is `product_deleted` so live consumers drop the entity.
The second has a genuinely null payload — the compaction tombstone, the only thing that makes the
key itself disappear from the topic. `OutboxRelay` converts the JSONB literal `null` to an actual
Kafka null rather than publishing the four-character string `"null"`; get that wrong and every
rebuild months later replays every product ever deleted.

---

## Four things that are less obvious than they look

**Price history is written by a database trigger, not by this code.** Three paths change a price
— the admin API, the bulk import, promotion expiry — and a fourth is whoever opens `psql` during
an incident. Only the database covers all of them. The actor and reason reach the trigger through
`SET LOCAL souq.actor` / `souq.reason`, transaction-scoped so a pooled connection cannot attribute
the next admin's change to this one. Adding an explicit insert here as well — the obvious thing to
write — produces **two** rows per change, which is worse than none: it silently doubles every
report finance builds from the table.

**Updates are conditional on `version`.** Two merchandisers editing one product is the normal
state of a merchandising team, not a rare race. The check and the write are one statement; reading
the version and comparing it in Java reintroduces the lost update the column exists to prevent.

**Products and variants come back in one query.** Twenty products with a few variants each is 21
round trips if you loop. The `LIMIT` is applied to a subquery of product ids, not to the joined
result — otherwise a product with six variants consumes six slots of a 24-item page.

**Image hosts are an allow-list.** A product image renders on every storefront page for that
product. An arbitrary origin means a compromised admin account sees the IP and referrer of every
shopper, and can change what it serves afterwards.

---

## `variants.available` is a display hint

Denormalised from inventory-service so a product page is one query instead of a fan-out per SKU.
It is **eventually consistent** and the contract says so. `NULL` means "we have not heard from
inventory" and is not the same as `0`; the UI must render them differently, because "out of stock"
when the truth is "unknown" suppresses sales of items that are in stock.

The authoritative check happens when the order saga reserves. Gating checkout on this column means
accepting orders that cannot be fulfilled.

---

## Verification

```bash
python3 scripts/java-check.py services/catalog-service/src   # cross-reference, no JDK needed
make sql-check                                                # migrations + invariants, real Postgres
```

`make sql-check` runs the three migrations and then asserts, among others:

- a reference price below the selling price is rejected — a "was 999, now 1299" strikethrough is a
  regulator's problem in most markets, so the schema makes it unrepresentable
- a price change writes **exactly one** history row, with the actor and reason from the session
- a non-price update writes none
- a category move rewrites the moved node's path *and* every descendant's
- `parent_id` and `path` still agree afterwards — the invariant the redundancy depends on
- a category with products or children is not deleted

CI additionally runs `mvn verify`, which is the only thing here that actually compiles the code.

---

## Endpoints

| Route | Who |
|---|---|
| `GET /v1/products`, `/v1/products/{idOrSlug}`, `/v1/products/batch` | public; `ACTIVE` only, cached at the edge for 60 s with `stale-while-revalidate` |
| `GET /v1/categories`, `/{slug}`, `/{slug}/subtree` | public; 300 s |
| `POST|PATCH|DELETE /v1/admin/products/**` | `ADMIN` or `MERCHANT` |
| `POST|DELETE /v1/admin/categories/**` | `ADMIN` with MFA — restructuring the tree moves every product beneath it |

Admin routes live in a separate controller rather than behind per-method role checks. The split is
the safety property: a new endpoint in the wrong file is visible in review, whereas a missing
annotation on one method among twenty is not.
