# admin

Next.js 15 (App Router) · port 3001 · the operations tool.

Requires an `ADMIN` or `OPS` role **and** a session with MFA (docs/CONTRACTS.md §7). A role
survives a stolen password; a second factor does not. The check is server-side, on every request,
in [`src/lib/session.ts`](src/lib/session.ts) — hiding navigation is a courtesy, not a control.

---

## What it is for

Four screens, and each exists because of a question somebody actually asks during an incident.

| Screen | The question |
|---|---|
| **Overview** | Is anything wrong right now? |
| **Orders** | Why has *this* order not shipped? |
| **Catalogue** | Change a price, publish a draft, retire a product |
| **Dead letters** | Which events never got applied, and is it safe to replay them? |

---

## Four decisions that shape it

**The operational figures come before the commercial ones.** Stuck sagas and DLQ depth sit in the
first row; revenue is in the second. A dashboard that leads with revenue trains people to read
revenue, and the numbers that mean "go and do something" belong where the eye lands. Colour is
never the only signal either — roughly one in twelve men has a red-green deficiency, so every
alarming tile also carries a text badge.

**Nothing is cached and nothing is retried.** Both differ deliberately from the storefront. An
operations tool showing a two-minute-old view of a stuck saga is actively dangerous, and a silent
triple-retry turns "the service is down" into "the page is slow" — the wrong thing to learn at 2am.
Timeouts are longer instead, because a DLQ scan legitimately takes ten seconds.

**The saga inspector removes the cancel control past the point of no return — it does not disable
it.** [`docs/DESIGN-INVARIANTS.md`](../../docs/DESIGN-INVARIANTS.md) §1 shows that compensating
after `inventory.commit` loses money, so `PAID`, `STOCK_COMMITTED` and `CONFIRMED` have no
compensating transition at all. A disabled button invites someone to find another way; an
explanation does not.

The route behind it does **not** re-implement that rule. order-service enforces it in its state
machine and again in a `CHECK` constraint; a fourth copy here would be a fourth thing to keep in
step and the one most likely to drift.

**Replay needs no confirmation; discard does.** Every consumer dedupes on `event_id` through its
inbox table, so replaying a message that did partially apply is a no-op — making a safe action feel
dangerous is how people learn to click through every dialog, including the one that mattered.
Discard is the only irreversible control in the app, so it asks for the event id to be typed rather
than for a yes/no on a row of identical-looking buttons.

---

## Error messages are shown in full

The opposite of the storefront, on purpose. The audience is the team that operates the platform,
the page is behind an MFA-gated role check, and withholding `connection refused to
payment-service:8086` from an on-call engineer helps nobody.

The three ways to fail the authorisation check are also told apart on screen — unlike login,
registration and password reset, where the responses are deliberately identical to avoid an
enumeration oracle. Here the caller has already authenticated, so nothing is disclosed, and
"you need MFA" versus "you need a role" is a two-minute fix versus a support ticket.

---

## Verification

```bash
make frontend      # typecheck + production build, both apps
```

`next build` is not redundant with `tsc`: a `useState` in a server component typechecks cleanly
and fails at request time. Neither app was built anywhere before this target existed, and the first
run found a real bug — `saga-inspector.tsx` read `step.deadlineAt`, a field the contract never
declared, while order-service had been persisting `deadline_at` all along. The contract was the
thing that was wrong.
