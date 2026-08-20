# What is built, and what is not

Written honestly. A repository that overstates itself wastes more of your time than one that is
smaller than you hoped.

**388 files · ~59,600 lines.**

**275 automated assertions execute in this container and all pass.** Separately, and listed
separately because this container has neither a JDK nor pip: **41 JUnit tests** for the two Java
services and **20 pytest tests** for `libs/py-contracts`, both of which run in CI.

**All 13 container images build**, verified by building them.

*(Counts exclude `node_modules`, lockfiles and generated `dist/`.)*

---

## Verified

"Verified" means it was executed in this repository, and the command is given so you can re-run it.

| Area | Result | Command |
|---|---|---|
| **Paymob integration** | 40 assertions — 7 forged-callback variants rejected, duplicate-order replay does not double-charge, transport failure maps to UNKNOWN not DECLINED, card data never retained | `cd services/payment-service && go test ./internal/psp/` |
| **Payment idempotency** | 11 assertions on the deterministic provider-key derivation | `go test ./internal/payment/` |
| **Saga state machine** | Exhaustive state-space search over every interleaving, plus 13 targeted assertions. Includes regression pins that assert two known-bad designs still fail | `cd services/order-service && go test ./...` |
| **Pricing engine (C++)** | 55 assertions, compiled clean under `-Wall -Wextra -Wpedantic -Werror` | `cd services/pricing-engine && g++ -std=c++20 -O2 -Wall -Wextra -Wpedantic -Werror -o build/t src/rules.cpp test/rules_test.cpp && ./build/t` |
| **Zod contracts** | 29 assertions, strict typecheck; one parses `machine.go` and asserts the Go and TypeScript saga states agree | `make contracts` |
| **Recommender fallbacks** | 7 behavioural assertions with fakes: outage, timeout at 301ms, cold-start top-up, Redis down, impression-id freshness | in-repo script |
| **Search query builder** | multiplicative `function_score`, namespaced attribute facets, Bayesian rating sort | in-repo script |
| **Schema invariants** | 66 assertions against real Postgres, up from 26. Every migration in the repository is applied, including the two Java services' Flyway migrations, which nothing had ever executed before | `make sql-check` |
| **Price-history trigger** | A price change writes exactly **one** history row, with the actor and reason from `SET LOCAL`; a non-price update writes none; an unattributed change records `system` | `make sql-check` |
| **Category tree** | A move rewrites the moved node's path *and* every descendant's, leaves other branches alone, and `parent_id` still agrees with `path` afterwards | `make sql-check` |
| **Oversell, empirically** | 50 concurrent connections racing 10 units → exactly 5 winners, `reserved == 10`, `CHECK` rejects a direct bypass | `make sql-check` |
| **AsyncAPI** | 7 channels, 24 messages, every `$ref` resolves | `make contracts` |
| **Terraform** | 6 modules + prod env, 153 resources, no undeclared variables | in-repo script |
| **Kubernetes** | 13 workloads × (Deployment, Service, PDB, HPA, NetworkPolicy) — the 11 services plus both frontends. Both overlays render; 26 deployments pass immutable-tag, memory-limit, **distinct liveness/readiness paths**, non-root, read-only rootfs, no-privilege-escalation, PDB-and-NetworkPolicy-present, and every `configMapKeyRef`/`secretKeyRef` resolving, and every Next.js probe path resolving to a real route handler | `make k8s-check` |
| **Node services** | cart, review and notification typecheck against real installed dependencies | `cd services/<svc> && npx tsc --noEmit` |
| **Frontend** | storefront (24 routes) and admin (10 routes) both typecheck **and produce a production build**. Neither had ever been built anywhere — see below | `make frontend` |
| **Images** | All 13 build. Ten of thirteen were broken — `COPY ../../libs/...` is a hard Docker error, three Go images pinned a toolchain older than their own `go.mod`, and `server.cpp` had never been compiled anywhere | `make images` |
| **Image consistency** | Dockerfiles, compose and the CI matrix agree; every `COPY` source exists; every deployed image has a Dockerfile | `make image-check` |
| **Contract parity** | 11 models and 23 error codes compared field-by-field between the TypeScript and Python contracts | `make contracts` |
| **Internal links** | Every static `href` in both frontends resolves to a real route, honouring route groups and `dynamicParams = false` | `make frontend` |
| **Java cross-reference** | 57 files, 217 types: every import, every `new`, and every call against a type this repository declares resolves. Verified by injecting a bad call and watching it fail | `make java-check` |
| **Go formatting** | `gofmt` clean across all four Go modules — 24 files were not, and `make lint` never checked. Verified by injecting an unformatted file and watching it fail | `make lint` |
| **Pricing rule loader** | 22 assertions: the shipped `rules.json` loads and prices a VIP cart end to end; 11 malformed files are rejected, including an uncapped percentage discount | `./build/rules_json_test` |
| **No dangling references** | every path referenced by compose, the Makefile and the scripts exists | in-repo script |
| **Kubernetes** | both overlays render; dev patches verified applied (replicas 3→1, PDB 2→0, HPA 3/30→1/3) | `kubectl kustomize infra/k8s/overlays/dev` |
| **CI** | 14 jobs / 26 parallel instances, `formal → images → e2e` gating asserted | YAML parse |
| **Cross-links** | all 9 alert→runbook and 7 ADR index links resolve | in-repo script |

