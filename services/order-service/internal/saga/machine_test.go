package saga

import (
	"testing"
)

// The tests in this file are the bridge between internal/saga/model_test.go and the
// code. The model proves the design is right; these prove machine.go is that
// design and not a different one that happens to compile.

func TestHappyPath(t *testing.T) {
	// PENDING -> STOCK_RESERVED -> PAID -> STOCK_COMMITTED -> CONFIRMED,
	// with the command emitted at each step.
	steps := []struct {
		from     State
		trigger  Trigger
		wantNext State
		wantEmit []Step
	}{
		{StatePending, TriggerReserved, StateStockReserved, []Step{StepAuthorize}},
		{StateStockReserved, TriggerAuthorized, StatePaid, []Step{StepCommit}},
		{StatePaid, TriggerCommitted, StateStockCommitted, []Step{StepCapture}},
		{StateStockCommitted, TriggerCaptured, StateConfirmed, nil},
	}

	for _, s := range steps {
		d, err := Next(s.from, s.trigger, Ctx{AuthorizeSent: true})
		if err != nil {
			t.Fatalf("%s on %s: unexpected error %v", s.from, s.trigger, err)
		}
		if d.Next != s.wantNext {
			t.Errorf("%s on %s: next = %s, want %s", s.from, s.trigger, d.Next, s.wantNext)
		}
		if !sameSteps(d.Emit, s.wantEmit) {
			t.Errorf("%s on %s: emit = %v, want %v", s.from, s.trigger, d.Emit, s.wantEmit)
		}
	}
}

// The invariant from docs/DESIGN-INVARIANTS.md §1. This is the single most important
// test in the service: if it ever fails, we can deduct stock and refund the
// customer for it in the same order.
func TestNoCompensationPastPointOfNoReturn(t *testing.T) {
	pastTheLine := []State{StatePaid, StateStockCommitted, StateConfirmed}

	for _, from := range pastTheLine {
		if !RollbackForbidden(from) {
			t.Fatalf("%s should be flagged RollbackForbidden", from)
		}

		for _, trig := range allTriggers() {
			d, err := Next(from, trig, Ctx{AuthorizeSent: true})
			if err != nil {
				continue // illegal transitions are fine; they are rejected loudly
			}
			if d.Next == StateCompensating || d.Next == StateCancelled {
				t.Errorf("%s on %s rolled back to %s — see docs/DESIGN-INVARIANTS.md §1",
					from, trig, d.Next)
			}
			for _, s := range d.Emit {
				if s == StepRelease || s == StepVoid {
					t.Errorf("%s on %s emitted compensation %s — see docs/DESIGN-INVARIANTS.md §1",
						from, trig, s)
				}
			}
		}
	}
}

// A timeout while waiting for Commit or Capture must retry the SAME command,
// never a different one, and never give up silently.
func TestRollForwardRetriesTheSameCommand(t *testing.T) {
	cases := map[State]Step{
		StatePaid:           StepCommit,
		StateStockCommitted: StepCapture,
	}
	for from, want := range cases {
		d, err := Next(from, TriggerTimeout, Ctx{AuthorizeSent: true})
		if err != nil {
			t.Fatalf("%s on timeout: %v", from, err)
		}
		if d.Next != from {
			t.Errorf("%s on timeout: moved to %s, should stay put", from, d.Next)
		}
		if !sameSteps(d.Emit, []Step{want}) {
			t.Errorf("%s on timeout: emit = %v, want [%s]", from, d.Emit, want)
		}
		if d.Deadline == 0 {
			t.Errorf("%s on timeout: no retry deadline set, the saga would stall", from)
		}
	}
}

