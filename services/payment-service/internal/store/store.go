// Package store implements service.Store against Postgres.
//
// The service layer talks to interfaces, so this file is the only place that
// knows SQL exists — and the only place that can get the transaction boundary
// wrong. Two rules hold throughout:
//
//  1. Every write that must be published takes a transaction and writes to the
//     outbox inside it. There is no path that commits then publishes.
//  2. The idempotency key is claimed with ONE atomic statement. Never
//     SELECT-then-INSERT — two concurrent retries both observing an absent row
//     is a real interleaving, and it double-charges.
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

	"github.com/souq/payment-service/internal/service"
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
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", statementTimeout.Milliseconds())
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "payment-service"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InTx satisfies service.Store.
func (s *Store) InTx(ctx context.Context, fn func(service.Tx) error) error {
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

	if err := fn(&txn{tx: tx}); err != nil {
		// A context that survives the caller's cancellation, or a client
		// disconnect leaves the transaction open until the idle timeout.
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetPaymentByOrder(ctx context.Context, orderID string) (*service.Payment, error) {
	return scanPayment(s.pool.QueryRow(ctx, selectPayment+` WHERE order_id = $1`, orderID))
}

// GetPaymentByMerchantRef resolves a provider callback back to a payment.
//
// For Paymob the merchant reference IS our derived provider key, sent as
// `merchant_order_id`. That is the only handle a callback gives us.
func (s *Store) GetPaymentByMerchantRef(ctx context.Context, ref string) (*service.Payment, error) {
	return scanPayment(s.pool.QueryRow(ctx, selectPayment+` WHERE psp_idempotency_key = $1`, ref))
}

func (s *Store) UnbalancedLedgerGroups(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM unbalanced_entry_groups`).Scan(&n)
	return n, err
}

// NeedsReconciliation finds payments stranded mid-flight — the crash-between-
// transactions case. The reconciler presents the same provider key and asks
// what actually happened.
func (s *Store) NeedsReconciliation(ctx context.Context, olderThan time.Duration) ([]*service.Payment, error) {
	rows, err := s.pool.Query(ctx, selectPayment+`
		 WHERE state IN ('AUTHORIZING','CAPTURING')
		   AND updated_at < now() - $1::interval
		 ORDER BY updated_at
		 LIMIT 100`, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*service.Payment
	for rows.Next() {
		p, err := scanPaymentRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------

type txn struct{ tx pgx.Tx }

const selectPayment = `
	SELECT id, order_id, user_id, state, amount, currency, captured_amount,
	       refunded_amount, provider, payment_method_token, psp_idempotency_key,
	       psp_authorization_id, psp_capture_id, authorization_expires_at,
	       correlation_id, version
	  FROM payments`

type scannable interface{ Scan(dest ...any) error }

func scanPayment(row scannable) (*service.Payment, error) {
	p, err := scanPaymentRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, service.ErrNotFound
	}
	return p, err
}

func scanPaymentRow(row scannable) (*service.Payment, error) {
	var p service.Payment
	var state string
	var authID, captureID *string

	err := row.Scan(&p.ID, &p.OrderID, &p.UserID, &state, &p.Amount, &p.Currency,
		&p.CapturedAmount, &p.RefundedAmount, &p.Provider, &p.MethodToken,
		&p.PSPIdempotencyKey, &authID, &captureID, &p.AuthExpiresAt,
		&p.CorrelationID, &p.Version)
	if err != nil {
		return nil, err
	}
	p.State = service.State(state)
	p.ProviderAuthID = deref(authID)
	p.ProviderCaptureID = deref(captureID)
	return &p, nil
}

func (t *txn) GetPaymentForUpdate(ctx context.Context, id string) (*service.Payment, error) {
	p, err := scanPaymentRow(t.tx.QueryRow(ctx, selectPayment+` WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, service.ErrNotFound
	}
	return p, err
}

func (t *txn) InsertPayment(ctx context.Context, p *service.Payment) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO payments (id, order_id, user_id, state, currency, amount,
		                      provider, payment_method_token, psp_idempotency_key,
		                      correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		p.ID, p.OrderID, p.UserID, string(p.State), p.Currency, p.Amount,
		p.Provider, p.MethodToken, p.PSPIdempotencyKey, p.CorrelationID)
	if err != nil {
		if isUnique(err) {
			// Either a second payment for one order, or two payments sharing a
			// provider key. Both are constraints that exist to stop a double
			// charge, so both are a domain outcome and not a 500.
			return service.ErrDuplicate
		}
		return fmt.Errorf("insert payment %s: %w", p.ID, err)
	}
	return nil
}

