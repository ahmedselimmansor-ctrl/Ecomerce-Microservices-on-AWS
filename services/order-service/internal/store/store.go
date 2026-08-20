// Package store is the only place in order-service that knows SQL exists.
//
// Two rules hold throughout:
//
//  1. Every write that must be published as an event takes a pgx.Tx and writes
//     to the outbox inside it. There is no code path that commits a business
//     change and then publishes — internal/eventbus/outbox_model_test.go shows that loses
//     events on crash, and the model needs only two steps to find it.
//  2. Nothing here retries. Retry policy belongs to the caller, which knows
//     whether the operation is safe to repeat; a repository that silently
//     retries a non-idempotent write is a duplicate-charge waiting to happen.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound     = errors.New("store: not found")
	ErrVersionStale = errors.New("store: row changed since it was read")
	ErrDuplicateKey = errors.New("store: duplicate key")
)

type Store struct {
	pool *pgxpool.Pool
}

// Open builds a pool with the timeouts docs/CONTRACTS.md §5.4 mandates. The
// statement_timeout is set on the connection rather than per query so that a
// pathological plan cannot pin a connection indefinitely no matter which code
// path issued it.
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
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", statementTimeout.Milliseconds())
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	cfg.ConnConfig.RuntimeParams["application_name"] = "order-service"

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

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a transaction, rolling back on error or panic.
//
// The isolation level is ReadCommitted, not Serializable. Every invariant this
// service needs is enforced either by a unique constraint or by a conditional
// UPDATE that the database evaluates atomically, so the extra serialisation
// failures Serializable produces would buy nothing but retry churn. Where
// stronger isolation IS needed — the inventory reservation path — the
// conditional-UPDATE strategy proved in inventory-service internal/stock/model_test.go is
// used instead, which is safe at ReadCommitted by construction.
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

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// IsUniqueViolation reports a 23505. Used to turn a race on a unique index
// into a clean domain outcome rather than a 500 — the pattern that makes
// insert-first idempotency work (payment-service internal/psp/paymob_test.go).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsCheckViolation reports a 23514 — a CHECK constraint rejected the row.
// In this service that always means a code path tried to write a state the
// state machine forbids, so it is logged as a bug, never as bad input.
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// IsSerializationFailure reports a 40001 or 40P01 (deadlock). Safe to retry.
func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}