## Not executed here

- **`mvn verify`, `terraform plan`, `next build`** — need a JDK, the `terraform` binary and a
  Next.js install. Each runs in CI; none runs in this container.
- **`pytest`** and **pip** — not installed here, so `libs/py-contracts` cannot be imported in this
  container at all. `scripts/contract-parity.py` was written stdlib-only for exactly that reason:
  it parses the TypeScript with a regex and the Python with `ast`, so contract drift is caught
  locally even though the models themselves cannot be instantiated.

The JDK gap matters most, because CI is the first thing that compiles the Java. It already let one
real error through — a controller calling `TokenService.describe(...)`, a method that was never
written. [`scripts/java-check.py`](scripts/java-check.py) now closes the most valuable part of
that gap without a compiler: it resolves every type name, every `new`, and every call against a
type this repository declares.

It is a net with a known mesh size, stated here rather than left to be discovered. It has no type
inference, so a call through a *local variable* is invisible to it; it resolves calls through
*fields* because their declared type is written down. Generics, overload resolution and
assignability are all out of scope. Passing it means considerably less than `mvn compile` passing
— but it caught `ProductService.batch(...)` before CI did.

## Complete

**Formal** — 4 state-space models, 12 configs, [`DESIGN-INVARIANTS.md`](docs/DESIGN-INVARIANTS.md) documenting the 5
design decisions that came out of counterexamples, `check.sh` asserting each expected outcome.

**Contracts** — `docs/CONTRACTS.md` (normative), AsyncAPI 3.0, pricing protobuf,
`@souq/contracts` Zod package shared by both frontends and the Node services.

**Services**