// UpdatePaymentState advances the state with optimistic locking.
//
// The version check stops two concurrent handlers — a Kafka redelivery racing
// the reconciler — from both applying a transition.
func (t *txn) UpdatePaymentState(ctx context.Context, id string, expectVersion int, next service.State, f service.StateFields) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE payments
		   SET state = $3,
		       psp_authorization_id = COALESCE($4, psp_authorization_id),
		       psp_capture_id = COALESCE($5, psp_capture_id),
		       auth_code = COALESCE($6, auth_code),
		       decline_code = COALESCE($7, decline_code),
		       reason_code = COALESCE($8, reason_code),
		       retriable = COALESCE($9, retriable),
		       captured_amount = COALESCE($10, captured_amount),
		       authorization_expires_at = COALESCE($11, authorization_expires_at),
		       updated_at = now(),
		       version = version + 1
		 WHERE id = $1 AND version = $2`,
		id, expectVersion, string(next),
		nullIfEmpty(f.ProviderAuthID), nullIfEmpty(f.ProviderCaptureID),
		nullIfEmpty(f.AuthCode), nullIfEmpty(f.DeclineCode), nullIfEmpty(f.ReasonCode),
		f.Retriable, f.CapturedAmount, f.AuthExpiresAt)
	if err != nil {
		if isCheck(err) {
			// A CHECK rejected it: capture above authorisation, refund above
			// capture, or an unmodelled state. All are bugs, not bad input.
			return fmt.Errorf("payment %s: the database rejected state %q: %w", id, next, err)
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return service.ErrVersionStale
	}
	return nil
}

func (t *txn) RecordAttempt(ctx context.Context, a service.Attempt) error {
	var redacted []byte
	if a.RedactedResponse != nil {
		var err error
		if redacted, err = json.Marshal(a.RedactedResponse); err != nil {
			return err
		}
	}
	// Append-only. This is what a support agent and the reconciler read when
	// the question is "did we actually charge them?" — a question the payments
	// row alone cannot always answer.
	_, err := t.tx.Exec(ctx, `
		INSERT INTO payment_attempts
		  (payment_id, operation, attempt_no, psp_idempotency_key, outcome,
		   psp_reference, latency_ms, redacted_response, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		a.PaymentID, a.Operation, a.AttemptNo, a.PSPKey, a.Outcome,
		nullIfEmpty(a.ProviderRef), a.LatencyMs, redacted, nullIfEmpty(a.ErrorMessage))
	return err
}

func (t *txn) WriteLedger(ctx context.Context, entries []service.LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, e := range entries {
		batch.Queue(`
			INSERT INTO ledger_entries
			  (payment_id, order_id, account, direction, amount, currency, entry_group, description)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			e.PaymentID, e.OrderID, e.Account, e.Direction, e.Amount,
			e.Currency, uuidFrom(e.EntryGroup), nullIfEmpty(e.Description))
	}
	return t.tx.SendBatch(ctx, batch).Close()
}

func (t *txn) Enqueue(ctx context.Context, e service.OutboxEvent) error {
	headers, err := json.Marshal(e.Headers)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
		                    topic, partition_key, payload, headers)
		VALUES ('payment',$1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		e.AggregateID, e.EventID, e.EventType, e.Topic, e.PartitionKey, e.Payload, headers)
	return err
}

// ClaimEvent is the inbox. Insert-first, not select-then-insert: the primary
// key does the mutual exclusion, so two pods handed the same partition during
// a rebalance cannot both decide they are first.
func (t *txn) ClaimEvent(ctx context.Context, eventID, consumer string) (bool, error) {
	tag, err := t.tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer)
		VALUES ($1,$2) ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, consumer)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------------
// Outbox relay support

type OutboxRecord struct {
	ID           int64
	EventID      string
	EventType    string
	Topic        string
	PartitionKey string
	Payload      []byte
	Headers      map[string]string
	CreatedAt    time.Time
}

func (s *Store) ClaimOutbox(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, event_id, event_type, topic, partition_key, payload, headers, created_at
		  FROM outbox WHERE published_at IS NULL
		 ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutboxRecord
	for rows.Next() {
		var r OutboxRecord
		var headers []byte
		if err := rows.Scan(&r.ID, &r.EventID, &r.EventType, &r.Topic,
			&r.PartitionKey, &r.Payload, &headers, &r.CreatedAt); err != nil {
			return nil, err
		}
		if len(headers) > 0 {
			_ = json.Unmarshal(headers, &r.Headers)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) MarkPublished(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
	return err
}

func (s *Store) RawTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) OutboxBacklog(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isCheck(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// uuidFrom turns the service layer's opaque group id into the UUID the column
// expects. The service does not know about Postgres types and should not.
func uuidFrom(s string) string {
	if len(s) == 36 && s[8] == '-' {
		return s
	}
	// Deterministic padding into UUID shape from a ULID-ish id.
	h := fmt.Sprintf("%032x", []byte(s))
	if len(h) > 32 {
		h = h[:32]
	}
	for len(h) < 32 {
		h += "0"
	}
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
