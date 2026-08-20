// Package saga implements the order orchestration saga.
//
// model_test.go in this package drives THIS function through an exhaustive
// state-space search: every interleaving of every participant action, with
// duplication, reordering and spurious timeouts. There is no separate
// specification to keep in step — the model calls Next() directly, so a change
// here is a change to what is being verified.
//
// Read docs/DESIGN-INVARIANTS.md before changing anything in this file. Two of the
// rules encoded here look like oversights and are not:
//
//  1. PAID and STOCK_COMMITTED have no compensating transition. Once the
//     Commit command is emitted, the saga rolls forward or pages a human.
//     Adding a rollback there reintroduces a bug that loses stock (§1).
//  2. Compensation commands must be handled by participants that have never
//     heard of the thing being compensated. That is the tombstone rule (§2)
//     and it lives on the participant side, but the orchestrator relies on
//     it to guarantee termination.
package saga

import (
	"fmt"
	"time"
)

// State is a saga state. The values match OrderStatus in
// libs/ts-contracts/src/api.ts and the CHECK constraint on orders.status.
type State string

const (
	StatePending        State = "PENDING"
	StateStockReserved  State = "STOCK_RESERVED"
	StatePaid           State = "PAID"
	StateStockCommitted State = "STOCK_COMMITTED"
	StateConfirmed      State = "CONFIRMED"
	StateCompensating   State = "COMPENSATING"
	StateCancelled      State = "CANCELLED"
)

// Step is a command the orchestrator sends to a participant.
type Step string

const (
	StepReserve   Step = "RESERVE"
	StepAuthorize Step = "AUTHORIZE"
	StepCommit    Step = "COMMIT"
	StepCapture   Step = "CAPTURE"
	StepRelease   Step = "RELEASE"
	StepVoid      Step = "VOID"
)

// Trigger is what moves the machine: an inbound event, or a timeout.
type Trigger string

const (
	TriggerReserved      Trigger = "inventory.reserved"
	TriggerReserveFailed Trigger = "inventory.reservation_failed"
	TriggerReleased      Trigger = "inventory.released"
	TriggerCommitted     Trigger = "inventory.committed"
	TriggerAuthorized    Trigger = "payment.authorized"
	TriggerAuthFailed    Trigger = "payment.failed"
	TriggerCaptured      Trigger = "payment.captured"
	TriggerVoided        Trigger = "payment.voided"
	TriggerTimeout       Trigger = "saga.timeout"
)

// CancelReason mirrors the enum in the AsyncAPI contract.
type CancelReason string

const (
	ReasonInsufficientStock  CancelReason = "INSUFFICIENT_STOCK"
	ReasonPaymentDeclined    CancelReason = "PAYMENT_DECLINED"
	ReasonPaymentTimeout     CancelReason = "PAYMENT_TIMEOUT"
	ReasonReservationTimeout CancelReason = "RESERVATION_TIMEOUT"
	ReasonCustomerCancelled  CancelReason = "CUSTOMER_CANCELLED"
	ReasonFraudRejected      CancelReason = "FRAUD_REJECTED"
)

// Timeouts per docs/CONTRACTS.md §4. A zero value means "never time out",
// which is only correct past the point of no return.
const (
	TimeoutPending       = 30 * time.Second
	TimeoutStockReserved = 120 * time.Second
	TimeoutCompensating  = 300 * time.Second

	// Past the point of no return we retry with backoff instead of timing out.
	RetryCommitInterval   = 5 * time.Second
	RetryCaptureInterval  = 5 * time.Second
	MaxRollForwardRetries = 5
)

