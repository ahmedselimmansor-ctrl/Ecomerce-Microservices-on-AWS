package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OutboxRecord is one pending event. It carries everything the relay needs to
// publish without touching another table.
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

// Enqueue writes an event into the outbox inside the caller's transaction.
//
// This function taking a pgx.Tx rather than a *Store is the entire point. It
// is impossible to call it outside a transaction, which makes the dual-write
// bug from internal/eventbus/outbox_model_test.go §5a unrepresentable in this codebase.
func Enqueue(ctx context.Context, tx pgx.Tx, r OutboxRecord) error {
	headers, err := json.Marshal(r.Headers)
	if err != nil {
		return fmt.Errorf("marshal outbox headers: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_id, event_type,
		                    topic, partition_key, payload, headers)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (event_id) DO NOTHING`,
		r.AggregateType, r.AggregateID, r.EventID, r.EventType,
		r.Topic, r.PartitionKey, r.Payload, headers)
	if err != nil {
		return fmt.Errorf("enqueue outbox event %s: %w", r.EventType, err)
	}
	return nil
}

// ClaimBatch locks up to `limit` unpublished rows for this relay instance.
//
// FOR UPDATE SKIP LOCKED is what lets several relay replicas run concurrently
// without coordinating: each grabs a disjoint set and no row is ever published
// twice by two pods in the same instant. Duplicates can still happen across a
// crash — that is inherent, which is why consumers dedup (§5.2).
//
// The rows stay locked until the caller commits or rolls back the transaction,
// so ClaimBatch must be called inside InTx and the publish must happen before
// that transaction ends.
func ClaimBatch(ctx context.Context, tx pgx.Tx, limit int) ([]OutboxRecord, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_id, event_type,
		       topic, partition_key, payload, headers, created_at, attempts
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY id
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim outbox batch: %w", err)
	}
	defer rows.Close()

	var out []OutboxRecord
	for rows.Next() {
		var r OutboxRecord
		var headers []byte
		if err := rows.Scan(&r.ID, &r.AggregateType, &r.AggregateID, &r.EventID,
			&r.EventType, &r.Topic, &r.PartitionKey, &r.Payload, &headers,
			&r.CreatedAt, &r.Attempts); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		if len(headers) > 0 {
			if err := json.Unmarshal(headers, &r.Headers); err != nil {
				return nil, fmt.Errorf("unmarshal outbox headers for %s: %w", r.EventID, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkPublished stamps rows as done. If the process dies between the Kafka
// write and this call, the rows stay pending and are republished on the next
// tick — at-least-once, exactly as the model assumes.
func MarkPublished(ctx context.Context, tx pgx.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	return nil
}

// MarkFailed records a publish failure so a poison row is visible rather than
// silently retried forever at full speed.
func MarkFailed(ctx context.Context, tx pgx.Tx, ids []int64, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE outbox
		   SET attempts = attempts + 1,
		       last_error = $2
		 WHERE id = ANY($1)`, ids, truncate(reason, 1000))
	return err
}

// Backlog is the relay's health signal. Sustained growth means the relay is
// losing to the write rate and events are getting stale, even though every
// individual publish is succeeding.
func (s *Store) Backlog(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n)
	return n, err
}

// PurgePublished trims history. Called by a nightly job; the outbox is a queue,
// not an audit log — the events themselves live in Kafka with real retention.
func (s *Store) PurgePublished(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM outbox
		 WHERE published_at IS NOT NULL
		   AND published_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Inbox (docs/CONTRACTS.md §5.2)

// MarkProcessed claims an event for a consumer. It returns false when the
// event was already handled, which is the signal to ack the Kafka offset and
// do nothing else.
//
// Insert-first, not select-then-insert. The primary key does the mutual
// exclusion, so two pods consuming a rebalanced partition cannot both decide
// they are the first to see a message — the same argument as
// payment-service internal/psp/paymob_test.go §4b.
func MarkProcessed(ctx context.Context, tx pgx.Tx, eventID, consumer string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id, consumer)
		VALUES ($1, $2)
		ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, consumer)
	if err != nil {
		return false, fmt.Errorf("mark event processed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PurgeProcessed trims the inbox. 30 days is far longer than any realistic
// redelivery window, and the table is only read by primary-key lookup.
func (s *Store) PurgeProcessed(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM processed_events
		 WHERE processed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
