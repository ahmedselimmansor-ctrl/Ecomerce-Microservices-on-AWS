package orchestrator

import (
	"context"
	"encoding/json"
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

// PlaceOrder accepts an order and starts its saga.
//
// One transaction does all of it: the order rows, the idempotency record, the
// first saga step, and the Reserve command in the outbox. There is no window
// in which an order exists without a saga to drive it, and none in which a
// Reserve command exists for an order that was rolled back.
//
// It returns 202-shaped data, not a finished order. Checkout is asynchronous
// and pretending otherwise would mean holding an HTTP connection open across
// three services and a card network — the first slow PSP would exhaust the
// connection pool and take the storefront down with it.
func (o *Orchestrator) PlaceOrder(ctx context.Context, in PlaceOrderInput) (*domain.Order, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	ord := &domain.Order{
		ID:                 domain.NewID("ord"),
		UserID:             in.UserID,
		Status:             saga.StatePending,
		Items:              in.Items,
		Subtotal:           in.Subtotal,
		DiscountTotal:      in.DiscountTotal,
		ShippingTotal:      in.ShippingTotal,
		TaxTotal:           in.TaxTotal,
		ShippingAddress:    in.ShippingAddress,
		BillingAddress:     in.BillingAddress,
		PaymentMethodToken: in.PaymentMethodToken,
		RulesVersion:       in.RulesVersion,
		CorrelationID:      platform.CorrelationIDFrom(ctx),
		IdempotencyKey:     in.IdempotencyKey,
		PlacedAt:           time.Now().UTC(),
	}

	// Derive the total from the lines rather than trusting the caller.
	if err := ord.RecomputeTotal(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOrder, err)
	}

	// And then check it against what the customer was actually shown. A
	// mismatch means the cart moved under them — charging the new number
	// without asking is how you end up in a chargeback dispute you lose.
	if !ord.Total.Equal(in.ExpectedTotal) {
		return nil, fmt.Errorf("%w: cart total is %s but the client expected %s",
			ErrTotalMismatch, ord.Total, in.ExpectedTotal)
	}

	err := o.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := store.InsertOrder(ctx, tx, ord); err != nil {
			return err
		}
		// Start() rather than an inline literal, so the initial transition
		// comes from the same state machine as every other one.
		d := saga.Start()
		for _, step := range d.Emit {
			if err := o.emitCommand(ctx, tx, ord, step, d.Deadline); err != nil {
				return err
			}
		}
		return o.emitOrderCreated(ctx, tx, ord)
	})
	if err != nil {
		return nil, err
	}

	platform.SagaTransitions.WithLabelValues("-", "place", string(saga.StatePending)).Inc()
	slog.InfoContext(ctx, "order placed",
		slog.String("orderId", ord.ID),
		slog.String("userId", ord.UserID),
		slog.Int("lines", len(ord.Items)),
		slog.Int64("total", ord.Total.Amount),
		slog.String("currency", ord.Total.Currency),
		slog.String("rulesVersion", ord.RulesVersion))

	return ord, nil
}

func (o *Orchestrator) emitOrderCreated(ctx context.Context, tx pgx.Tx, ord *domain.Order) error {
	items := make([]map[string]any, 0, len(ord.Items))
	for _, it := range ord.Items {
		items = append(items, map[string]any{
			"sku": it.SKU, "productId": it.ProductID, "title": it.Title,
			"quantity": it.Quantity, "unitPrice": it.UnitPrice, "lineTotal": it.LineTotal,
		})
	}

	env, err := eventbus.NewEnvelope(source, "souq.order.created.v1", ord.ID,
		ord.CorrelationID, platform.RequestIDFrom(ctx), map[string]any{
			"orderId": ord.ID, "userId": ord.UserID, "items": items,
			"subtotal": ord.Subtotal, "discountTotal": ord.DiscountTotal,
			"shippingTotal": ord.ShippingTotal, "taxTotal": ord.TaxTotal,
			"total": ord.Total, "shippingAddress": ord.ShippingAddress,
			"rulesVersion": ord.RulesVersion, "idempotencyKey": ord.IdempotencyKey,
		})
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
		EventType:     env.Type,
		Topic:         eventbus.TopicOrderEvents,
		PartitionKey:  ord.ID,
		Payload:       raw,
		Headers:       env.Headers(),
	})
}

