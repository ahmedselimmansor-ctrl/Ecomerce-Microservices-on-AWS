package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souq/order-service/internal/domain"
	"github.com/souq/order-service/internal/saga"
)

// InsertOrder persists a new order and its lines. Must be called inside the
// same transaction as the Reserve command's outbox row, so that an order can
// never exist without the saga that drives it.
func InsertOrder(ctx context.Context, tx pgx.Tx, o *domain.Order) error {
	ship, err := json.Marshal(o.ShippingAddress)
	if err != nil {
		return fmt.Errorf("marshal shipping address: %w", err)
	}
	var bill []byte
	if o.BillingAddress != nil {
		if bill, err = json.Marshal(o.BillingAddress); err != nil {
			return fmt.Errorf("marshal billing address: %w", err)
		}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO orders (
			id, user_id, status, currency, subtotal, discount_total,
			shipping_total, tax_total, total, shipping_address, billing_address,
			payment_method_token, rules_version, correlation_id, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		o.ID, o.UserID, string(o.Status), o.Total.Currency,
		o.Subtotal.Amount, o.DiscountTotal.Amount, o.ShippingTotal.Amount,
		o.TaxTotal.Amount, o.Total.Amount, ship, bill,
		o.PaymentMethodToken, o.RulesVersion, o.CorrelationID, o.IdempotencyKey)
	if err != nil {
		if IsUniqueViolation(err) {
			return ErrDuplicateKey
		}
		return fmt.Errorf("insert order: %w", err)
	}

	batch := &pgx.Batch{}
	for i, it := range o.Items {
		batch.Queue(`
			INSERT INTO order_items (order_id, line_no, sku, product_id, title,
			                         image_url, quantity, unit_price, line_total)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			o.ID, i, it.SKU, it.ProductID, it.Title, nullIfEmpty(it.ImageURL),
			it.Quantity, it.UnitPrice.Amount, it.LineTotal.Amount)
	}
	br := tx.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		return fmt.Errorf("insert order items: %w", err)
	}
	return nil
}

// GetOrder loads an order with its lines. Used by the HTTP layer.
func (s *Store) GetOrder(ctx context.Context, id string) (*domain.Order, error) {
	return getOrder(ctx, s.pool.QueryRow, s.pool.Query, id)
}

// GetOrderTx is the same read inside a caller's transaction.
func GetOrderTx(ctx context.Context, tx pgx.Tx, id string) (*domain.Order, error) {
	return getOrder(ctx, tx.QueryRow, tx.Query, id)
}

type rowQuerier func(context.Context, string, ...any) pgx.Row
type rowsQuerier func(context.Context, string, ...any) (pgx.Rows, error)

func getOrder(ctx context.Context, qr rowQuerier, qs rowsQuerier, id string) (*domain.Order, error) {
	var (
		o                  domain.Order
		status             string
		currency           string
		ship, bill         []byte
		paymentID          *string
		reservationID      *string
		cancellationReason *string
		failedStep         *string
		tracking           *string
	)

	err := qr(ctx, `
		SELECT id, user_id, status, currency, subtotal, discount_total,
		       shipping_total, tax_total, total, shipping_address, billing_address,
		       payment_id, reservation_id, payment_method_token,
		       cancellation_reason, failed_step, tracking_number,
		       rules_version, correlation_id, idempotency_key,
		       placed_at, updated_at, version
		  FROM orders WHERE id = $1`, id).Scan(
		&o.ID, &o.UserID, &status, &currency, &o.Subtotal.Amount, &o.DiscountTotal.Amount,
		&o.ShippingTotal.Amount, &o.TaxTotal.Amount, &o.Total.Amount, &ship, &bill,
		&paymentID, &reservationID, &o.PaymentMethodToken,
		&cancellationReason, &failedStep, &tracking,
		&o.RulesVersion, &o.CorrelationID, &o.IdempotencyKey,
		&o.PlacedAt, &o.UpdatedAt, &o.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("select order %s: %w", id, err)
	}

	o.Status = saga.State(status)
	for _, m := range []*domain.Money{&o.Subtotal, &o.DiscountTotal, &o.ShippingTotal, &o.TaxTotal, &o.Total} {
		m.Currency = currency
	}
	if err := json.Unmarshal(ship, &o.ShippingAddress); err != nil {
		return nil, fmt.Errorf("unmarshal shipping address for %s: %w", id, err)
	}
	if len(bill) > 0 {
		var b domain.Address
		if err := json.Unmarshal(bill, &b); err != nil {
			return nil, fmt.Errorf("unmarshal billing address for %s: %w", id, err)
		}
		o.BillingAddress = &b
	}
	o.PaymentID = deref(paymentID)
	o.ReservationID = deref(reservationID)
	o.CancellationReason = saga.CancelReason(deref(cancellationReason))
	o.FailedStep = saga.Step(deref(failedStep))
	o.TrackingNumber = deref(tracking)

	rows, err := qs(ctx, `
		SELECT line_no, sku, product_id, title, image_url, quantity, unit_price, line_total
		  FROM order_items WHERE order_id = $1 ORDER BY line_no`, id)
	if err != nil {
		return nil, fmt.Errorf("select order items for %s: %w", id, err)
	}
	defer rows.Close()

	for rows.Next() {
		var it domain.OrderItem
		var img *string
		if err := rows.Scan(&it.LineNo, &it.SKU, &it.ProductID, &it.Title, &img,
			&it.Quantity, &it.UnitPrice.Amount, &it.LineTotal.Amount); err != nil {
			return nil, fmt.Errorf("scan order item: %w", err)
		}
		it.ImageURL = deref(img)
		it.UnitPrice.Currency = currency
		it.LineTotal.Currency = currency
		o.Items = append(o.Items, it)
	}
	return &o, rows.Err()
}

// ListOrders returns a user's orders, newest first, cursor-paginated.
// The cursor is the last seen (placed_at, id) pair, which is stable under
// concurrent inserts in a way an OFFSET never is.
func (s *Store) ListOrders(ctx context.Context, userID string, limit int, cursorTime time.Time, cursorID string) ([]*domain.Order, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM orders
		 WHERE user_id = $1
		   AND ($2::timestamptz IS NULL OR (placed_at, id) < ($2, $3))
		 ORDER BY placed_at DESC, id DESC
		 LIMIT $4`,
		userID, nullTime(cursorTime), cursorID, limit)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]*domain.Order, 0, len(ids))
	for _, id := range ids {
		o, err := s.GetOrder(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// UpdateStatus advances the saga state with optimistic locking.
//
// The version check is what stops two concurrent handlers — a Kafka redelivery
// racing the timeout sweeper — from both applying a transition. The loser gets
// ErrVersionStale, re-reads, and finds the trigger is now a no-op.
func UpdateStatus(ctx context.Context, tx pgx.Tx, orderID string, expectVersion int, next saga.State, fields StatusFields) error {
	tag, err := tx.Exec(ctx, `
		UPDATE orders
		   SET status = $3,
		       payment_id = COALESCE($4, payment_id),
		       reservation_id = COALESCE($5, reservation_id),
		       cancellation_reason = COALESCE($6, cancellation_reason),
		       failed_step = COALESCE($7, failed_step),
		       updated_at = now(),
		       version = version + 1
		 WHERE id = $1 AND version = $2`,
		orderID, expectVersion, string(next),
		nullIfEmpty(fields.PaymentID),
		nullIfEmpty(fields.ReservationID),
		nullIfEmpty(string(fields.CancellationReason)),
		nullIfEmpty(string(fields.FailedStep)))
	if err != nil {
		if IsCheckViolation(err) {
			// A CHECK rejected the row: the code tried to write a state the
			// schema forbids. That is a bug in the state machine, not bad
			// input, and it must surface as one.
			return fmt.Errorf("illegal order state %q rejected by the database: %w", next, err)
		}
		return fmt.Errorf("update order status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVersionStale
	}
	return nil
}

type StatusFields struct {
	PaymentID          string
	ReservationID      string
	CancellationReason saga.CancelReason
	FailedStep         saga.Step
}

// ---------------------------------------------------------------------------
// Saga steps

type StepRow struct {
	OrderID     string
	Step        saga.Step
	State       string
	Attempts    int
	LastEventID string
	Error       string
	SentAt      *time.Time
	AckedAt     *time.Time
	DeadlineAt  *time.Time
}

// RecordStepSent upserts a step as SENT and sets its deadline. A nil deadline
// means the step never times out, which is only correct past the point of no
// return (docs/DESIGN-INVARIANTS.md §1).
func RecordStepSent(ctx context.Context, tx pgx.Tx, orderID string, step saga.Step, deadline *time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO saga_steps (order_id, step, state, attempts, sent_at, deadline_at)
		VALUES ($1, $2, 'SENT', 1, now(), $3)
		ON CONFLICT (order_id, step) DO UPDATE
		   SET state = 'SENT',
		       attempts = saga_steps.attempts + 1,
		       sent_at = now(),
		       deadline_at = EXCLUDED.deadline_at,
		       error = NULL`,
		orderID, string(step), deadline)
	return err
}

// RecordStepAcked closes a step out. Clearing deadline_at removes it from the
// sweeper's partial index, which is why that index stays small.
func RecordStepAcked(ctx context.Context, tx pgx.Tx, orderID string, step saga.Step, eventID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO saga_steps (order_id, step, state, attempts, acked_at, last_event_id)
		VALUES ($1, $2, 'ACKED', 1, now(), $3)
		ON CONFLICT (order_id, step) DO UPDATE
		   SET state = 'ACKED',
		       acked_at = now(),
		       last_event_id = EXCLUDED.last_event_id,
		       deadline_at = NULL`,
		orderID, string(step), nullIfEmpty(eventID))
	return err
}

// StepsFor returns the saga trace for the admin inspector.
func (s *Store) StepsFor(ctx context.Context, orderID string) ([]StepRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT order_id, step, state, attempts, last_event_id, error,
		       sent_at, acked_at, deadline_at
		  FROM saga_steps WHERE order_id = $1
		 ORDER BY COALESCE(sent_at, now())`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StepRow
	for rows.Next() {
		var r StepRow
		var step, lastEvent, errMsg *string
		if err := rows.Scan(&r.OrderID, &step, &r.State, &r.Attempts, &lastEvent,
			&errMsg, &r.SentAt, &r.AckedAt, &r.DeadlineAt); err != nil {
			return nil, err
		}
		r.Step = saga.Step(deref(step))
		r.LastEventID = deref(lastEvent)
		r.Error = deref(errMsg)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OverdueOrder is one row the sweeper must act on.
type OverdueOrder struct {
	OrderID  string
	Status   saga.State
	Step     saga.Step
	Version  int
	Attempts int
}

// ClaimOverdue finds sagas whose current step has blown its deadline and locks
// them for this sweeper instance. SKIP LOCKED means several replicas can sweep
// concurrently without stepping on each other.
func ClaimOverdue(ctx context.Context, tx pgx.Tx, limit int) ([]OverdueOrder, error) {
	rows, err := tx.Query(ctx, `
		SELECT o.id, o.status, s.step, o.version, s.attempts
		  FROM saga_steps s
		  JOIN orders o ON o.id = s.order_id
		 WHERE s.state = 'SENT'
		   AND s.deadline_at IS NOT NULL
		   AND s.deadline_at < now()
		   AND o.status NOT IN ('CONFIRMED','CANCELLED','SHIPPED','DELIVERED','REFUNDED')
		 ORDER BY s.deadline_at
		 LIMIT $1
		   FOR UPDATE OF o SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim overdue sagas: %w", err)
	}
	defer rows.Close()

	var out []OverdueOrder
	for rows.Next() {
		var r OverdueOrder
		var status, step string
		if err := rows.Scan(&r.OrderID, &status, &step, &r.Version, &r.Attempts); err != nil {
			return nil, err
		}
		r.Status = saga.State(status)
		r.Step = saga.Step(step)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountStuck is the number that goes on the dashboard and into the alert. It
// counts sagas that are overdue by a wide margin — a normal in-flight retry is
// not "stuck", a saga that has been retrying for five minutes is.
func (s *Store) CountStuck(ctx context.Context, grace time.Duration) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(DISTINCT o.id)
		  FROM saga_steps s
		  JOIN orders o ON o.id = s.order_id
		 WHERE s.state = 'SENT'
		   AND s.deadline_at IS NOT NULL
		   AND s.deadline_at < now() - $1::interval
		   AND o.status NOT IN ('CONFIRMED','CANCELLED','SHIPPED','DELIVERED','REFUNDED')`,
		fmt.Sprintf("%d seconds", int(grace.Seconds()))).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