// Decision is what the orchestrator should do as a result of a trigger. It is
// deliberately data rather than side effects: the caller applies it inside one
// database transaction alongside the outbox write, which is what makes the
// whole thing atomic.
type Decision struct {
	// Next is the state to persist. Equal to the current state when the
	// trigger was a duplicate or is not relevant here.
	Next State

	// Emit are the commands to write to the outbox, in order.
	Emit []Step

	// AckOnly is true when the trigger was correctly received but causes no
	// transition — a late event for a state we have already left. The consumer
	// must still commit its Kafka offset, or it will spin on the message.
	AckOnly bool

	// Reason is set when Next is CANCELLED.
	Reason CancelReason

	// FailedStep records where the saga gave up, for support and analytics.
	FailedStep Step

	// Deadline is when the newly-entered state should be swept if nothing has
	// happened. Zero means no timeout applies.
	Deadline time.Duration
}

// Ctx is everything the machine needs to know beyond the current state.
// It is a value, not an interface, because the machine must stay pure —
// it does no I/O, which is what makes it exhaustively testable.
type Ctx struct {
	// AuthorizeSent records whether the AUTHORIZE command was ever emitted.
	// The orchestrator needs it to decide whether cancelling requires a VOID.
	// This is PaySettledForCancel in the state-space model.
	AuthorizeSent bool

	// InventorySettled / PaymentSettled are the acknowledgements the
	// orchestrator has actually received. Compensation is only complete when
	// both are true — anything else and we would cancel an order while a
	// participant still holds resources for it.
	InventorySettled bool
	PaymentSettled   bool

	// RollForwardAttempts counts Commit/Capture retries past the point of no
	// return. At MaxRollForwardRetries the caller raises orders_stuck.
	RollForwardAttempts int

	// PaymentRetriable distinguishes PROVIDER_UNAVAILABLE (back off and retry)
	// from CARD_DECLINED (compensate now). Getting this wrong either hammers a
	// struggling PSP or cancels an order the customer could still pay for.
	PaymentRetriable bool
}

// ErrIllegalTransition is returned for a trigger that cannot occur in the
// current state under any interleaving the model permits. It means a real bug,
// not a race — a race produces AckOnly instead.
type ErrIllegalTransition struct {
	From    State
	Trigger Trigger
}

func (e ErrIllegalTransition) Error() string {
	return fmt.Sprintf("saga: illegal transition from %s on %s", e.From, e.Trigger)
}

// Start returns the decision for a freshly accepted order. Corresponds to
// SagaStart in the model.
func Start() Decision {
	return Decision{
		Next:     StatePending,
		Emit:     []Step{StepReserve},
		Deadline: TimeoutPending,
	}
}

