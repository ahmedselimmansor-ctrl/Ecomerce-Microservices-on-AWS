package saga

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	mc "github.com/souq/go-modelcheck"
)

// Exhaustive state-space model of the order saga.
//
// This is the file that proves the design correct, and it does so by
// enumerating every interleaving of every action rather than by testing the
// orderings somebody thought of.
//
// The crucial property of this model is that it drives the REAL decision
// function. `sagaReact` below calls `Next()` — the same call the production
// orchestrator makes. There is no separate specification that can drift out of
// step with the implementation, because the specification IS the
// implementation plus an adversarial environment.
//
// # The adversary
//
// The environment models everything that makes a distributed saga hard:
//
//	DUPLICATION       a message is never removed from `net` once sent, so any
//	                  receiver can be re-triggered by it at any point, forever.
//	                  A handler that is not idempotent shows up immediately.
//	REORDERING        every enabled action is explored at every state, so all
//	                  delivery orders are covered, including a compensation
//	                  arriving before the thing it compensates.
//	SPURIOUS TIMEOUTS the orchestrator may time out while the reply is still in
//	                  flight. This is the single most common source of real
//	                  saga bugs and it is a first-class action here.
//	CRASHED PARTICIPANTS modelled by a participant action simply never being
//	                  scheduled on some branch; the timeout path must still
//	                  reach a terminal state.
//
// # Scope
//
// One order. Orders do not interact — no shared state, no shared reservation —
// so a second order multiplies the state space without covering a new class of
// interleaving. Concurrency BETWEEN orders is where they contend for stock,
// and that is modelled in inventory-service, against the SQL that actually
// arbitrates it.

// ---------------------------------------------------------------------------
// Messages
//
// A bitmask rather than a set, so Key() is cheap and the state is a value type
// the explorer can hash without allocating.

type msg uint16

const (
	mReserve msg = 1 << iota
	mRelease
	mCommit
	mAuthorize
	mCapture
	mVoid

	mReserved
	mReserveFailed
	mReleased
	mCommitted
	mAuthorized
	mAuthFailed
	mCaptured
	mVoided
)

var msgNames = []struct {
	bit  msg
	name string
}{
	{mReserve, "Reserve"}, {mRelease, "Release"}, {mCommit, "Commit"},
	{mAuthorize, "Authorize"}, {mCapture, "Capture"}, {mVoid, "Void"},
	{mReserved, "Reserved"}, {mReserveFailed, "ReserveFailed"},
	{mReleased, "Released"}, {mCommitted, "Committed"},
	{mAuthorized, "Authorized"}, {mAuthFailed, "AuthFailed"},
	{mCaptured, "Captured"}, {mVoided, "Voided"},
}

func (m msg) has(b msg) bool { return m&b != 0 }

