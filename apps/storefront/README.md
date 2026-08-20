# storefront

Next.js 15 (App Router) · Tailwind · shadcn/ui · Zod · TanStack Query.

## The two files worth reading first

**[`src/lib/bff.ts`](src/lib/bff.ts)** — the service client. The browser never calls a service
directly (docs/CONTRACTS.md §8); everything goes through Route Handlers under `/api/bff/*`,
which fan out and **validate every response with a Zod schema before a component sees it**.
When inventory-service starts returning `available: null`, this produces one clean 502 with a
requestId instead of `undefined is not a number` three renders away from the cause.

It also carries the retry policy from §5.4: full jitter, and **retries only on GETs and on
requests carrying an `Idempotency-Key`** — automatically retrying a bare POST is how you charge
someone twice.

`gather()` is there because `Promise.all` is wrong for a page composed of one required call and
three optional ones. A product page needs the product; it must not 500 because the
recommendation service is redeploying.

**[`src/components/checkout/order-progress.tsx`](src/components/checkout/order-progress.tsx)** —
the UI consequence of an asynchronous saga. `POST /v1/orders` returns **202**, not 201: the saga
has started, not finished. Showing a success page at that moment would be a lie roughly one time
in ten. The component subscribes to SSE, falls back to backing-off polling when a proxy kills the
stream, and after 90 seconds stops implying the order is about to settle and tells the customer
what to do instead.

Failure copy is keyed on `reasonCode`, never on the human-readable `detail` — the detail is
written for an engineer reading a log and it changes with the next copy edit.

## Eventual consistency in the UI

- `ProductVariant.available` is denormalised from inventory and is a **display hint only**. The
  authoritative check happens when the saga reserves. Gating "Add to cart" on it alone means
  accepting orders you cannot fulfil.
- `cart.pricingDegraded` is true when pricing-engine was unreachable and the cart fell back to
  list price. The UI hides promotional messaging rather than showing prices it cannot justify.
- `recommendations.fallback` is true when Personalize was cold or unavailable. The UI drops the
  "Recommended for you" heading rather than lying about personalisation.

## What is here

**11 pages** — home, PLP with facets and pagination, PDP with a variant picker, cart, checkout,
order status, order history, profile, sign in, register, forgotten password.
**13 BFF Route Handlers** under `/api/bff/*`.
**16 shadcn/ui components**, plus a catalogue set (price, rating, stock indicator, product card,
facets) that encodes the rules below rather than leaving them to each caller.

```bash
make frontend      # typecheck + production build, both apps
```

`next build` is not redundant with `tsc`. A `useState` in a server component is not a type error —
it typechecks cleanly and fails at request time — so the build is the only thing that catches a
Server/Client boundary violation. Both run in CI.

## Three rules the components encode

**`available === null` is not `0`.** Null means inventory has not reported; zero means none left.
`StockIndicator` renders nothing for null and `AddToCart` stays **enabled**, because the
authoritative check is the saga's reservation. Blocking on a stale null refuses sales of items
that are in stock, which costs more than the occasional rejected reservation.

**Money never becomes a float except in `formatMoney`.** Amounts are integer minor units end to
end. A discount percentage is rounded *down* — claiming 30% off when the real figure is 29.6% is a
small lie that a consumer authority treats as a large one.

**Errors switch on `code`, never on `detail`.** `detail` is written for an engineer reading a log,
it is not localised, and it changes with the next copy edit. `ApiError.userMessage` is the single
place the mapping lives.

## Two security decisions worth knowing about

**Neither token reaches JavaScript.** The refresh token is an HttpOnly, SameSite=Strict cookie
scoped to `/api/bff/auth`; the access token is derived from it per request, server-side, and never
sent to the browser. An XSS can act as the user — any session design allows that — but cannot
steal a 30-day credential and replay it from elsewhere for a month.

**The post-login redirect is sanitised.** `?next=` accepts path-only, same-origin values.
`//evil.example` is the variant a naive `startsWith('/')` check misses, and an open redirect on a
login page is the standard way to make a phishing link look like it came from us.