// Next applies a trigger. It is a pure function: same inputs, same output,
// no clock, no network, no database. Every branch below has a counterpart
// action in internal/saga/model_test.go, named in the comment above it.
func Next(from State, t Trigger, c Ctx) (Decision, error) {
	switch from {

	// -------------------------------------------------------------- PENDING
	case StatePending:
		switch t {
		case TriggerReserved: // SagaOnReserved
			return Decision{
				Next:     StateStockReserved,
				Emit:     []Step{StepAuthorize},
				Deadline: TimeoutStockReserved,
			}, nil

		case TriggerReserveFailed: // SagaOnReserveFailed
			// Nothing to compensate: AUTHORIZE was never sent, so there is no
			// payment to void and the reservation already failed closed.
			return Decision{
				Next:       StateCancelled,
				Reason:     ReasonInsufficientStock,
				FailedStep: StepReserve,
			}, nil

		case TriggerTimeout: // SagaTimeoutPending
			// The reserve reply may still be in flight. Release is safe
			// anyway: if inventory has not seen the reserve yet it writes a
			// tombstone, and a late reserve is then rejected (FINDINGS §2).
			return Decision{
				Next:     StateCompensating,
				Emit:     []Step{StepRelease},
				Deadline: TimeoutCompensating,
			}, nil

		case TriggerReleased, TriggerVoided:
			// Compensation ack for a compensation we have not started. Only
			// reachable on redelivery after a restart; harmless.
			return Decision{Next: from, AckOnly: true}, nil
		}

	// ------------------------------------------------------- STOCK_RESERVED
	case StateStockReserved:
		switch t {
		case TriggerAuthorized: // SagaOnAuthorized — THE POINT OF NO RETURN
			return Decision{
				Next: StatePaid,
				Emit: []Step{StepCommit},
				// No deadline. Past this line the saga rolls forward only.
				Deadline: 0,
			}, nil

		case TriggerAuthFailed: // SagaOnAuthFailed
			if c.PaymentRetriable {
				// The PSP is down, not the card. Stay put, let the retry
				// policy resend AUTHORIZE, and keep the stock reserved —
				// cancelling here would lose a good order to a transient blip.
				return Decision{Next: from, AckOnly: true}, nil
			}
			return Decision{
				Next:       StateCompensating,
				Emit:       []Step{StepRelease},
				Reason:     ReasonPaymentDeclined,
				FailedStep: StepAuthorize,
				Deadline:   TimeoutCompensating,
			}, nil

		case TriggerTimeout: // SagaTimeoutReserved
			// Both compensations, because AUTHORIZE was sent and may have
			// succeeded without us hearing about it. VOID against an unknown
			// payment writes a tombstone that blocks a late authorization.
			return Decision{
				Next:       StateCompensating,
				Emit:       []Step{StepRelease, StepVoid},
				Reason:     ReasonPaymentTimeout,
				FailedStep: StepAuthorize,
				Deadline:   TimeoutCompensating,
			}, nil

		case TriggerReserved:
			// Duplicate delivery of the event that got us here.
			return Decision{Next: from, AckOnly: true}, nil
		}

	// ----------------------------------------------------------------- PAID
	case StatePaid:
		switch t {
		case TriggerCommitted: // SagaOnCommitted
			return Decision{
				Next: StateStockCommitted,
				Emit: []Step{StepCapture},
			}, nil

		case TriggerAuthorized:
			return Decision{Next: from, AckOnly: true}, nil

		case TriggerTimeout:
			// Deliberately NOT a compensation. TestRollbackAfterCommitIsUnsafe
			// reproduces what happens if you make it one: inventory commits,
			// the ack is delayed, we void the payment, and the stock is gone
			// with nothing charged for it.
			return Decision{
				Next:     from,
				Emit:     []Step{StepCommit}, // idempotent resend
				Deadline: RetryCommitInterval,
			}, nil

		case TriggerReserveFailed:
			// Cannot happen: we would not be PAID if the reservation failed.
			return Decision{}, ErrIllegalTransition{From: from, Trigger: t}
		}

	// ------------------------------------------------------ STOCK_COMMITTED
	case StateStockCommitted:
		switch t {
		case TriggerCaptured: // SagaOnCaptured
			return Decision{Next: StateConfirmed}, nil

		case TriggerCommitted:
			return Decision{Next: from, AckOnly: true}, nil

		case TriggerTimeout:
			// Same reasoning as PAID. The stock is already gone; the only
			// correct move is to keep trying to collect the money.
			return Decision{
				Next:     from,
				Emit:     []Step{StepCapture},
				Deadline: RetryCaptureInterval,
			}, nil

		case TriggerAuthFailed:
			// An authorization cannot fail after it succeeded and we committed
			// against it. If this fires, something upstream is badly wrong and
			// we want a loud error rather than a silent state change.
			return Decision{}, ErrIllegalTransition{From: from, Trigger: t}
		}

	// --------------------------------------------------------- COMPENSATING
	case StateCompensating:
		switch t {
		case TriggerReleased, TriggerReserveFailed:
			nc := c
			nc.InventorySettled = true
			return settleOrWait(nc)

		case TriggerVoided, TriggerAuthFailed:
			nc := c
			nc.PaymentSettled = true
			return settleOrWait(nc)

		case TriggerReserved:
			// The reservation we already asked to release. Ignore the ack for
			// it; the RELEASE command is in flight and inventory will honour
			// it against the now-existing reservation.
			return Decision{Next: from, AckOnly: true}, nil

		case TriggerAuthorized:
			// Payment succeeded after we gave up. The VOID is either already
			// sent or must be sent now, depending on which timeout got us here.
			if !c.AuthorizeSent {
				return Decision{Next: from, AckOnly: true}, nil
			}
			return Decision{
				Next:     from,
				Emit:     []Step{StepVoid},
				Deadline: TimeoutCompensating,
			}, nil

		case TriggerCommitted:
			// Inventory committed while we were compensating. Only reachable
			// if someone added a rollback past the point of no return — the
			// exact bug FINDINGS §1 documents. Refuse loudly.
			return Decision{}, ErrIllegalTransition{From: from, Trigger: t}

		case TriggerTimeout:
			// Compensation itself is stuck. Resend both; the participants are
			// idempotent and a duplicate release is a no-op.
			return Decision{
				Next:     from,
				Emit:     compensationsFor(c),
				Deadline: TimeoutCompensating,
			}, nil
		}

	// --------------------------------------------------- terminal states
	case StateConfirmed, StateCancelled:
		// Terminal is stable — TerminalIsStable in the model. Every late
		// event is acknowledged and dropped.
		return Decision{Next: from, AckOnly: true}, nil
	}

	return Decision{}, ErrIllegalTransition{From: from, Trigger: t}
}

