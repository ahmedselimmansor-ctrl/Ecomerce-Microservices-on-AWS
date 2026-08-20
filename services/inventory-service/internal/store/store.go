// Package store is the only place in inventory-service that knows SQL exists.
//
// Every function that publishes an event takes a pgx.Tx rather than a pool.
// That is not a style preference — it makes "commit the stock change, then
// publish the event" unrepresentable, which is the dual-write failure the
// outbox model finds in two steps.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("store: not found")
	ErrDuplicateKey = errors.New("store: duplicate key")
)

type ReservationState string

const (
	StateReserved  ReservationState = "RESERVED"
	StateCommitted ReservationState = "COMMITTED"
	StateReleased  ReservationState = "RELEASED"
	StateFailed    ReservationState = "FAILED"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, url string, maxConns int32, statementTimeout time.Duration) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	// Jitter stops the whole pool recycling in lockstep and hammering the
	// database with a reconnect storm every 30 minutes.
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Set on the connection rather than per query, so no code path can pin a
	// connection with a pathological plan.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", statementTimeout.Milliseconds())
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "inventory-service"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s *Store) Pool() *pgxpool.Pool            { return s.pool }

// InTx runs fn in a transaction.
//
// ReadCommitted, not Serializable. Every invariant this service needs is
// enforced either by a unique constraint or by the conditional UPDATE below,
// which the database evaluates atomically — so the extra serialisation
// failures would buy nothing but retry churn.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() {
		if v := recover(); v != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(v)
		}
	}()

	if err := fn(tx); err != nil {
		// Roll back with a context that survives the caller's cancellation,
		// otherwise a client disconnect leaves the transaction open until the
		// idle_in_transaction timeout fires.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit(ctx)
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsCheckViolation reports a 23514. In this service that always means a code
// path tried to write a state the schema forbids — an oversell that got past
// the application. It is a bug, never bad input.
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// ---------------------------------------------------------------------------
// The statement everything depends on

// TryTake is the whole safety argument, in one statement.
//
// The predicate `on_hand - reserved >= $2` and the increment
// `reserved = reserved + $2` are evaluated by Postgres inside the same row
// latch. There is no window between the check and the write for another
// transaction to slip into — which is what makes this safe at READ COMMITTED
// with no explicit lock, no retry loop, and no serialising every buyer of a
// hot SKU behind one FOR UPDATE.
//
// `taken = false` is the "insufficient stock" signal. It is also returned for
// an unknown or non-ACTIVE SKU, so the caller distinguishes them with
// AvailableFor.
func TryTake(ctx context.Context, tx pgx.Tx, sku string, quantity int) (taken bool, onHand, reserved int, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE stock_levels
		   SET reserved = reserved + $2,
		       updated_at = now(),
		       version = version + 1
		 WHERE sku = $1
		   AND status = 'ACTIVE'
		   AND on_hand - reserved >= $2
		RETURNING on_hand, reserved`,
		sku, quantity).Scan(&onHand, &reserved)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, 0, nil
	}
	if err != nil {
		if IsCheckViolation(err) {
			// The no_oversell CHECK rejected it. The conditional above should
			// have made that impossible, so this means the constraint and the
			// predicate disagree — a real bug, surfaced loudly.
			return false, 0, 0, fmt.Errorf("OVERSELL PREVENTED BY CONSTRAINT for %s: %w", sku, err)
		}
		return false, 0, 0, fmt.Errorf("take %d of %s: %w", quantity, sku, err)
	}
	return true, onHand, reserved, nil
}

// CommitLine deducts from BOTH on_hand and reserved.
func CommitLine(ctx context.Context, tx pgx.Tx, sku string, quantity int) (onHand, reserved int, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE stock_levels
		   SET on_hand = on_hand - $2,
		       reserved = reserved - $2,
		       updated_at = now(),
		       version = version + 1
		 WHERE sku = $1
		RETURNING on_hand, reserved`,
		sku, quantity).Scan(&onHand, &reserved)
	if err != nil {
		return 0, 0, fmt.Errorf("commit %d of %s: %w", quantity, sku, err)
	}
	return onHand, reserved, nil
}

