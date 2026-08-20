// Package payment holds the money-moving logic.
//
// This file is short and it is the most important file in the service. It
// exists because of docs/DESIGN-INVARIANTS.md §4, which is worth restating in full
// because the failure is not obvious:
//
// A payment service can have an atomic INSERT ... ON CONFLICT claiming the
// idempotency key, return 409 to concurrent duplicates, and only reap keys
// abandoned by a crashed owner — textbook correct — and still charge the
// customer twice:
//
//  1. request r1 wins the key                 -> IN_PROGRESS
//  2. r1 calls the provider                   -> MONEY MOVES
//  3. r1 crashes before writing COMPLETED
//  4. the reaper expires the abandoned key    -> ABSENT
//  5. request r2 wins the key                 -> IN_PROGRESS
//  6. r2 calls the provider with a FRESH key  -> MONEY MOVES AGAIN
//
// The reaper cannot distinguish "crashed before charging" from "crashed after
// charging"; only the provider knows. And the reaper cannot be deleted,
// because without it a customer whose payment crashed can never retry.
//
// So the fix is not in our table at all. It is that step 6 must present the
// SAME idempotency key the provider already saw in step 2, so the provider
// recognises the retry and replays its own stored result instead of charging.
package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// PSPKeyDeriver produces provider-facing idempotency keys.
//
// It is a type rather than a bare function so that the secret salt is bound to
// it at construction. That makes it impossible to call the derivation without
// the salt, and it makes the dependency visible in every constructor that
// needs to move money.
type PSPKeyDeriver struct {
	// salt is a per-environment secret from AWS Secrets Manager. It is not
	// needed for correctness — a plain hash would dedup just as well — but it
	// stops an attacker who can guess an order id from computing the key we
	// will present to the provider, which in some provider APIs is enough to
	// probe or interfere with a payment.
	salt []byte
}

var ErrEmptySalt = errors.New("payment: the PSP key salt is empty; refusing to derive predictable keys")

func NewPSPKeyDeriver(salt string) (*PSPKeyDeriver, error) {
	// Fail at construction, not at first use. A service that starts without
	// its salt and only discovers it on the first real payment is a service
	// that discovers it in production, during checkout.
	if len(strings.TrimSpace(salt)) < 32 {
		return nil, fmt.Errorf("%w: need at least 32 characters, got %d", ErrEmptySalt, len(salt))
	}
	return &PSPKeyDeriver{salt: []byte(salt)}, nil
}

// Derive returns the provider idempotency key for one logical operation.
//
// Deterministic in every input and nothing else. Specifically it does NOT
// depend on:
//
//   - the attempt number  — that is the whole point; attempt 2 must collide
//     with attempt 1 at the provider
//   - the pod, the time, or any random source — a retry usually happens on a
//     different pod, minutes later
//   - the payment row's mutable state — the row may have been rolled back
//
// The operation is included so that AUTHORIZE, CAPTURE, VOID and REFUND for
// the same payment get distinct keys. Without it, a capture would collide with
// its own authorization at the provider and be silently treated as a replay.
func (d *PSPKeyDeriver) Derive(operation Operation, orderID, idempotencyKey string) (string, error) {
	if orderID == "" {
		return "", errors.New("payment: cannot derive a PSP key without an order id")
	}
	if idempotencyKey == "" {
		return "", errors.New("payment: cannot derive a PSP key without the client idempotency key")
	}
	if !operation.valid() {
		return "", fmt.Errorf("payment: unknown operation %q", operation)
	}

	// HMAC rather than a plain hash of salt||data: length-extension is not a
	// practical threat here, but HMAC is the primitive that is unambiguously
	// correct for keyed hashing, and using the obviously-right one costs
	// nothing.
	mac := hmac.New(sha256.New, d.salt)

	// Length-prefix each field. Concatenating with a separator lets
	// ("ord_1|x", "y") and ("ord_1", "x|y") collide, which would make two
	// different payments share a provider key — and the provider would then
	// treat the second as a replay of the first and never charge it.
	for _, field := range []string{string(operation), orderID, idempotencyKey} {
		fmt.Fprintf(mac, "%d:%s", len(field), field)
	}

	// Providers cap idempotency key length (Stripe at 255, Adyen at 64), and
	// some reject non-alphanumerics. 32 hex characters is 128 bits of the
	// digest — far beyond any collision concern at our volume — and safe
	// everywhere.
	return "souq" + hex.EncodeToString(mac.Sum(nil))[:32], nil
}

// DeriveRefund keys a refund by its own id, because a payment can be refunded
// several times partially and each refund is a distinct movement of money.
func (d *PSPKeyDeriver) DeriveRefund(orderID, refundID string) (string, error) {
	if refundID == "" {
		return "", errors.New("payment: cannot derive a refund key without a refund id")
	}
	return d.Derive(OpRefund, orderID, refundID)
}

type Operation string

const (
	OpAuthorize Operation = "authorize"
	OpCapture   Operation = "capture"
	OpVoid      Operation = "void"
	OpRefund    Operation = "refund"
)

func (o Operation) valid() bool {
	switch o {
	case OpAuthorize, OpCapture, OpVoid, OpRefund:
		return true
	}
	return false
}
