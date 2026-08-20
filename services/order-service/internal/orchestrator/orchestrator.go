// Package orchestrator drives the order saga: it turns inbound participant
// events into state transitions and outbound commands, all inside one database
// transaction per event.
//
// The pure decision logic lives in internal/saga. This package is the
// plumbing: read state, ask the machine what to do, persist the answer and the
// resulting commands atomically. Keeping the two apart is what makes the
// machine exhaustively testable without a database.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souq/order-service/internal/domain"
	"github.com/souq/order-service/internal/eventbus"
	"github.com/souq/order-service/internal/platform"
	"github.com/souq/order-service/internal/saga"
	"github.com/souq/order-service/internal/store"
)

const (
	source        = "souq/order-service"
	consumerGroup = "order-service.saga-events"
)

type Orchestrator struct {
	store *store.Store
}

func New(s *store.Store) *Orchestrator {
	return &Orchestrator{store: s}
}

// ---------------------------------------------------------------------------
// Inbound: participant events

// Handle applies one event from inventory-service or payment-service.
//
// Everything happens in a single transaction: the inbox claim, the state
// transition, the step bookkeeping, and the outbox rows for whatever commands
// the machine decided to emit. Either all of it lands or none of it does,
// which is what makes the saga recoverable from a crash at any instruction.
func (o *Orchestrator) Handle(ctx context.Context, e eventbus.Envelope) error {
	trigger, known := saga.TriggerFor(e.Type)
	if !known {
		// A type we do not model. Ack and move on rather than crash-looping:
		// a newer producer rolling out ahead of us must not stop the saga.
		slog.WarnContext(ctx, "ignoring unmodelled event type",
			slog.String("eventType", e.Type), slog.String("eventId", e.ID))
		return nil
	}

	var body struct {
		OrderID       string `json:"orderId"`
		ReservationID string `json:"reservationId"`
		PaymentID     string `json:"paymentId"`
		ReasonCode    string `json:"reasonCode"`
		Retriable     bool   `json:"retriable"`
	}
	if err := e.Bind(&body); err != nil {
		return fmt.Errorf("%w: %v", eventbus.ErrPermanent, err)
	}
	if body.OrderID == "" {
		return fmt.Errorf("%w: event %s carries no orderId", eventbus.ErrPermanent, e.ID)
	}

	return o.store.InTx(ctx, func(tx pgx.Tx) error {
		// Inbox first. If this event has already been applied, the whole
		// transaction becomes a no-op and we ack the offset.
		// TestWithoutTheInboxTheSideEffectRunsTwice is the counterexample for skipping it.
		fresh, err := store.MarkProcessed(ctx, tx, e.ID, consumerGroup)
		if err != nil {
			return err
		}
		if !fresh {
			platform.EventsConsumed.WithLabelValues("saga", "duplicate").Inc()
			return nil
		}

		ord, err := store.GetOrderTx(ctx, tx, body.OrderID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// An event for an order we have never heard of. Not retriable:
				// waiting will not conjure the order into existence.
				return fmt.Errorf("%w: unknown order %s", eventbus.ErrPermanent, body.OrderID)
			}
			return err
		}

		sctx, err := o.buildCtx(ctx, tx, ord, body.Retriable)
		if err != nil {
			return err
		}

		decision, err := saga.Next(ord.Status, trigger, sctx)
		if err != nil {
			var illegal saga.ErrIllegalTransition
			if errors.As(err, &illegal) {
				// This should be impossible. If it happens, the design and the
				// implementation have diverged — page, do not retry.
				platform.SagaIllegalTransitions.
					WithLabelValues(string(ord.Status), string(trigger)).Inc()
				slog.ErrorContext(ctx, "ILLEGAL SAGA TRANSITION — see internal/saga/model_test.go",
					slog.String("orderId", ord.ID),
					slog.String("from", string(ord.Status)),
					slog.String("trigger", string(trigger)),
					slog.String("eventId", e.ID))
				return fmt.Errorf("%w: %v", eventbus.ErrPermanent, err)
			}
			return err
		}

		return o.apply(ctx, tx, ord, trigger, decision, e, body.ReservationID, body.PaymentID, body.ReasonCode)
	})
}

