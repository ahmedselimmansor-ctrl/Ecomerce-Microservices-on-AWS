package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souq/order-service/internal/eventbus"
	"github.com/souq/order-service/internal/platform"
	"github.com/souq/order-service/internal/saga"
	"github.com/souq/order-service/internal/store"
)

// Sweeper fires the saga's timeouts.
//
// A timeout in this system means "we have waited long enough and are going to
// act as if the reply is never coming". Crucially, the reply may still arrive
// afterwards — internal/saga/model_test.go models exactly that race, and every
// participant handles it via tombstones (FINDINGS §2). So the sweeper is
// allowed to be wrong about whether a message is lost; it is not allowed to be
// wrong about which states may compensate.
type Sweeper struct {
	orch     *Orchestrator
	store    *store.Store
	interval time.Duration
	batch    int
}

func NewSweeper(o *Orchestrator, s *store.Store, interval time.Duration) *Sweeper {
	return &Sweeper{orch: o, store: s, interval: interval, batch: 50}
}

func (s *Sweeper) Run(ctx context.Context) {
	slog.InfoContext(ctx, "saga sweeper started", slog.Duration("interval", s.interval))

	// Stagger replicas so they do not all claim at the same instant. They use
	// SKIP LOCKED and would be correct anyway, but staggering avoids a burst
	// of contention every tick.
	select {
	case <-time.After(time.Duration(rand.Int63n(int64(s.interval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "saga sweeper stopping")
			return
		case <-ticker.C:
			if err := s.tick(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "sweeper tick failed", slog.String("error", err.Error()))
			}
			if n, err := s.store.CountStuck(ctx, 5*time.Minute); err == nil {
				platform.SagaStuck.Set(float64(n))
			}
		}
	}
}

func (s *Sweeper) tick(ctx context.Context) error {
	return s.store.InTx(ctx, func(tx pgx.Tx) error {
		overdue, err := store.ClaimOverdue(ctx, tx, s.batch)
		if err != nil {
			return err
		}

		for _, o := range overdue {
			if err := s.fire(ctx, tx, o); err != nil {
				// One wedged order must not stop the sweep for the rest.
				slog.ErrorContext(ctx, "could not sweep order",
					slog.String("orderId", o.OrderID),
					slog.String("status", string(o.Status)),
					slog.String("error", err.Error()))
			}
		}
		return nil
	})
}

func (s *Sweeper) fire(ctx context.Context, tx pgx.Tx, ov store.OverdueOrder) error {
	// Roll-forward states retry rather than compensate. If they have been
	// retrying too long, escalate — a human has to look at it, because the
	// alternative (rolling back) would deduct stock with no payment.
	if saga.RollbackForbidden(ov.Status) && ov.Attempts >= saga.MaxRollForwardRetries {
		slog.ErrorContext(ctx, "SAGA STUCK PAST POINT OF NO RETURN — manual intervention required",
			slog.String("orderId", ov.OrderID),
			slog.String("status", string(ov.Status)),
			slog.String("step", string(ov.Step)),
			slog.Int("attempts", ov.Attempts),
			slog.String("runbook", "docs/runbooks/stuck-saga.md"))
		// Push the deadline out so we stop hot-looping while a human works,
		// but keep the row visible to the stuck-orders alert.
		_, err := tx.Exec(ctx, `
			UPDATE saga_steps SET deadline_at = now() + interval '5 minutes'
			 WHERE order_id = $1 AND step = $2`, ov.OrderID, string(ov.Step))
		return err
	}

	ord, err := store.GetOrderTx(ctx, tx, ov.OrderID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}

	sctx, err := s.orch.buildCtx(ctx, tx, ord, false)
	if err != nil {
		return err
	}

	decision, err := saga.Next(ord.Status, saga.TriggerTimeout, sctx)
	if err != nil {
		var illegal saga.ErrIllegalTransition
		if errors.As(err, &illegal) {
			platform.SagaIllegalTransitions.
				WithLabelValues(string(ord.Status), string(saga.TriggerTimeout)).Inc()
		}
		return err
	}

	// Belt and braces on top of the state machine: even if a future edit to
	// machine.go got this wrong, the sweeper refuses to be the thing that
	// compensates past the point of no return.
	if saga.RollbackForbidden(ord.Status) {
		for _, step := range decision.Emit {
			if step == saga.StepRelease || step == saga.StepVoid {
				platform.SagaIllegalTransitions.
					WithLabelValues(string(ord.Status), "sweeper.compensate").Inc()
				slog.ErrorContext(ctx, "refusing to compensate past the point of no return",
					slog.String("orderId", ord.ID),
					slog.String("status", string(ord.Status)),
					slog.String("step", string(step)),
					slog.String("reference", "docs/DESIGN-INVARIANTS.md §1"))
				return nil
			}
		}
	}

	slog.InfoContext(ctx, "saga timeout fired",
		slog.String("orderId", ord.ID),
		slog.String("status", string(ord.Status)),
		slog.String("waitingOn", string(ov.Step)),
		slog.Int("attempts", ov.Attempts),
		slog.String("action", string(decision.Next)))

	return s.orch.apply(ctx, tx, ord, saga.TriggerTimeout, decision,
		timeoutEnvelope(ord.ID), ord.ReservationID, ord.PaymentID, "")
}

// timeoutEnvelope stands in for the inbound event that apply() normally works
// from. A timeout has no event id because nothing arrived — that is the whole
// point of it — so the synthetic envelope carries only the subject. It is
// never written to the inbox and never published.
func timeoutEnvelope(orderID string) eventbus.Envelope {
	return eventbus.Envelope{
		SpecVersion: "1.0",
		Source:      source,
		Type:        "souq.saga.timeout.internal",
		Subject:     orderID,
		Time:        time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}