func (m msg) String() string {
	var parts []string
	for _, n := range msgNames {
		if m.has(n.bit) {
			parts = append(parts, n.name)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// eventToTrigger maps a learned event onto the trigger the state machine takes.
var eventToTrigger = map[msg]Trigger{
	mReserved:      TriggerReserved,
	mReserveFailed: TriggerReserveFailed,
	mReleased:      TriggerReleased,
	mCommitted:     TriggerCommitted,
	mAuthorized:    TriggerAuthorized,
	mAuthFailed:    TriggerAuthFailed,
	mCaptured:      TriggerCaptured,
	mVoided:        TriggerVoided,
}

// stepToCommand maps an emitted step onto the message it puts on the wire.
var stepToCommand = map[Step]msg{
	StepReserve:   mReserve,
	StepRelease:   mRelease,
	StepCommit:    mCommit,
	StepAuthorize: mAuthorize,
	StepCapture:   mCapture,
	StepVoid:      mVoid,
}

// ---------------------------------------------------------------------------
// Participant states

type invState uint8

const (
	invNone invState = iota
	invReserved
	invCommitted
	invReleased
	invFailed
)

func (s invState) String() string {
	return [...]string{"NONE", "RESERVED", "COMMITTED", "RELEASED", "FAILED"}[s]
}

type payState uint8

const (
	payNone payState = iota
	payAuthorized
	payCaptured
	payVoided
	payFailed
)

func (s payState) String() string {
	return [...]string{"NONE", "AUTHORIZED", "CAPTURED", "VOIDED", "FAILED"}[s]
}

// ---------------------------------------------------------------------------
// The model state

type world struct {
	saga State
	inv  invState
	pay  payState

	// net never shrinks. That is how at-least-once delivery is modelled: a
	// message stays deliverable forever, so every handler must tolerate being
	// called with it repeatedly.
	net msg

	// learned is the orchestrator's inbox — the events it has already applied.
	// An event in `net` but not in `learned` is in flight.
	learned msg

	// allowRollbackAfterCommit reproduces the bug documented in
	// docs/DESIGN-INVARIANTS.md §1. Off in the shipped model; the regression
	// test turns it on and asserts the invariants FAIL.
	allowRollbackAfterCommit bool
}

func (w world) Key() string {
	return fmt.Sprintf("s=%s|i=%s|p=%s|net=%d|learned=%d",
		w.saga, w.inv, w.pay, w.net, w.learned)
}

func (w world) Terminal() bool { return IsTerminal(w.saga) }

// ctx builds the facts the state machine needs, from the world. Exactly what
// the real orchestrator derives from `saga_steps`.
func (w world) ctx() Ctx {
	return Ctx{
		AuthorizeSent:    w.net.has(mAuthorize),
		InventorySettled: w.learned.has(mReleased),
		PaymentSettled:   w.learned.has(mVoided),
		// Explored both ways by the two react actions below.
		PaymentRetriable: false,
	}
}

// ---------------------------------------------------------------------------
// Actions

// sagaStart emits the first Reserve, inside the same transaction that
// persisted the order.
var sagaStart = mc.Action[world]{
	Name: "saga.start",
	Fn: func(w world) (world, bool) {
		if w.saga != StatePending || w.net.has(mReserve) {
			return w, false
		}
		w.net |= mReserve
		return w, true
	},
}

// sagaObserve moves one in-flight event into the inbox. Separating "receive"
// from "react" is not decoration — it is how the real consumer works (write
// to processed_events, then apply), and it doubles the interleavings the
// explorer has to consider.
func sagaObserve(event msg, name string) mc.Action[world] {
	return mc.Action[world]{
		Name: "saga.observe:" + name,
		Fn: func(w world) (world, bool) {
			if !w.net.has(event) || w.learned.has(event) {
				return w, false
			}
			w.learned |= event
			return w, true
		},
	}
}

// sagaReact applies a learned event through the REAL state machine.
//
// This is the whole point of the file. Everything the model concludes about
// the saga is a conclusion about `Next()`, not about a paraphrase of it.
func sagaReact(event msg, name string, retriable bool) mc.Action[world] {
	suffix := ""
	if retriable {
		suffix = "(retriable)"
	}
	return mc.Action[world]{
		Name: "saga.react:" + name + suffix,
		Fn: func(w world) (world, bool) {
			if !w.learned.has(event) {
				return w, false
			}
			trigger, ok := eventToTrigger[event]
			if !ok {
				return w, false
			}

			c := w.ctx()
			c.PaymentRetriable = retriable

			d, err := Next(w.saga, trigger, c)
			if err != nil {
				// An illegal transition. The machine refused, which is the
				// correct behaviour — the state does not change and there is
				// nothing to explore down this branch.
				return w, false
			}
			return applyDecision(w, d), true
		},
	}
}

// sagaTimeout fires a timeout. Enabled in any non-terminal state, because a
// timeout can always fire — including while the reply is in flight, which is
// the race that breaks naive implementations.
var sagaTimeout = mc.Action[world]{
	Name: "saga.timeout",
	Fn: func(w world) (world, bool) {
		if IsTerminal(w.saga) {
			return w, false
		}
		d, err := Next(w.saga, TriggerTimeout, w.ctx())
		if err != nil {
			return w, false
		}
		return applyDecision(w, d), true
	},
}

// sagaTimeoutBUG bypasses the state machine to compensate from PAID.
//
// This is the design that the explorer originally found a counterexample for, kept as a
// permanent regression pin. It is only enabled when
// allowRollbackAfterCommit is set, and the test that sets it asserts the
// invariants FAIL. If that test ever starts passing, the invariant has been
// weakened and this protection is gone.
var sagaTimeoutBUG = mc.Action[world]{
	Name: "saga.timeout.ROLLBACK_AFTER_COMMIT",
	Fn: func(w world) (world, bool) {
		if !w.allowRollbackAfterCommit || w.saga != StatePaid {
			return w, false
		}
		w.saga = StateCompensating
		w.net |= mRelease | mVoid
		return w, true
	},
}

func applyDecision(w world, d Decision) world {
	w.saga = d.Next
	for _, step := range d.Emit {
		w.net |= stepToCommand[step]
	}
	return w
}

// --------------------------------------------------------------- inventory

var invReserveOk = mc.Action[world]{
	Name: "inv.reserve.ok",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mReserve) || w.inv != invNone {
			return w, false
		}
		w.inv = invReserved
		w.net |= mReserved
		return w, true
	},
}

var invReserveOutOfStock = mc.Action[world]{
	Name: "inv.reserve.out-of-stock",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mReserve) || w.inv != invNone {
			return w, false
		}
		w.inv = invFailed
		w.net |= mReserveFailed
		return w, true
	},
}

