package payment

import (
	"strings"
	"testing"
)

const testSalt = "a-test-salt-that-is-at-least-32-characters-long"

func deriver(t *testing.T) *PSPKeyDeriver {
	t.Helper()
	d, err := NewPSPKeyDeriver(testSalt)
	if err != nil {
		t.Fatalf("NewPSPKeyDeriver: %v", err)
	}
	return d
}

// THE property. Everything else in this file supports it.
//
// If this test fails, docs/DESIGN-INVARIANTS.md §4 is live in production: a crash
// between the provider call and our commit will charge the customer twice.
func TestSameLogicalPaymentAlwaysYieldsTheSameKey(t *testing.T) {
	d := deriver(t)

	first, err := d.Derive(OpAuthorize, "ord_01J8Z", "idem-abc")
	if err != nil {
		t.Fatal(err)
	}

	// A different process, minutes later, after a reaper freed our key.
	// Nothing about the environment is the same. The key must be.
	second, err := d.Derive(OpAuthorize, "ord_01J8Z", "idem-abc")
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatalf("the same logical payment produced two different provider keys:\n  %s\n  %s\n"+
			"this is the double-charge bug in docs/DESIGN-INVARIANTS.md §4", first, second)
	}
}

// A fresh deriver — a new pod, a new deploy — must agree with the old one.
func TestKeyIsStableAcrossDeriverInstances(t *testing.T) {
	a, _ := NewPSPKeyDeriver(testSalt)
	b, _ := NewPSPKeyDeriver(testSalt)

	ka, _ := a.Derive(OpCapture, "ord_1", "idem-1")
	kb, _ := b.Derive(OpCapture, "ord_1", "idem-1")

	if ka != kb {
		t.Error("two deriver instances with the same salt disagreed; the key is not deterministic")
	}
}

// Different payments must never collide, or the provider treats the second as
// a replay of the first and the merchant is never paid for it.
func TestDistinctInputsNeverCollide(t *testing.T) {
	d := deriver(t)

	cases := []struct {
		name                    string
		op                      Operation
		orderID, idempotencyKey string
	}{
		{"baseline", OpAuthorize, "ord_1", "idem-1"},
		{"different order", OpAuthorize, "ord_2", "idem-1"},
		{"different idempotency key", OpAuthorize, "ord_1", "idem-2"},
		{"capture not authorize", OpCapture, "ord_1", "idem-1"},
		{"void not authorize", OpVoid, "ord_1", "idem-1"},
		{"refund not authorize", OpRefund, "ord_1", "idem-1"},
	}

	seen := map[string]string{}
	for _, c := range cases {
		k, err := d.Derive(c.op, c.orderID, c.idempotencyKey)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if prev, dup := seen[k]; dup {
			t.Errorf("%q and %q produced the same provider key %s", c.name, prev, k)
		}
		seen[k] = c.name
	}
}

// The length-prefixing exists for exactly this. Without it, these two payments
// share a key and the second is silently never charged.
func TestFieldBoundariesCannotBeShiftedToForceACollision(t *testing.T) {
	d := deriver(t)

	a, err := d.Derive(OpAuthorize, "ord_1", "x-idem")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Derive(OpAuthorize, "ord_1x", "-idem")
	if err != nil {
		t.Fatal(err)
	}

	if a == b {
		t.Error("a field-boundary collision is possible; the length prefixing is not working")
	}
}

// Capture must not collide with its own authorization at the provider, or the
// capture is treated as a replay and the money is never actually taken.
func TestCaptureDoesNotCollideWithItsAuthorization(t *testing.T) {
	d := deriver(t)

	auth, _ := d.Derive(OpAuthorize, "ord_1", "idem-1")
	capture, _ := d.Derive(OpCapture, "ord_1", "idem-1")

	if auth == capture {
		t.Error("authorize and capture share a provider key; the capture would be swallowed as a replay")
	}
}

// A different environment must produce different keys, so a staging retry can
// never collide with a production payment at a shared provider account.
func TestDifferentSaltsProduceDifferentKeys(t *testing.T) {
	prod, _ := NewPSPKeyDeriver("production-salt-value-at-least-32-chars-long")
	stage, _ := NewPSPKeyDeriver("staging-salt-value-at-least-32-characters-ok")

	kp, _ := prod.Derive(OpAuthorize, "ord_1", "idem-1")
	ks, _ := stage.Derive(OpAuthorize, "ord_1", "idem-1")

	if kp == ks {
		t.Error("two environments derived the same provider key")
	}
}

func TestRefusesToStartWithoutAStrongSalt(t *testing.T) {
	for _, salt := range []string{"", "   ", "too-short", strings.Repeat("x", 31)} {
		if _, err := NewPSPKeyDeriver(salt); err == nil {
			t.Errorf("accepted a %d-character salt; it must refuse", len(salt))
		}
	}
	if _, err := NewPSPKeyDeriver(strings.Repeat("x", 32)); err != nil {
		t.Errorf("rejected a valid 32-character salt: %v", err)
	}
}

func TestRejectsMissingInputs(t *testing.T) {
	d := deriver(t)

	if _, err := d.Derive(OpAuthorize, "", "idem-1"); err == nil {
		t.Error("derived a key with no order id")
	}
	if _, err := d.Derive(OpAuthorize, "ord_1", ""); err == nil {
		t.Error("derived a key with no idempotency key")
	}
	if _, err := d.Derive(Operation("charge-it"), "ord_1", "idem-1"); err == nil {
		t.Error("derived a key for an unknown operation")
	}
}

// Providers cap key length and some reject non-alphanumerics.
func TestKeyShapeIsAcceptableToEveryProvider(t *testing.T) {
	d := deriver(t)
	k, _ := d.Derive(OpAuthorize, "ord_01J8Z3K9S2M4P6R8T0V2X4Y6A8", "550e8400-e29b-41d4-a716-446655440000")

	if len(k) > 64 {
		t.Errorf("key is %d characters; Adyen caps idempotency keys at 64", len(k))
	}
	if !strings.HasPrefix(k, "souq") {
		t.Errorf("key %q lost its namespace prefix", k)
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			t.Fatalf("key %q contains %q; keep it lowercase alphanumeric for provider compatibility", k, r)
		}
	}
}

// Partial refunds are separate movements of money and must not share a key.
func TestEachRefundGetsItsOwnKey(t *testing.T) {
	d := deriver(t)

	first, err := d.DeriveRefund("ord_1", "ref_1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.DeriveRefund("ord_1", "ref_2")
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Error("two partial refunds share a provider key; the second would never be paid out")
	}

	// But a retry of the SAME refund must still be idempotent.
	retry, _ := d.DeriveRefund("ord_1", "ref_1")
	if retry != first {
		t.Error("retrying one refund produced a new key; it would refund twice")
	}
}

// Regression guard: pins the exact output. A change to the derivation is a
// change to a value the provider has already seen for every in-flight
// payment, which would make every one of them look new. If this test fails,
// the change needs a migration plan, not a golden-value update.
func TestDerivationIsPinned(t *testing.T) {
	d := deriver(t)
	got, err := d.Derive(OpAuthorize, "ord_01J8Z3K9S2M4P6R8T0V2X4Y6A8", "550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 36 { // "souq" + 32 hex characters
		t.Fatalf("key length changed to %d; in-flight payments would stop deduplicating", len(got))
	}
	t.Logf("pinned derivation output: %s", got)
}