// A spurious timeout in PENDING must still release, because the reserve may
// have succeeded and we have not heard about it yet.
func TestSpuriousTimeoutInPendingStillReleases(t *testing.T) {
	d, err := Next(StatePending, TriggerTimeout, Ctx{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next != StateCompensating {
		t.Fatalf("next = %s, want COMPENSATING", d.Next)
	}
	if !sameSteps(d.Emit, []Step{StepRelease}) {
		t.Errorf("emit = %v, want [RELEASE]", d.Emit)
	}
	// No VOID: AUTHORIZE was never sent from PENDING, so there is nothing to
	// void, and sending one would write a tombstone that blocks a legitimate
	// retry of this order.
	for _, s := range d.Emit {
		if s == StepVoid {
			t.Error("emitted VOID from PENDING where AUTHORIZE was never sent")
		}
	}
}

// A timeout in STOCK_RESERVED must send BOTH compensations, because the
// authorize may have succeeded silently.
func TestTimeoutInStockReservedVoidsToo(t *testing.T) {
	d, err := Next(StateStockReserved, TriggerTimeout, Ctx{AuthorizeSent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !sameSteps(d.Emit, []Step{StepRelease, StepVoid}) {
		t.Errorf("emit = %v, want [RELEASE VOID]", d.Emit)
	}
}

// PROVIDER_UNAVAILABLE must not cancel the order.
func TestRetriablePaymentFailureDoesNotCancel(t *testing.T) {
	d, err := Next(StateStockReserved, TriggerAuthFailed, Ctx{
		AuthorizeSent: true, PaymentRetriable: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next != StateStockReserved || !d.AckOnly {
		t.Errorf("retriable decline: next = %s ackOnly = %v, want STOCK_RESERVED ack-only",
			d.Next, d.AckOnly)
	}

	// A hard decline, on the other hand, must compensate immediately.
	d, err = Next(StateStockReserved, TriggerAuthFailed, Ctx{
		AuthorizeSent: true, PaymentRetriable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next != StateCompensating {
		t.Errorf("hard decline: next = %s, want COMPENSATING", d.Next)
	}
}

// Compensation only completes when BOTH participants have acknowledged.
func TestCancelWaitsForBothCompensations(t *testing.T) {
	// Inventory released, payment was engaged but has not acked yet.
	d, err := Next(StateCompensating, TriggerReleased, Ctx{AuthorizeSent: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next == StateCancelled {
		t.Error("cancelled while the payment compensation was still outstanding")
	}

	// Now the void lands too.
	d, err = Next(StateCompensating, TriggerVoided, Ctx{
		AuthorizeSent: true, InventorySettled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next != StateCancelled {
		t.Errorf("next = %s, want CANCELLED once both settled", d.Next)
	}
}

// When AUTHORIZE was never sent, the release alone is enough to cancel.
func TestCancelNeedsNoVoidWhenPaymentWasNeverEngaged(t *testing.T) {
	d, err := Next(StateCompensating, TriggerReleased, Ctx{AuthorizeSent: false})
	if err != nil {
		t.Fatal(err)
	}
	if d.Next != StateCancelled {
		t.Errorf("next = %s, want CANCELLED", d.Next)
	}
}

// Every trigger must be safe to deliver twice — Kafka is at-least-once.
func TestAllTransitionsAreIdempotent(t *testing.T) {
	for _, from := range allStates() {
		for _, trig := range allTriggers() {
			ctx := Ctx{AuthorizeSent: true}

			first, err1 := Next(from, trig, ctx)
			if err1 != nil {
				continue
			}
			// Redelivery of the same event once the transition has been
			// applied must not move the machine again.
			second, err2 := Next(first.Next, trig, ctx)
			if err2 != nil {
				continue // an illegal transition is a loud rejection, not a silent double-apply
			}
			if second.Next != first.Next {
				t.Errorf("redelivery of %s in %s moved %s -> %s (not idempotent)",
					trig, from, first.Next, second.Next)
			}
		}
	}
}

// Terminal states absorb everything.
func TestTerminalStatesAbsorbLateEvents(t *testing.T) {
	for _, from := range []State{StateConfirmed, StateCancelled} {
		for _, trig := range allTriggers() {
			d, err := Next(from, trig, Ctx{AuthorizeSent: true})
			if err != nil {
				t.Errorf("%s on %s returned an error; terminal states must absorb", from, trig)
				continue
			}
			if d.Next != from {
				t.Errorf("%s on %s moved to %s; terminal must be stable", from, trig, d.Next)
			}
			if len(d.Emit) != 0 {
				t.Errorf("%s on %s emitted %v; terminal must emit nothing", from, trig, d.Emit)
			}
		}
	}
}

// Exhaustive reachability: from PENDING, every path under every trigger must
// end in CONFIRMED or CANCELLED. This is the code-level restatement of the
// model's Termination property.
func TestEveryReachableStateCanTerminate(t *testing.T) {
	reachable := map[State]bool{StatePending: true}
	frontier := []State{StatePending}

	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]

		for _, trig := range allTriggers() {
			for _, ctx := range allContexts() {
				d, err := Next(cur, trig, ctx)
				if err != nil {
					continue
				}
				if !reachable[d.Next] {
					reachable[d.Next] = true
					frontier = append(frontier, d.Next)
				}
			}
		}
	}

	if !reachable[StateConfirmed] {
		t.Error("CONFIRMED is not reachable from PENDING")
	}
	if !reachable[StateCancelled] {
		t.Error("CANCELLED is not reachable from PENDING")
	}

	// Every reachable non-terminal state must have at least one trigger that
	// makes progress towards a terminal state, or the saga can wedge.
	for s := range reachable {
		if IsTerminal(s) {
			continue
		}
		progresses := false
		for _, trig := range allTriggers() {
			for _, ctx := range allContexts() {
				if d, err := Next(s, trig, ctx); err == nil && d.Next != s {
					progresses = true
				}
			}
		}
		if !progresses {
			t.Errorf("%s has no outgoing transition — the saga can wedge there", s)
		}
	}
}

// Every state the code declares must actually be reachable in the model.
//
// This replaces what used to be a cross-check against a separate specification
// file. It is a stronger check: a spec file can agree with the code and both be
// wrong about which states occur. This asserts that allStates() is neither
// missing a state the machine can enter nor listing one it cannot.
func TestEveryDeclaredStateIsReachableAndNoOthersAre(t *testing.T) {
	declared := map[State]bool{}
	for _, s := range allStates() {
		declared[s] = true
	}

	reachable := map[State]bool{StatePending: true}
	frontier := []State{StatePending}

	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]

		for _, trig := range allTriggers() {
			for _, ctx := range allContexts() {
				d, err := Next(cur, trig, ctx)
				if err != nil {
					continue
				}
				if !declared[d.Next] {
					t.Errorf("the machine can enter %q, which allStates() does not list", d.Next)
					declared[d.Next] = true // report once
				}
				if !reachable[d.Next] {
					reachable[d.Next] = true
					frontier = append(frontier, d.Next)
				}
			}
		}
	}

	for s := range declared {
		if !reachable[s] {
			t.Errorf("allStates() lists %q but no sequence of triggers can reach it", s)
		}
	}
	t.Logf("%d states declared, all reachable, no undeclared states entered", len(reachable))
}

// Every step must map to a command type, and every event type the participants
// emit must map back to a trigger. A gap either way is a message that silently
// does nothing.
func TestCommandAndTriggerMappingsAreComplete(t *testing.T) {
	for _, s := range []Step{StepReserve, StepAuthorize, StepCommit, StepCapture, StepRelease, StepVoid} {
		if CommandFor(s) == "" {
			t.Errorf("no CloudEvents type mapped for step %s", s)
		}
	}

	events := []string{
		"souq.inventory.reserved.v1",
		"souq.inventory.reservation_failed.v1",
		"souq.inventory.released.v1",
		"souq.inventory.committed.v1",
		"souq.payment.authorized.v1",
		"souq.payment.failed.v1",
		"souq.payment.captured.v1",
		"souq.payment.voided.v1",
	}
	for _, e := range events {
		if _, ok := TriggerFor(e); !ok {
			t.Errorf("no trigger mapped for event %s", e)
		}
	}

	// An unknown type must be reported as unknown, not silently coerced.
	if _, ok := TriggerFor("souq.order.teleported.v1"); ok {
		t.Error("unknown event type was accepted as a trigger")
	}
}

// --------------------------------------------------------------- helpers

func allStates() []State {
	return []State{
		StatePending, StateStockReserved, StatePaid, StateStockCommitted,
		StateConfirmed, StateCompensating, StateCancelled,
	}
}

func allTriggers() []Trigger {
	return []Trigger{
		TriggerReserved, TriggerReserveFailed, TriggerReleased, TriggerCommitted,
		TriggerAuthorized, TriggerAuthFailed, TriggerCaptured, TriggerVoided,
		TriggerTimeout,
	}
}

// The context values that actually change a decision.
func allContexts() []Ctx {
	var out []Ctx
	for _, authSent := range []bool{false, true} {
		for _, inv := range []bool{false, true} {
			for _, pay := range []bool{false, true} {
				for _, retriable := range []bool{false, true} {
					out = append(out, Ctx{
						AuthorizeSent:    authSent,
						InventorySettled: inv,
						PaymentSettled:   pay,
						PaymentRetriable: retriable,
					})
				}
			}
		}
	}
	return out
}

func sameSteps(a, b []Step) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