// ReturnLine gives held units back, clamped at zero.
//
// GREATEST(...) rather than a bare subtraction: if a reconciliation job has
// already adjusted `reserved` down, subtracting again would underflow and trip
// the CHECK, turning a bookkeeping discrepancy into a failed release that
// holds the stock hostage.
func ReturnLine(ctx context.Context, tx pgx.Tx, sku string, quantity int) (onHand, reserved int, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE stock_levels
		   SET reserved = GREATEST(reserved - $2, 0),
		       updated_at = now(),
		       version = version + 1
		 WHERE sku = $1
		RETURNING on_hand, reserved`,
		sku, quantity).Scan(&onHand, &reserved)
	if err != nil {
		return 0, 0, fmt.Errorf("return %d of %s: %w", quantity, sku, err)
	}
	return onHand, reserved, nil
}

// AdjustOnHand changes physical stock. Refuses to go below what is reserved.
func AdjustOnHand(ctx context.Context, tx pgx.Tx, sku string, delta int) (onHand, reserved int, ok bool, err error) {
	err = tx.QueryRow(ctx, `
		UPDATE stock_levels
		   SET on_hand = on_hand + $2,
		       updated_at = now(),
		       version = version + 1
		 WHERE sku = $1
		   AND on_hand + $2 >= reserved
		   AND on_hand + $2 >= 0
		RETURNING on_hand, reserved`,
		sku, delta).Scan(&onHand, &reserved)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return onHand, reserved, true, nil
}

func AvailableFor(ctx context.Context, tx pgx.Tx, sku string) (available int, known bool, err error) {
	var onHand, reserved int
	var status string

	err = tx.QueryRow(ctx,
		`SELECT on_hand, reserved, status FROM stock_levels WHERE sku = $1`, sku).
		Scan(&onHand, &reserved, &status)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if status != "ACTIVE" {
		return 0, true, nil
	}
	if onHand-reserved < 0 {
		return 0, true, nil
	}
	return onHand - reserved, true, nil
}

// ---------------------------------------------------------------------------
// Reservations

type Reservation struct {
	ID           string
	OrderID      string
	State        ReservationState
	WasTombstone bool
	ExpiresAt    *time.Time
}

func LoadReservationForUpdate(ctx context.Context, tx pgx.Tx, id string) (Reservation, error) {
	var r Reservation
	var state string

	err := tx.QueryRow(ctx, `
		SELECT id, order_id, state, was_tombstone, expires_at
		  FROM reservations WHERE id = $1 FOR UPDATE`, id).
		Scan(&r.ID, &r.OrderID, &state, &r.WasTombstone, &r.ExpiresAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.State = ReservationState(state)
	return r, nil
}

func ReservationForOrder(ctx context.Context, tx pgx.Tx, orderID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM reservations WHERE order_id = $1`, orderID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

func InsertReservation(ctx context.Context, tx pgx.Tx, id, orderID string, state ReservationState, tombstone bool, reason string, expiresAt *time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO reservations (id, order_id, state, was_tombstone, reason_code, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, orderID, string(state), tombstone, nullIfEmpty(reason), expiresAt)
	if err != nil {
		if IsUniqueViolation(err) {
			return ErrDuplicateKey
		}
		return fmt.Errorf("insert reservation %s: %w", id, err)
	}
	return nil
}

func InsertReservationIfAbsent(ctx context.Context, tx pgx.Tx, id, orderID string, state ReservationState, reason string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO reservations (id, order_id, state, reason_code)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING`,
		id, orderID, string(state), nullIfEmpty(reason))
	return err
}

func MarkReservation(ctx context.Context, tx pgx.Tx, id string, state ReservationState, reason string) error {
	_, err := tx.Exec(ctx, `
		UPDATE reservations
		   SET state = $2,
		       reason_code = COALESCE($3, reason_code),
		       expires_at = NULL,
		       updated_at = now()
		 WHERE id = $1`,
		id, string(state), nullIfEmpty(reason))
	return err
}

type ReservationLine struct {
	SKU      string
	Quantity int
}

func InsertReservationItem(ctx context.Context, tx pgx.Tx, reservationID, sku string, quantity int) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO reservation_items (reservation_id, sku, quantity) VALUES ($1, $2, $3)`,
		reservationID, sku, quantity)
	return err
}

