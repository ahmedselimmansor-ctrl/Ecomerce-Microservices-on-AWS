# payment-service

Go. Owns the `payments` Aurora cluster. Talks to **Paymob** in production and a deliberately
unreliable mock locally.

## Read this before touching anything in here

[`payment-service internal/psp/paymob_test.go`](../../payment-service internal/psp/paymob_test.go) and
[`DESIGN-INVARIANTS.md §4`](../../docs/DESIGN-INVARIANTS.md). The short version:

> An atomic `INSERT ... ON CONFLICT` claiming the idempotency key, 409s for concurrent
> duplicates, and a reaper that only fires on a crashed owner — all textbook correct — **still
> double-charges**. The owner can crash *after* the provider moved money and *before* we
> recorded it. The reaper cannot tell that apart from a crash before the charge, and the reaper
> cannot be deleted or the customer can never retry.
>
> The fix is not in our table. It is that the retry must present the **same key to the provider**,
> so the provider recognises it and replays its own result.

That is what [`internal/payment/psp_key.go`](internal/payment/psp_key.go) does, in 60 lines, with
[11 tests](internal/payment/psp_key_test.go) pinning it.

## Paymob: the complication

**Paymob has no `Idempotency-Key` header.** Stripe and Adyen do; Paymob does not. So the
mechanism above has nowhere obvious to live.

The solution is the one uniqueness constraint Paymob *does* enforce — `merchant_order_id` on
order registration:

```
merchant_order_id = sha256-derived deterministic key   (psp_key.go)

1. register the order with it
2. Paymob says "duplicate"  ->  this payment already started.
   Look it up, read its transactions, return the REAL outcome.
   Do not charge again.
```

Step 2 is the entire safety property, not an edge case. It is
[`replayExisting`](internal/psp/paymob.go), and
[`TestDuplicateOrderDoesNotChargeTwice`](internal/psp/paymob_test.go) fails loudly if it ever
stops working.

Honest limitation: this is weaker than a real idempotency header, because the window between
"order registered" and "transaction created" is not covered. The reconciler closes it by
querying Paymob for anything left in `AUTHORIZING` or `CAPTURING`.

## Other Paymob specifics that cost real time to discover

| | |
|---|---|
| **HMAC field order** | 20 fields, concatenated with no separator, HMAC-SHA512. Wrong order rejects *every* callback, and the symptom is silent — orders stuck in `AUTHORIZING` while Paymob's dashboard shows them paid. Pinned by `TestHMACFieldOrderIsPinned`. |
| **`encoding/json` gives you float64** | So `129900` formats as `"129900.0"` and no callback ever verifies. This is the most common way a Go Paymob integration fails. `renderHMACScalar` handles it; a test pins it. |
| **Empty billing fields are rejected** | Paymob requires the literal `"NA"`, and the 400 it returns does not name the field. `billingData()` substitutes it. |
| **Wallets have no capture** | Vodafone Cash / Orange Money / Etisalat Cash move the money on approval. `SupportsCapture` returns false and the saga's CAPTURE step is satisfied locally. Same for cash on delivery. |
| **Single currency per account** | A mismatch is rejected locally with a useful message rather than by Paymob with an opaque one. |
| **Phone formats** | `+201005550000`, `00201005550000`, `0100 555 0000` all normalise to `01005550000`. |

## The webhook is the attack surface

[`internal/httpapi/webhook.go`](internal/httpapi/webhook.go) is reachable from the internet by
design. Five rules, each because breaking it has bitten somebody:

1. Verify the signature before touching anything else — no parsing, no lookups, no logging.
2. Return **200 for duplicates**. Paymob retries a non-200 for hours; a 500 on something we
   already applied turns one callback into thousands.
3. Return **401 for a bad signature**, never a 5xx. A forgery gets told no once.
4. Do the work **synchronously**. Returning 200 and queueing means acknowledging a payment we
   have not recorded.
5. **Never log the raw body** — it carries the PAN's last four, the cardholder name, and a full
   billing address.

`TestForgedCallbackIsRejected` covers seven forgery variants including a raised amount, a
flipped `success`, and a swapped order id.

## Outcomes are four values, not a boolean

`APPROVED · DECLINED · PENDING · UNKNOWN`

`UNKNOWN` is the one that matters. A timeout is **not** a decline — the charge may have
succeeded and only the response was lost. Collapsing it into failure makes the saga compensate
against real money. The row stays in `AUTHORIZING`, nothing is emitted, and the reconciler
resolves it.

## The mock is deliberately unreliable

`SOUQ_MOCK_DECLINE_RATE=0.1` by default, so roughly one local checkout in ten exercises the
compensation path. It is idempotent on the key and its outcome is a deterministic function of
that key, so a failing local test is reproducible rather than flaky.

## Verification

```bash
go test ./...        # 57 assertions
```

Plus `make sql-check`, which asserts against real Postgres that two payments cannot share a
provider key, that capture cannot exceed authorisation, that refund cannot exceed capture, and
that the double-entry ledger reports itself balanced.

## Configuration

```
SOUQ_PSP_PROVIDER            paymob | mock
SOUQ_PSP_KEY_SALT            >=32 chars, from Secrets Manager. Refuses to start without it.
SOUQ_PAYMOB_API_KEY
SOUQ_PAYMOB_HMAC_SECRET      refuses to start without it — an unverified webhook marks orders paid
SOUQ_PAYMOB_IFRAME_ID
SOUQ_PAYMOB_INTEGRATION_CARD
SOUQ_PAYMOB_INTEGRATION_WALLET
SOUQ_PAYMOB_INTEGRATION_COD
SOUQ_PAYMOB_CURRENCY         default EGP
```

## Before going live

Verify `hmacFieldOrder` against your account's current Paymob documentation and run one captured
live callback through `ParseCallback`. Paymob has changed the ordering before, and the failure
mode is silent.