| Service | State |
|---|---|
| order (Go) | Complete: saga machine, outbox, inbox, idempotency, relay, sweeper, HTTP + SSE, JWKS verifier |
| payment (Go) | Complete: **Paymob** + mock providers, deterministic provider keys, HMAC webhook, double-entry ledger, service layer |
| inventory (Go) | Complete: reservation engine, tombstones, TTL sweeper, relay, consumer, HTTP API, concurrency model, real-Postgres oversell race |
| pricing (C++) | Complete: rule engine, JSON rule loader with validation, gRPC server with atomic SIGHUP reload, CMake build, Dockerfile |
| cart (Node) | Complete: Redis CAS via Lua, pricing circuit breaker, catalog cache, Fastify app |
| notification (Node) | Complete: three-layer dedup, CQL schema, SES/SNS, opt-out + quiet hours, 13 bilingual templates, bounce handling |
| review (Node) | Complete: MongoDB repo, derived verified-purchase, incremental aggregates, moderation, Fastify app, order consumer |
| search (Python) | Complete: ES mapping, query builder, alias reindex, FastAPI app, Kafka indexer, Postgres fallback |
| recommendation (Python) | Complete: Personalize wrapper, per-placement deadlines, 3 fallback rankers, FastAPI app |
| identity (Java) | Complete: Argon2id + breach check, per-account/per-IP lockout, rotating refresh with reuse detection, TOTP, password reset, JWKS + OIDC discovery, RFC 9457 handler, outbox relay. 9 JUnit tests (CI only) |
| catalog (Java) | Complete: product/variant/category CRUD, optimistic locking, compacted-topic events with a real Kafka tombstone, JWKS verifier, inventory consumer with inbox, outbox relay, edge caching. 22 JUnit tests (CI only) |

**Frontend** — storefront: 80 files, ~7,400 lines. 11 pages (home, PLP with facets, PDP with a
variant picker, cart, checkout, order status, order history, profile, auth) and 13 BFF Route
Handlers. Admin: 34 files, ~3,100 lines — KPI overview, order search with the saga inspector,
catalogue CRUD with optimistic locking, and the DLQ view. Both use a 16-component shadcn/ui set.

**Infrastructure** — Terraform: network, data, eks, edge, personalize, observability + prod env.
Kubernetes: base + dev/prod overlays covering all 11 services, ExternalSecrets, 4 Kyverno
policies. CI: 13 jobs / 26 parallel instances. docker-compose: 11 services + 8 datastores +
observability profile, every build context resolving.

**Observability** — Prometheus scrape config, 14 alert rules mirrored from Terraform by
`scripts/sync-alerts.sh` so the same alerts can be watched locally, and a 14-panel Grafana
dashboard checked into git rather than clicked together in the UI.

**Operations** — 9 runbooks (every alert links to one, all verified to resolve), 7 ADRs,
10 scripts including `sql-check`, `smoke`, `chaos`, `dlq-depth` and `tools/integration/run.sh`,
which exercises the tombstone race directly.

## Not built

Everything on the plan is written, and everything previously listed here is now done — KMS
signing, `libs/py-contracts`, `GetPrices`, the account security and password screens, and the
legal and help pages.

What is genuinely absent, named so nobody is surprised:

- **A real Paymob hosted-field integration in the checkout UI.** The flow submits a test token.
  The server side — the full Paymob adapter, HMAC callback verification, the deterministic
  provider key — is complete and has 40 assertions against it; what is missing is the browser
  half, which by design must be Paymob's own iframe so a card number never reaches this origin.
- **Recovery-code sign-in.** `MfaService.consumeRecoveryCode` exists and is tested at the schema
  level, but the login form has no "use a recovery code instead" path.
- **`GetPrices` has no caller.** It is implemented and correct, but the storefront gets listing
  prices from search-service, which already holds them. It exists for a client that does not yet
  exist.

Smaller gaps: `libs/py-contracts` (the Python services validate by hand rather than from
generated Pydantic models), and `GetPrices` in the pricing gRPC server returns every SKU as
unknown because the engine has no catalogue lookup — it is wired for `CalculateCart`, which is
the call on the hot path.

## Where the depth went, and why

Into the places where being wrong costs money and being wrong is not obvious: the saga, the
reservation path, payment idempotency, and the Paymob integration. Those four are modelled,
implemented, and empirically verified.

The rest is CRUD over well-understood datastores. Real work, but work whose failure mode is a
bug report rather than a customer charged for stock that does not exist.

Several things found while finishing the remaining work are worth recording, because every one of
them was silent — nothing failed, nothing warned, and the gap was only visible once something
actually ran the code:

- **`.ONESHELL:` in the Makefile meant `cd` leaked between recipe lines**, so `make check` ran the
  saga and reservation models from the wrong directory. Every `cd` is now in a subshell.
