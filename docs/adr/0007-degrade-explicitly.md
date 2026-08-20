# 7. Degradation is a field in the contract

**Status:** Accepted · **Date:** 2026-08

## Context

Several services have a fallback: pricing-engine falling back to list price,
Personalize falling back to bestsellers, OpenSearch falling back to a Postgres
`LIKE`. The usual approach is to make these invisible — the fallback returns
something plausible and nobody upstream knows.

That is wrong for two reasons. The customer is shown something that looks
personalised and is not, or a price without the promotion they were promised.
And the operator has no way to distinguish "the recommendation carousel is
working" from "the recommendation carousel has been serving bestsellers to
everyone for three days".

## Decision

Degradation is part of the response contract:

- `Cart.pricingDegraded`
- `RecommendationResponse.fallback` and `fallbackReason`
- `SearchResponse.degraded`

Each defaults to `false`, so a service that forgets to set it cannot
accidentally claim to be degraded. The storefront acts on them:

- A degraded cart hides promotional messaging rather than showing prices it
  cannot justify.
- Fallback recommendations lose the "Recommended for you" heading rather than
  lying about personalisation.
- A degraded cart has `rulesVersion: null`, and order-service **refuses an
  order without one**. Showing a cart we cannot price is fine; taking money for
  one is not.

## Consequences

**Good.** Every fallback is measurable. `RecommendationsAllFallback` and
`PricingEngineCircuitOpen` are real alerts because the data exists to raise
them, and each is a ticket rather than a page — which is the correct severity
for "customers are getting a worse experience but nothing is broken".

**Bad.** Every consumer has to handle the flag, and a consumer that ignores it
is back to the original problem silently. The Zod schemas make the field
present and typed, which makes ignoring it a deliberate act rather than an
oversight.

**A judgement embedded here:** selling at list price is a bad day, failing
checkout is worse. Every fallback in this platform resolves toward serving the
customer something, and toward telling the truth about what it served.