// CancelOrder handles a customer-initiated cancellation.
//
// Only legal before the point of no return. Past it the money is committed
// against stock that is already allocated, and the correct action is a refund
// through support, not a saga rollback — see docs/DESIGN-INVARIANTS.md §1.
func (o *Orchestrator) CancelOrder(ctx context.Context, orderID, userID string) error {
	return o.store.InTx(ctx, func(tx pgx.Tx) error {
		ord, err := store.GetOrderTx(ctx, tx, orderID)
		if err != nil {
			return err
		}
		if ord.UserID != userID {
			// Same response as a missing order: confirming that an order
			// exists but belongs to someone else is an enumeration oracle.
			return store.ErrNotFound
		}
		if !ord.Cancellable() {
			return ErrNotCancellable
		}

		steps := []saga.Step{saga.StepRelease}
		sctx, err := o.buildCtx(ctx, tx, ord, false)
		if err != nil {
			return err
		}
		if sctx.AuthorizeSent {
			steps = append(steps, saga.StepVoid)
		}

		if err := store.UpdateStatus(ctx, tx, ord.ID, ord.Version, saga.StateCompensating,
			store.StatusFields{CancellationReason: saga.ReasonCustomerCancelled}); err != nil {
			return err
		}
		for _, s := range steps {
			if err := o.emitCommand(ctx, tx, ord, s, saga.TimeoutCompensating); err != nil {
				return err
			}
		}
		platform.SagaTransitions.
			WithLabelValues(string(ord.Status), "customer.cancel", string(saga.StateCompensating)).Inc()
		return nil
	})
}

// ---------------------------------------------------------------------------

type PlaceOrderInput struct {
	UserID             string
	Items              []domain.OrderItem
	Subtotal           domain.Money
	DiscountTotal      domain.Money
	ShippingTotal      domain.Money
	TaxTotal           domain.Money
	ExpectedTotal      domain.Money
	ShippingAddress    domain.Address
	BillingAddress     *domain.Address
	PaymentMethodToken string
	RulesVersion       string
	IdempotencyKey     string
}

func (in PlaceOrderInput) Validate() error {
	switch {
	case in.UserID == "":
		return fmt.Errorf("%w: userId is required", ErrInvalidOrder)
	case len(in.Items) == 0:
		return fmt.Errorf("%w: an order must have at least one line", ErrInvalidOrder)
	case len(in.Items) > 100:
		return fmt.Errorf("%w: an order may not exceed 100 lines", ErrInvalidOrder)
	case in.PaymentMethodToken == "":
		return fmt.Errorf("%w: paymentMethodToken is required", ErrInvalidOrder)
	case in.RulesVersion == "":
		// Without it the order cannot be re-priced deterministically later.
		return fmt.Errorf("%w: rulesVersion is required", ErrInvalidOrder)
	case in.IdempotencyKey == "":
		return fmt.Errorf("%w: Idempotency-Key is required on this endpoint", ErrInvalidOrder)
	}

	if err := in.ShippingAddress.Validate(); err != nil {
		return fmt.Errorf("%w: shippingAddress: %v", ErrInvalidOrder, err)
	}
	if in.BillingAddress != nil {
		if err := in.BillingAddress.Validate(); err != nil {
			return fmt.Errorf("%w: billingAddress: %v", ErrInvalidOrder, err)
		}
	}

	for i, it := range in.Items {
		switch {
		case !domain.ValidID("sku", it.SKU):
			return fmt.Errorf("%w: items[%d].sku is not a valid SKU id", ErrInvalidOrder, i)
		case !domain.ValidID("prd", it.ProductID):
			return fmt.Errorf("%w: items[%d].productId is not a valid product id", ErrInvalidOrder, i)
		case it.Quantity < 1 || it.Quantity > 999:
			return fmt.Errorf("%w: items[%d].quantity must be between 1 and 999", ErrInvalidOrder, i)
		case it.UnitPrice.Amount < 0:
			return fmt.Errorf("%w: items[%d].unitPrice cannot be negative", ErrInvalidOrder, i)
		}
	}
	return nil
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const (
	ErrInvalidOrder   = sentinel("invalid order")
	ErrTotalMismatch  = sentinel("order total does not match the total shown to the customer")
	ErrNotCancellable = sentinel("order can no longer be cancelled")
)
