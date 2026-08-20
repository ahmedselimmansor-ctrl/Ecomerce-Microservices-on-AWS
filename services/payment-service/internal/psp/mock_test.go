package psp

import (
	"context"
	"sync"
	"testing"
	"time"
)

// The AtMostOneCharge property from payment-service internal/psp/paymob_test.go, asserted
// against code instead of a model.
//
// The mock counts real money movements. If a retry storm ever produces two,
// the same storm against Paymob would too.
func TestMockNeverChargesTwiceForOneKey(t *testing.T) {
	m := NewMock(0, 0, 0) // always approve, no latency

	req := AuthorizeRequest{
		IdempotencyKey: "souq-deterministic-key",
		OrderID:        "ord_1",
		PaymentID:      "pay_1",
		Amount:         Money{Amount: 129900, Currency: "EGP"},
		Method:         MethodCard,
	}

	// Twenty concurrent retries — a saga redelivery storm plus a client that
	// gave up and pressed the button again.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = m.Authorize(context.Background(), req)
		}()
	}
	wg.Wait()

	if got := m.ChargeCount(req.IdempotencyKey); got != 1 {
		t.Fatalf("money moved %d times for one logical payment; want exactly 1", got)
	}
}

// A retry must return the ORIGINAL outcome, not a fresh roll. A mock that
// re-decides on every call hides the bug it exists to expose.
func TestMockReplaysTheOriginalOutcome(t *testing.T) {
	m := NewMock(1.0, 0, 0) // always decline

	req := AuthorizeRequest{IdempotencyKey: "k1", OrderID: "ord_1", Amount: Money{Amount: 100, Currency: "EGP"}}

	first, _ := m.Authorize(context.Background(), req)
	if first.Outcome != OutcomeDeclined {
		t.Fatalf("first outcome = %s, want DECLINED", first.Outcome)
	}

	// Now flip the mock to always approve. The replay must ignore that.
	m.DeclineRate = 0
	second, _ := m.Authorize(context.Background(), req)

	if second.Outcome != first.Outcome {
		t.Errorf("replay returned %s but the original was %s", second.Outcome, first.Outcome)
	}
}

// Same key, same fate — so a failing local test is reproducible rather than
// flaky.
func TestMockOutcomeIsDeterministic(t *testing.T) {
	a := NewMock(0.5, 0, 0)
	b := NewMock(0.5, 0, 0)

	for _, key := range []string{"k1", "k2", "k3", "k4", "k5", "k6"} {
		req := AuthorizeRequest{IdempotencyKey: key, OrderID: "o", Amount: Money{Amount: 1, Currency: "EGP"}}
		ra, _ := a.Authorize(context.Background(), req)
		rb, _ := b.Authorize(context.Background(), req)
		if ra.Outcome != rb.Outcome {
			t.Errorf("key %q gave %s on one instance and %s on another", key, ra.Outcome, rb.Outcome)
		}
	}
}

// The trap case: the response was lost but the money moved. The saga must see
// UNKNOWN so it reconciles instead of compensating against a real charge.
func TestMockUnknownOutcomeStillRecordsTheCharge(t *testing.T) {
	m := NewMock(0, 1.0, 0) // always unknown

	req := AuthorizeRequest{IdempotencyKey: "k-unknown", OrderID: "ord_1", Amount: Money{Amount: 100, Currency: "EGP"}}
	res, err := m.Authorize(context.Background(), req)

	if res.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %s, want UNKNOWN", res.Outcome)
	}
	if err == nil {
		t.Error("an unknown outcome returned no error; the caller would treat it as success")
	}
	if m.ChargeCount(req.IdempotencyKey) != 1 {
		t.Error("the mock reported UNKNOWN without recording the charge, so the reconciler has nothing to find")
	}
}

func TestMockRespectsContextCancellation(t *testing.T) {
	m := NewMock(0, 0, 500*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	res, err := m.Authorize(ctx, AuthorizeRequest{
		IdempotencyKey: "k", OrderID: "o", Amount: Money{Amount: 1, Currency: "EGP"},
	})

	if err == nil {
		t.Fatal("a cancelled context produced no error")
	}
	if res.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %s, want UNKNOWN — a timeout is not a decline", res.Outcome)
	}
}

// The mock must refuse webhooks. A verified-looking callback from a provider
// that never sends any is either misrouting or an attack.
func TestMockRefusesCallbacks(t *testing.T) {
	m := NewMock(0, 0, 0)
	if _, err := m.ParseCallback(context.Background(), []byte(`{"success":true}`), nil, nil); err == nil {
		t.Fatal("the mock accepted a callback; an attacker could mark orders paid")
	}
}