var invRelease = mc.Action[world]{
	Name: "inv.release",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mRelease) || w.inv != invReserved {
			return w, false
		}
		w.inv = invReleased
		w.net |= mReleased
		return w, true
	},
}

// invReleaseTombstone: a Release for a reservation that does not exist yet.
//
// Remove this action and the model finds a wedged state in five steps: the
// saga times out, releases nothing, the late Reserve creates a reservation
// nobody will release, and the saga waits forever for a Released that never
// comes. docs/DESIGN-INVARIANTS.md §2.
var invReleaseTombstone = mc.Action[world]{
	Name: "inv.release.tombstone",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mRelease) || w.inv != invNone {
			return w, false
		}
		w.inv = invReleased
		w.net |= mReleased
		return w, true
	},
}

var invCommit = mc.Action[world]{
	Name: "inv.commit",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mCommit) || w.inv != invReserved {
			return w, false
		}
		w.inv = invCommitted
		w.net |= mCommitted
		return w, true
	},
}

// Deliberately absent: no transition out of invCommitted. Committed stock is
// final, and a late Release against it is rejected rather than honoured.

// ----------------------------------------------------------------- payment

var payAuthorizeOk = mc.Action[world]{
	Name: "pay.authorize.ok",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mAuthorize) || w.pay != payNone {
			return w, false
		}
		w.pay = payAuthorized
		w.net |= mAuthorized
		return w, true
	},
}

var payAuthorizeDeclined = mc.Action[world]{
	Name: "pay.authorize.declined",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mAuthorize) || w.pay != payNone {
			return w, false
		}
		w.pay = payFailed
		w.net |= mAuthFailed
		return w, true
	},
}

var payVoid = mc.Action[world]{
	Name: "pay.void",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mVoid) || w.pay != payAuthorized {
			return w, false
		}
		w.pay = payVoided
		w.net |= mVoided
		return w, true
	},
}

// Same tombstone argument as inventory: a Void can overtake its Authorize.
var payVoidTombstone = mc.Action[world]{
	Name: "pay.void.tombstone",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mVoid) || w.pay != payNone {
			return w, false
		}
		w.pay = payVoided
		w.net |= mVoided
		return w, true
	},
}

var payCapture = mc.Action[world]{
	Name: "pay.capture",
	Fn: func(w world) (world, bool) {
		if !w.net.has(mCapture) || w.pay != payAuthorized {
			return w, false
		}
		w.pay = payCaptured
		w.net |= mCaptured
		return w, true
	},
}

// ---------------------------------------------------------------------------
// Invariants