// buildCtx assembles the facts the state machine needs beyond the current
// state. Reading them from saga_steps rather than tracking them in memory is
// what lets any replica pick up any order after a restart or a rebalance.
func (o *Orchestrator) buildCtx(ctx context.Context, tx pgx.Tx, ord *domain.Order, retriable bool) (saga.Ctx, error) {
	rows, err := tx.Query(ctx,
		`SELECT step, state, attempts FROM saga_steps WHERE order_id = $1`, ord.ID)
	if err != nil {
		return saga.Ctx{}, err
	}
	defer rows.Close()

	c := saga.Ctx{PaymentRetriable: retriable}
	for rows.Next() {
		var step, state string
		var attempts int
		if err := rows.Scan(&step, &state, &attempts); err != nil {
			return saga.Ctx{}, err
		}
		switch saga.Step(step) {
		case saga.StepAuthorize:
			c.AuthorizeSent = true
		case saga.StepRelease:
			if state == "ACKED" {
				c.InventorySettled = true
			}
		case saga.StepVoid:
			if state == "ACKED" {
				c.PaymentSettled = true
			}
		case saga.StepCommit, saga.StepCapture:
			if attempts > c.RollForwardAttempts {
				c.RollForwardAttempts = attempts
			}
		}
	}
	return c, rows.Err()
}

// apply persists a decision. Runs inside the caller's transaction.
func (o *Orchestrator) apply(
	ctx context.Context, tx pgx.Tx,
	ord *domain.Order, trigger saga.Trigger, d saga.Decision,
	e eventbus.Envelope,
	reservationID, paymentID, reasonCode string,
) error {
	// Close out the step this event acknowledges.
	if step, ok := stepAckedBy(trigger); ok {
		if err := store.RecordStepAcked(ctx, tx, ord.ID, step, e.ID); err != nil {
			return err
		}
	}

	if d.AckOnly && d.Next == ord.Status && len(d.Emit) == 0 {
		return nil
	}

	if d.Next != ord.Status {
		fields := store.StatusFields{
			PaymentID:     paymentID,
			ReservationID: reservationID,
			FailedStep:    d.FailedStep,
		}
		if d.Next == saga.StateCancelled {
			fields.CancellationReason = pickReason(d.Reason, reasonCode, ord.CancellationReason)
		}

		if err := store.UpdateStatus(ctx, tx, ord.ID, ord.Version, d.Next, fields); err != nil {
			if errors.Is(err, store.ErrVersionStale) {
				// Another handler moved this order between our read and our
				// write. Return an error so the message is retried; the retry
				// re-reads and will usually find the trigger is now a no-op.
				return fmt.Errorf("order %s changed concurrently: %w", ord.ID, err)
			}
			return err
		}

		platform.SagaTransitions.
			WithLabelValues(string(ord.Status), string(trigger), string(d.Next)).Inc()

		if saga.IsTerminal(d.Next) {
			outcome := "confirmed"
			if d.Next == saga.StateCancelled {
				outcome = "cancelled"
			}
			platform.SagaDuration.WithLabelValues(outcome).
				Observe(time.Since(ord.PlacedAt).Seconds())
		}

		slog.InfoContext(ctx, "saga transition",
			slog.String("orderId", ord.ID),
			slog.String("from", string(ord.Status)),
			slog.String("trigger", string(trigger)),
			slog.String("to", string(d.Next)))
	}

	// Emit commands and the public events that follow from the new state.
	for _, step := range d.Emit {
		if err := o.emitCommand(ctx, tx, ord, step, d.Deadline); err != nil {
			return err
		}
	}
	return o.emitStateEvents(ctx, tx, ord, d, reservationID, paymentID)
}