// settleOrWait cancels once both participants have acknowledged, and waits
// otherwise. Corresponds to SagaCancel.
func settleOrWait(c Ctx) (Decision, error) {
	// Payment counts as settled when it was never engaged at all.
	paymentDone := c.PaymentSettled || !c.AuthorizeSent

	if c.InventorySettled && paymentDone {
		return Decision{
			Next:   StateCancelled,
			Reason: ReasonPaymentDeclined, // overwritten by the caller if already set
		}, nil
	}
	return Decision{Next: StateCompensating, AckOnly: true}, nil
}

// compensationsFor returns the compensation commands still outstanding.
func compensationsFor(c Ctx) []Step {
	var steps []Step
	if !c.InventorySettled {
		steps = append(steps, StepRelease)
	}
	if c.AuthorizeSent && !c.PaymentSettled {
		steps = append(steps, StepVoid)
	}
	return steps
}

// IsTerminal reports whether the saga has finished.
func IsTerminal(s State) bool {
	return s == StateConfirmed || s == StateCancelled
}

// RollbackForbidden reports whether the saga is past the point of no return.
// The admin UI uses it to hide the cancel button; the sweeper uses it to pick
// retry-forward instead of compensate.
func RollbackForbidden(s State) bool {
	return s == StatePaid || s == StateStockCommitted || s == StateConfirmed
}

// TriggerFor maps a CloudEvents type onto a saga trigger. Unknown types
// return false so the consumer can ack and skip rather than crash-loop on a
// message from a newer producer.
func TriggerFor(eventType string) (Trigger, bool) {
	switch eventType {
	case "souq.inventory.reserved.v1":
		return TriggerReserved, true
	case "souq.inventory.reservation_failed.v1":
		return TriggerReserveFailed, true
	case "souq.inventory.released.v1":
		return TriggerReleased, true
	case "souq.inventory.committed.v1":
		return TriggerCommitted, true
	case "souq.payment.authorized.v1":
		return TriggerAuthorized, true
	case "souq.payment.failed.v1":
		return TriggerAuthFailed, true
	case "souq.payment.captured.v1":
		return TriggerCaptured, true
	case "souq.payment.voided.v1":
		return TriggerVoided, true
	}
	return "", false
}

// CommandFor maps a step onto the CloudEvents type the participant expects.
func CommandFor(s Step) string {
	switch s {
	case StepReserve:
		return "souq.inventory.reserve.v1"
	case StepRelease:
		return "souq.inventory.release.v1"
	case StepCommit:
		return "souq.inventory.commit.v1"
	case StepAuthorize:
		return "souq.payment.authorize.v1"
	case StepCapture:
		return "souq.payment.capture.v1"
	case StepVoid:
		return "souq.payment.void.v1"
	}
	return ""
}