var invariants = []mc.Invariant[world]{
	{
		// We must never take a customer's money for stock we did not set aside.
		Name: "NoMoneyWithoutStock",
		Fn: func(w world) error {
			if w.pay == payCaptured && w.inv != invCommitted {
				return fmt.Errorf("payment CAPTURED but inventory is %s", w.inv)
			}
			return nil
		},
	},
	{
		// We must never deduct stock we were not paid for. AUTHORIZED counts:
		// the funds are ring-fenced and capture is guaranteed within the
		// authorisation window.
		Name: "NoStockWithoutMoney",
		Fn: func(w world) error {
			if w.inv == invCommitted && w.pay != payAuthorized && w.pay != payCaptured {
				return fmt.Errorf("inventory COMMITTED but payment is %s", w.pay)
			}
			return nil
		},
	},
	{
		// Once captured, never voided. Together with there being exactly one
		// capture transition, this gives at-most-once.
		Name: "NoDoubleCharge",
		Fn: func(w world) error {
			if w.pay == payCaptured && w.net.has(mVoid) {
				return fmt.Errorf("a Void was issued for a CAPTURED payment")
			}
			return nil
		},
	},
	{
		// A cancelled order holds no stock and no money.
		Name: "NoDanglingReservation",
		Fn: func(w world) error {
			if w.saga != StateCancelled {
				return nil
			}
			if w.inv != invReleased && w.inv != invFailed {
				return fmt.Errorf("order CANCELLED but inventory is %s", w.inv)
			}
			if w.pay != payNone && w.pay != payVoided && w.pay != payFailed {
				return fmt.Errorf("order CANCELLED but payment is %s", w.pay)
			}
			return nil
		},
	},
	{
		// A confirmed order really is settled on both sides.
		Name: "ConsistentTerminalState",
		Fn: func(w world) error {
			if w.saga != StateConfirmed {
				return nil
			}
			if w.inv != invCommitted || w.pay != payCaptured {
				return fmt.Errorf("order CONFIRMED but inventory=%s payment=%s", w.inv, w.pay)
			}
			return nil
		},
	},
}

func allActions(includeBug bool) []mc.Action[world] {
	actions := []mc.Action[world]{sagaStart, sagaTimeout}

	for event, name := range map[msg]string{
		mReserved: "Reserved", mReserveFailed: "ReserveFailed",
		mReleased: "Released", mCommitted: "Committed",
		mAuthorized: "Authorized", mAuthFailed: "AuthFailed",
		mCaptured: "Captured", mVoided: "Voided",
	} {
		actions = append(actions, sagaObserve(event, name))
		actions = append(actions, sagaReact(event, name, false))
	}

	// AuthFailed is explored both ways: a retriable provider outage must not
	// cancel the order, a hard decline must compensate immediately.
	actions = append(actions, sagaReact(mAuthFailed, "AuthFailed", true))

	actions = append(actions,
		invReserveOk, invReserveOutOfStock, invRelease, invReleaseTombstone, invCommit,
		payAuthorizeOk, payAuthorizeDeclined, payVoid, payVoidTombstone, payCapture,
	)

	if includeBug {
		actions = append(actions, sagaTimeoutBUG)
	}
	return actions
}

// ---------------------------------------------------------------------------
// The tests

// THE test. Exhausts every interleaving and asserts every safety property,
// plus that no reachable state is wedged.
func TestSagaModelIsSafeAndAlwaysTerminates(t *testing.T) {
	res := mc.Explore(mc.Model[world]{
		Initial:    world{saga: StatePending},
		Actions:    allActions(false),
		Invariants: invariants,
		MaxStates:  200_000,
	})

	if !res.OK() {
		t.Fatalf("the saga model is not sound:\n%s", res.Report())
	}

	t.Logf("%s", res.Report())

	// A green model that explored the wrong thing is worse than a red one.
	// Counting states proves nothing — a guard that is accidentally always
	// false gives a small, clean, meaningless result. So assert COVERAGE:
	// the specific situations this model exists to reason about must actually
	// have occurred.
	assertCoverage(t, res, map[string]func(world) bool{
		"an order reaches CONFIRMED":                func(w world) bool { return w.saga == StateConfirmed },
		"an order reaches CANCELLED":                func(w world) bool { return w.saga == StateCancelled },
		"money is captured against committed stock": func(w world) bool { return w.pay == payCaptured && w.inv == invCommitted },
		"a payment is voided during compensation":   func(w world) bool { return w.pay == payVoided },
		"stock is released during compensation":     func(w world) bool { return w.inv == invReleased },
		"a reservation fails for lack of stock":     func(w world) bool { return w.inv == invFailed },
		"a card is declined":                        func(w world) bool { return w.pay == payFailed },
		"a timeout fires while a reply is still in flight": func(w world) bool {
			return w.saga == StateCompensating && w.net.has(mReserved) && !w.learned.has(mReserved)
		},
		"a Release overtakes its Reserve (the tombstone path)": func(w world) bool { return w.net.has(mRelease) && w.inv == invReleased && !w.net.has(mReserved) },
		"a Void overtakes its Authorize":                       func(w world) bool { return w.pay == payVoided && !w.net.has(mAuthorized) },
		"the saga passes the point of no return":               func(w world) bool { return w.saga == StateStockCommitted },
	})
}