// emitCommand writes a saga command to the outbox and records the step.
func (o *Orchestrator) emitCommand(ctx context.Context, tx pgx.Tx, ord *domain.Order, step saga.Step, deadline time.Duration) error {
	eventType := saga.CommandFor(step)
	if eventType == "" {
		return fmt.Errorf("no command mapped for step %s", step)
	}

	reservationID := ord.ReservationID
	if reservationID == "" {
		reservationID = domain.NewID("rsv")
	}
	paymentID := ord.PaymentID
	if paymentID == "" {
		paymentID = domain.NewID("pay")
	}

	var payload any
	switch step {
	case saga.StepReserve:
		items := make([]map[string]any, 0, len(ord.Items))
		for _, it := range ord.Items {
			items = append(items, map[string]any{"sku": it.SKU, "quantity": it.Quantity})
		}
		payload = map[string]any{
			"orderId": ord.ID, "reservationId": reservationID,
			"items": items, "ttlSeconds": 900,
		}
	case saga.StepRelease:
		payload = map[string]any{
			"orderId": ord.ID, "reservationId": reservationID,
			"reasonCode": string(pickReason(saga.ReasonPaymentTimeout, "", ord.CancellationReason)),
		}
	case saga.StepCommit:
		payload = map[string]any{"orderId": ord.ID, "reservationId": reservationID}
	case saga.StepAuthorize:
		payload = map[string]any{
			"orderId": ord.ID, "paymentId": paymentID, "userId": ord.UserID,
			"amount":             ord.Total,
			"paymentMethodToken": ord.PaymentMethodToken,
		}
	case saga.StepCapture:
		payload = map[string]any{
			"orderId": ord.ID, "paymentId": paymentID, "amount": ord.Total,
		}
	case saga.StepVoid:
		payload = map[string]any{
			"orderId": ord.ID, "paymentId": paymentID, "reasonCode": "SAGA_COMPENSATION",
		}
	default:
		return fmt.Errorf("unhandled step %s", step)
	}

	// Persist the ids we just minted so a retry reuses them rather than
	// creating a second reservation or a second payment.
	if _, err := tx.Exec(ctx, `
		UPDATE orders
		   SET reservation_id = COALESCE(reservation_id, $2),
		       payment_id = COALESCE(payment_id, $3)
		 WHERE id = $1`, ord.ID, reservationID, paymentID); err != nil {
		return err
	}

	env, err := eventbus.NewEnvelope(source, eventType, ord.ID,
		ord.CorrelationID, platform.RequestIDFrom(ctx), payload)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}

	if err := store.Enqueue(ctx, tx, store.OutboxRecord{
		AggregateType: "order",
		AggregateID:   ord.ID,
		EventID:       env.ID,
		EventType:     eventType,
		Topic:         eventbus.TopicOrderCommands,
		PartitionKey:  ord.ID,
		Payload:       raw,
		Headers:       env.Headers(),
	}); err != nil {
		return err
	}

	var deadlineAt *time.Time
	if deadline > 0 {
		t := time.Now().Add(deadline)
		deadlineAt = &t
	}
	return store.RecordStepSent(ctx, tx, ord.ID, step, deadlineAt)
}

// emitStateEvents publishes the public order event for a terminal transition.
// Notification, analytics and the storefront read model all key off these.
func (o *Orchestrator) emitStateEvents(ctx context.Context, tx pgx.Tx, ord *domain.Order, d saga.Decision, reservationID, paymentID string) error {
	var eventType string
	var payload any

	switch d.Next {
	case saga.StateConfirmed:
		eventType = "souq.order.confirmed.v1"
		payload = map[string]any{
			"orderId": ord.ID, "userId": ord.UserID,
			"paymentId":     firstNonEmpty(paymentID, ord.PaymentID),
			"reservationId": firstNonEmpty(reservationID, ord.ReservationID),
			"total":         ord.Total,
			"confirmedAt":   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		}
	case saga.StateCancelled:
		eventType = "souq.order.cancelled.v1"
		payload = map[string]any{
			"orderId": ord.ID, "userId": ord.UserID,
			"reasonCode": string(pickReason(d.Reason, "", ord.CancellationReason)),
			"failedStep": string(d.FailedStep),
		}
	default:
		return nil
	}

	env, err := eventbus.NewEnvelope(source, eventType, ord.ID,
		ord.CorrelationID, platform.RequestIDFrom(ctx), payload)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return store.Enqueue(ctx, tx, store.OutboxRecord{
		AggregateType: "order",
		AggregateID:   ord.ID,
		EventID:       env.ID,
		EventType:     eventType,
		Topic:         eventbus.TopicOrderEvents,
		PartitionKey:  ord.ID,
		Payload:       raw,
		Headers:       env.Headers(),
	})
}

// stepAckedBy maps an inbound event onto the step it closes out.
func stepAckedBy(t saga.Trigger) (saga.Step, bool) {
	switch t {
	case saga.TriggerReserved, saga.TriggerReserveFailed:
		return saga.StepReserve, true
	case saga.TriggerAuthorized, saga.TriggerAuthFailed:
		return saga.StepAuthorize, true
	case saga.TriggerCommitted:
		return saga.StepCommit, true
	case saga.TriggerCaptured:
		return saga.StepCapture, true
	case saga.TriggerReleased:
		return saga.StepRelease, true
	case saga.TriggerVoided:
		return saga.StepVoid, true
	}
	return "", false
}

// pickReason keeps the first concrete reason recorded. The decision's reason
// is a default; a participant that told us exactly why wins, and an existing
// recorded reason wins over both — the first failure is the interesting one.
func pickReason(fromDecision saga.CancelReason, fromEvent string, existing saga.CancelReason) saga.CancelReason {
	if existing != "" {
		return existing
	}
	if r := saga.CancelReason(fromEvent); isKnownReason(r) {
		return r
	}
	return fromDecision
}

func isKnownReason(r saga.CancelReason) bool {
	switch r {
	case saga.ReasonInsufficientStock, saga.ReasonPaymentDeclined,
		saga.ReasonPaymentTimeout, saga.ReasonReservationTimeout,
		saga.ReasonCustomerCancelled, saga.ReasonFraudRejected:
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