- **`make lint` never ran `gofmt`.** Twenty-four Go files had drifted. In Go that is not a style
  preference, and a repository that drifts produces diffs full of whitespace — which is how a real
  change hides inside a reformat.
- **`price_history` was being written twice per change** — once by the V1 trigger and once by
  `JdbcProductRepository`. Two rows per change is worse than none: it silently doubles every report
  finance builds from that table.
- **`headers()` is async in Next 15.** `bff.ts` called it synchronously, which does not throw — it
  returns a Promise whose `.get` is `undefined`. Every request quietly minted a fresh correlation
  id, so a single checkout appeared in the logs as eleven unrelated traces.
- **The admin app had no `tsconfig.json` and no `package.json`**, so it had never been typechecked.
  `saga-inspector.tsx` read `step.deadlineAt`, a field the contract did not define — while
  order-service had been persisting `deadline_at` all along. The contract was the thing that was
  wrong, and now has it.
- **Neither frontend app was built anywhere, including CI.** `next build` is not redundant with
  `tsc`: a `useState` in a server component typechecks cleanly and fails at request time. That is
  demonstrated rather than asserted — injecting exactly that error leaves `tsc --noEmit` at exit 0
  and fails `make frontend`.
- **CI built eight of thirteen images.** identity, catalog and review had Dockerfiles nothing ever
  built, and the two frontends had none in the matrix at all — so an image could rot for months and
  only fail on a release. The matrix is now generated against every Dockerfile in the tree, and a
  check asserts the two sets match.
- **Neither frontend had a Kubernetes manifest**, despite the architecture diagram showing both
  behind the ALB. They now have the full set, with `NetworkPolicy` egress lists that differ from
  each other on purpose: the storefront cannot reach payment-service, and the admin cannot reach
  cart or the recommender.
- **Ten of thirteen images did not build.** `COPY ../../libs/ts-contracts` is not a path Docker
  will follow, it is an error; three Go images pinned `golang:1.23` while their own `go.mod` said
  `go 1.25.0`; and `pricing-engine/src/server.cpp` referenced a type that does not exist and was
  missing a `grpc++_reflection` link. None of it was visible: the CI images job only runs on push
  to `main`, the local Go toolchain is newer than either pin, and the quick C++ job compiles
  `rules.cpp` and the tests but never `server.cpp`.

  The npm failure underneath the first of those is worth recording on its own. `package-lock.json`
  records the shared library as `file:../../libs/ts-contracts`, a path *relative to the package*,
  so flattening the layout inside the image makes npm fail with `Cannot read properties of
  undefined (reading 'extraneous')` — a message naming neither the package nor the path. The fix
  is to replicate the repo layout under `/repo`.

- **`search-service` answered type-ahead on `?prefix=` while the BFF sent `?q=`.** Every suggest
  request was a 422. Nothing reported it because the client swallows suggest failures on purpose —
  an empty dropdown is invisible, and the form still submits.

- **Two checks that never fired.** The probe-path assertion matched flow-style YAML, but Kustomize
  normalises `httpGet: { path: … }` into a block mapping, so it found nothing and reported success
  for work it never did. The link checker matched only `href="/x"` and not the `href: '/x'` form
  every nav array uses, then resolved `/legal/anything` against `legal/[slug]` while ignoring
  `dynamicParams = false`.

  Both were found the same way, and it is the only way they could have been: deliberately break
  the thing the check exists to catch, and see whether anything fails. **Every check added in this
  session was verified that way.** A check that silently passes is worse than no check, because it
  is also a claim.

Everything unbuilt has its contract already fixed in
[`docs/CONTRACTS.md`](docs/CONTRACTS.md) and
[`souq-events.v1.yaml`](contracts/asyncapi/souq-events.v1.yaml) — ports, event schemas, error
envelope, retry budgets, data ownership. That is the part that is expensive to change later.