func ReservationItems(ctx context.Context, tx pgx.Tx, reservationID string) ([]ReservationLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT sku, quantity FROM reservation_items WHERE reservation_id = $1 ORDER BY sku`,
		reservationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReservationLine
	for rows.Next() {
		var l ReservationLine
		if err := rows.Scan(&l.SKU, &l.Quantity); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClaimExpired locks expired reservations for this sweeper instance.
// SKIP LOCKED lets several replicas sweep concurrently without coordinating.
func ClaimExpired(ctx context.Context, tx pgx.Tx, limit int) ([]Reservation, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, order_id
		  FROM reservations
		 WHERE state = 'RESERVED' AND expires_at < now()
		 ORDER BY expires_at
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ID, &r.OrderID); err != nil {
			return nil, err
		}
		r.State = StateReserved
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Ledger

type LedgerEntry struct {
	SKU           string
	ReservationID string
	OrderID       string
	Movement      string
	Quantity      int
	OnHandAfter   int
	ReservedAfter int
	Actor         string
	Note          string
}

func WriteLedger(ctx context.Context, tx pgx.Tx, e LedgerEntry) error {
	actor := e.Actor
	if actor == "" {
		actor = "system"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO stock_ledger
		  (sku, reservation_id, order_id, movement, quantity, on_hand_after, reserved_after, actor, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		e.SKU, nullIfEmpty(e.ReservationID), nullIfEmpty(e.OrderID), e.Movement,
		e.Quantity, e.OnHandAfter, e.ReservedAfter, actor, nullIfEmpty(e.Note))
	return err
}

// ---------------------------------------------------------------------------
// Levels

type LevelRow struct {
	SKU          string
	ProductID    string
	OnHand       int
	Reserved     int
	ReorderPoint int
	Status       string
}

func (s *Store) Levels(ctx context.Context, skus []string) ([]LevelRow, error) {
	if len(skus) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT sku, product_id, on_hand, reserved, reorder_point, status
		  FROM stock_levels WHERE sku = ANY($1)`, skus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LevelRow
	for rows.Next() {
		var r LevelRow
		if err := rows.Scan(&r.SKU, &r.ProductID, &r.OnHand, &r.Reserved, &r.ReorderPoint, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Outbox and inbox

type OutboxRecord struct {
	ID            int64
	AggregateType string
	AggregateID   string
	EventID       string
	EventType     string
	Topic         string
	PartitionKey  string
	Payload       []byte
	Headers       map[string]string
	CreatedAt     time.Time
	Attempts      int
}

func EnqueueOutbox(ctx context.Context, tx pgx.Tx, r OutboxRecord) error {
	headers, err := json.Marshal(r.Headers)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
		                    topic, partition_key, payload, headers)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (event_id) DO NOTHING`,
		r.AggregateType, r.AggregateID, r.EventID, r.EventType,
		r.Topic, r.PartitionKey, r.Payload, headers)
	return err
}

func ClaimOutboxBatch(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, event_type, topic, partition_key, payload, headers, created_at, attempts
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY id
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxRecord
	for rows.Next() {
		var r OutboxRecord
		var headers []byte
		if err := rows.Scan(&r.ID, &r.EventID, &r.EventType, &r.Topic,
			&r.PartitionKey, &r.Payload, &headers, &r.CreatedAt, &r.Attempts); err != nil {
			return nil, err
		}
		if len(headers) > 0 {
			_ = json.Unmarshal(headers, &r.Headers)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func MarkPublished(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
	return err
}

func MarkOutboxFailed(ctx context.Context, tx pgx.Tx, ids []int64, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	_, err := tx.Exec(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = ANY($1)`,
		ids, reason)
	return err
}

func (s *Store) OutboxBacklog(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n)
	return n, err
}

// ClaimEvent is the consumer inbox. `false` means already handled: ack the
// offset and do nothing else.
//
// Insert-first, not select-then-insert. The primary key performs the mutual
// exclusion, so two pods handed the same partition during a rebalance cannot
// both conclude they are the first to see a message.
func ClaimEvent(ctx context.Context, tx pgx.Tx, eventID, consumer string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer)
		VALUES ($1, $2)
		ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, consumer)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) PurgePublished(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM outbox
		 WHERE published_at IS NOT NULL AND published_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Store) PurgeProcessed(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM processed_events WHERE processed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Reservation is a plain read for the HTTP layer — no lock, no transaction.
// Used by the admin and by support to answer "what is holding this stock?".
func (s *Store) Reservation(ctx context.Context, id string) (Reservation, []ReservationLine, error) {
	var r Reservation
	var state string

	err := s.pool.QueryRow(ctx, `
		SELECT id, order_id, state, was_tombstone, expires_at
		  FROM reservations WHERE id = $1`, id).
		Scan(&r.ID, &r.OrderID, &state, &r.WasTombstone, &r.ExpiresAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil, ErrNotFound
	}
	if err != nil {
		return r, nil, err
	}
	r.State = ReservationState(state)

	rows, err := s.pool.Query(ctx,
		`SELECT sku, quantity FROM reservation_items WHERE reservation_id = $1 ORDER BY sku`, id)
	if err != nil {
		return r, nil, err
	}
	defer rows.Close()

	var lines []ReservationLine
	for rows.Next() {
		var l ReservationLine
		if err := rows.Scan(&l.SKU, &l.Quantity); err != nil {
			return r, nil, err
		}
		lines = append(lines, l)
	}
	return r, lines, rows.Err()
}
