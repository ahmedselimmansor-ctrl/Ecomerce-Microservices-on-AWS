package psp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Mock provider for local development and integration tests.
//
// It is deliberately not a happy-path stub. A mock that always approves means
// the compensation path — which is half the saga and all of the interesting
// half — is only ever exercised by a test nobody runs locally. This one
// declines a configurable fraction of authorisations, occasionally times out,
// and occasionally returns UNKNOWN, so `make up && make smoke` exercises
// rollback as a matter of course rather than as an exception.
//
// Two properties it shares with a real provider, because the code above it
// depends on both:
//
//   - It is idempotent on IdempotencyKey. A retry returns the original
//     outcome rather than rolling the dice again. Without this the mock would
//     hide exactly the bug docs/DESIGN-INVARIANTS.md §4 is about.
//   - Its outcome is a deterministic function of the key, not random per
//     call. That makes a failing integration test reproducible: the same
//     order id always fails the same way.
type Mock struct {
	// DeclineRate is the fraction of NEW authorisations that are declined.
	// 0.1 locally, so roughly one checkout in ten exercises compensation.
	DeclineRate float64
	// UnknownRate produces the nastiest case: we do not know what happened.
	// Small, because it should be rare, but non-zero because the reconciler
	// needs something to reconcile.
	UnknownRate float64
	// Latency simulates a real provider round trip so that timeout handling
	// and the 250ms/5s budgets are meaningful locally.
	Latency time.Duration

	mu      sync.Mutex
	results map[string]AuthorizeResult // idempotency key -> first outcome
	charges map[string]int             // key -> how many times money "moved"
}

func NewMock(declineRate, unknownRate float64, latency time.Duration) *Mock {
	return &Mock{
		DeclineRate: declineRate,
		UnknownRate: unknownRate,
		Latency:     latency,
		results:     map[string]AuthorizeResult{},
		charges:     map[string]int{},
	}
}

func (m *Mock) Name() string { return "mock" }

func (m *Mock) SupportsCapture(PaymentMethod) bool { return true }

func (m *Mock) Health(context.Context) error { return nil }

func (m *Mock) Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResult, error) {
	if err := m.sleep(ctx); err != nil {
		return AuthorizeResult{Outcome: OutcomeUnknown}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Idempotent replay. This is the behaviour that makes the mock useful:
	// it is what a real provider does, and code that only works against a
	// mock without it will double-charge in production.
	if prev, seen := m.results[req.IdempotencyKey]; seen {
		slog.DebugContext(ctx, "mock psp: replaying a previous outcome",
			slog.String("orderId", req.OrderID), slog.String("outcome", string(prev.Outcome)))
		return prev, nil
	}

	// Deterministic on the key, so a failing test is reproducible.
	roll := deterministicRoll(req.IdempotencyKey)

	var res AuthorizeResult
	switch {
	case roll < m.UnknownRate:
		// Charged or not? Nobody knows. The saga must not compensate; the
		// reconciler has to ask.
		res = AuthorizeResult{Outcome: OutcomeUnknown}
		m.charges[req.IdempotencyKey]++ // it DID move, which is the trap
		m.results[req.IdempotencyKey] = res
		return res, fmt.Errorf("%w: mock provider lost the response", ErrOutcomeUnknown)

	case roll < m.UnknownRate+m.DeclineRate:
		res = AuthorizeResult{
			Outcome:     OutcomeDeclined,
			ReasonCode:  declineFor(roll),
			DeclineCode: "MOCK_DECLINE",
			RawResponse: map[string]any{"mock": true},
		}

	default:
		m.charges[req.IdempotencyKey]++
		res = AuthorizeResult{
			Outcome:     OutcomeApproved,
			ProviderRef: "mock_txn_" + shortHash(req.IdempotencyKey),
			OrderRef:    "mock_ord_" + shortHash(req.OrderID),
			AuthCode:    strings.ToUpper(shortHash(req.PaymentID))[:6],
			ExpiresAt:   time.Now().Add(7 * 24 * time.Hour),
			RawResponse: map[string]any{"mock": true},
		}
	}

	m.results[req.IdempotencyKey] = res
	return res, nil
}

func (m *Mock) Capture(ctx context.Context, req CaptureRequest) (Result, error) {
	if err := m.sleep(ctx); err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}
	return Result{Outcome: OutcomeApproved, ProviderRef: req.ProviderRef}, nil
}

func (m *Mock) Void(ctx context.Context, req VoidRequest) (Result, error) {
	if err := m.sleep(ctx); err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}
	m.mu.Lock()
	// A void undoes the charge, so the invariant test below stays meaningful.
	if n := m.charges[req.IdempotencyKey]; n > 0 {
		m.charges[req.IdempotencyKey] = n - 1
	}
	m.mu.Unlock()
	return Result{Outcome: OutcomeApproved, ProviderRef: req.ProviderRef}, nil
}

func (m *Mock) Refund(ctx context.Context, req RefundRequest) (Result, error) {
	if err := m.sleep(ctx); err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}
	return Result{Outcome: OutcomeApproved, ProviderRef: "mock_refund_" + shortHash(req.RefundID)}, nil
}

// ParseCallback: the mock never sends callbacks, so anything arriving at the
// webhook while it is configured is either a misrouted real provider or an
// attacker. Refusing is the only correct answer.
func (m *Mock) ParseCallback(context.Context, []byte, map[string]string, map[string]string) (Callback, error) {
	return Callback{}, fmt.Errorf("%w: the mock provider does not send callbacks", ErrNotSupported)
}

// ChargeCount exposes how many times money actually moved for a key. Tests
// assert this is never greater than one — the AtMostOneCharge property from
// payment-service internal/psp/paymob_test.go, checked against the code rather than the model.
func (m *Mock) ChargeCount(idempotencyKey string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.charges[idempotencyKey]
}

func (m *Mock) sleep(ctx context.Context) error {
	if m.Latency <= 0 {
		return nil
	}
	// Jitter around the configured latency so local timing is not suspiciously
	// uniform and a p99 in a load test means something.
	d := m.Latency/2 + time.Duration(rand.Int63n(int64(m.Latency)))
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: context cancelled after %v", ErrOutcomeUnknown, d)
	}
}

// deterministicRoll maps a key onto [0,1) stably. The same order always gets
// the same fate, which is what makes a local failure reproducible.
func deterministicRoll(key string) float64 {
	sum := sha256.Sum256([]byte(key))
	return float64(binary.BigEndian.Uint32(sum[:4])) / float64(^uint32(0))
}

func declineFor(roll float64) ReasonCode {
	// Spread across the reasons so the storefront's failure copy for each one
	// is actually seen during development rather than discovered in
	// production.
	switch int(roll*1000) % 4 {
	case 0:
		return ReasonInsufficientFunds
	case 1:
		return ReasonCardExpired
	case 2:
		return ReasonThreeDSFailed
	default:
		return ReasonCardDeclined
	}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:6])
}