// assertCoverage re-walks the reachable states and fails if a situation the
// model is supposed to reason about never actually arose.
func assertCoverage(t *testing.T, res mc.Result[world], want map[string]func(world) bool) {
	t.Helper()

	hit := map[string]bool{}
	for _, w := range res.Reached {
		for name, pred := range want {
			if !hit[name] && pred(w) {
				hit[name] = true
			}
		}
	}

	var missing []string
	for name := range want {
		if !hit[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	for _, m := range missing {
		t.Errorf("the model never reached: %s", m)
	}
	if len(missing) == 0 {
		t.Logf("coverage: all %d modelled situations were reached", len(want))
	}
}

// The regression pin, replacing what OrderSagaBug.cfg used to hold.
//
// Allowing compensation from PAID must produce a counterexample. If this test
// ever passes, NoStockWithoutMoney has been weakened and the protection
// documented in docs/DESIGN-INVARIANTS.md §1 is gone.
func TestRollbackAfterCommitIsUnsafe(t *testing.T) {
	res := mc.Explore(mc.Model[world]{
		Initial:    world{saga: StatePending, allowRollbackAfterCommit: true},
		Actions:    allActions(true),
		Invariants: invariants,
		MaxStates:  200_000,
	})

	if res.Violation == nil {
		t.Fatal("allowing rollback past the point of no return did NOT produce a violation.\n" +
			"Either an invariant was weakened or the bug action no longer reproduces it.\n" +
			"See docs/DESIGN-INVARIANTS.md §1 — this test failing means the protection is gone.")
	}

	if res.Violation.Invariant != "NoStockWithoutMoney" {
		t.Errorf("expected NoStockWithoutMoney to catch it, got %s", res.Violation.Invariant)
	}

	t.Logf("the counterexample this design produces (%d steps):\n%s",
		len(res.Violation.Trace.Actions), res.Violation.Trace)
}

// The tombstone pin, replacing the reasoning in FINDINGS §2.
//
// Without the tombstone, a Release that overtakes its Reserve leaves a
// reservation nobody will ever release and the saga can never finish.
func TestWithoutTombstonesTheSagaCanWedge(t *testing.T) {
	actions := allActions(false)

	// Drop the two tombstone actions.
	var withoutTombstones []mc.Action[world]
	for _, a := range actions {
		if strings.Contains(a.Name, "tombstone") {
			continue
		}
		withoutTombstones = append(withoutTombstones, a)
	}

	res := mc.Explore(mc.Model[world]{
		Initial:    world{saga: StatePending},
		Actions:    withoutTombstones,
		Invariants: invariants,
		MaxStates:  200_000,
	})

	if res.OK() {
		t.Fatal("removing the tombstone handling did NOT break the model.\n" +
			"Either the model no longer explores the release-overtakes-reserve race, " +
			"or the liveness check is not working. See docs/DESIGN-INVARIANTS.md §2.")
	}

	if len(res.Wedged) == 0 && len(res.Deadlocks) == 0 {
		t.Errorf("expected a wedged or deadlocked state, got: %s", res.Report())
	}

	t.Logf("without tombstones: %d wedged, %d deadlocked", len(res.Wedged), len(res.Deadlocks))
}

// Every transition must survive being applied twice — Kafka is at-least-once.
// The model already covers this implicitly (messages are never removed from
// `net`), but asserting it directly gives a clearer failure.
func TestEveryReactionIsIdempotentUnderRedelivery(t *testing.T) {
	for event, name := range map[msg]string{
		mReserved: "Reserved", mReserveFailed: "ReserveFailed",
		mReleased: "Released", mCommitted: "Committed",
		mAuthorized: "Authorized", mAuthFailed: "AuthFailed",
		mCaptured: "Captured", mVoided: "Voided",
	} {
		for _, from := range allStates() {
			w := world{saga: from, learned: event, net: event | mAuthorize}

			react := sagaReact(event, name, false)
			once, applied := react.Fn(w)
			if !applied {
				continue
			}
			twice, _ := react.Fn(once)

			if twice.saga != once.saga {
				t.Errorf("redelivering %s in %s moved the saga %s -> %s",
					name, from, once.saga, twice.saga)
			}
		}
	}
}
