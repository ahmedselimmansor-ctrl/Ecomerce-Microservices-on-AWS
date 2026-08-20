// Package eventbus is the Kafka boundary: the outbox relay that publishes, and
// the consumer that receives saga commands.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"

	"github.com/souq/inventory-service/internal/platform"
	"github.com/souq/inventory-service/internal/stock"
	"github.com/souq/inventory-service/internal/store"
)

const (
	TopicCommands = "souq.order.commands.v1"
	ConsumerGroup = "inventory-service.saga-commands"
)

func DLQ(topic string) string { return topic + ".dlq" }

// ---------------------------------------------------------------------------
// Publisher

type Publisher struct{ writer *kafka.Writer }

func NewPublisher(brokers []string) *Publisher {
	return &Publisher{writer: &kafka.Writer{
		Addr: kafka.TCP(brokers...),
		// Hash by key so all events for one order land on one partition and
		// are ordered relative to each other. Ordering across orders is
		// neither provided nor needed.
		Balancer: &kafka.Hash{},
		// RequireAll: a produce is not acknowledged until every in-sync
		// replica has it. Anything weaker means a broker failure can lose an
		// event the relay has already marked published, which defeats the
		// entire point of the outbox.
		RequiredAcks: kafka.RequireAll,
		Compression:  kafka.Snappy,
		MaxAttempts:  3,
		BatchTimeout: 10 * time.Millisecond,
		WriteTimeout: 5 * time.Second,
		Async:        false, // the relay must know it succeeded
	}}
}

func (p *Publisher) Publish(ctx context.Context, topic string, msgs []kafka.Message) error {
	for i := range msgs {
		msgs[i].Topic = topic
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return p.writer.WriteMessages(ctx, msgs...)
}

func (p *Publisher) Close() error { return p.writer.Close() }

// ---------------------------------------------------------------------------
// Relay

// Relay drains the transactional outbox into Kafka.
//
// At-least-once by construction: claim rows (locking them), publish, mark,
// commit. A crash after the publish and before the commit republishes on the
// next tick. Making that impossible would need a distributed transaction
// across Postgres and Kafka, which is the thing the outbox exists to avoid.
type Relay struct {
	store     *store.Store
	pub       *Publisher
	interval  time.Duration
	batchSize int
}

func NewRelay(s *store.Store, pub *Publisher, interval time.Duration, batch int) *Relay {
	return &Relay{store: s, pub: pub, interval: interval, batchSize: batch}
}

func (r *Relay) Run(ctx context.Context) {
	slog.InfoContext(ctx, "outbox relay started",
		slog.Duration("interval", r.interval), slog.Int("batchSize", r.batchSize))

	// Jitter the first tick so N replicas starting together after a deploy do
	// not all hit the outbox in the same millisecond.
	select {
	case <-time.After(time.Duration(rand.Int63n(int64(r.interval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	backoff := r.interval

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "outbox relay stopping")
			return
		case <-ticker.C:
			n, err := r.tick(ctx)
			switch {
			case err != nil:
				// Back off so a broker outage does not become a tight loop
				// that also saturates the database.
				backoff = minDur(backoff*2, 30*time.Second)
				slog.ErrorContext(ctx, "relay tick failed",
					slog.String("error", err.Error()), slog.Duration("backoff", backoff))
				ticker.Reset(backoff)
			case n == r.batchSize:
				// A full batch means there is a backlog; come back at once.
				backoff = r.interval
				ticker.Reset(time.Millisecond)
			default:
				backoff = r.interval
				ticker.Reset(r.interval)
			}
		}
	}
}

func (r *Relay) tick(ctx context.Context) (int, error) {
	var published int

	err := r.store.InTx(ctx, func(tx pgx.Tx) error {
		batch, err := store.ClaimOutboxBatch(ctx, tx, r.batchSize)
		if err != nil || len(batch) == 0 {
			return err
		}

		byTopic := map[string][]kafka.Message{}
		idsByTopic := map[string][]int64{}

		for _, rec := range batch {
			headers := make([]kafka.Header, 0, len(rec.Headers))
			for k, v := range rec.Headers {
				headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
			}
			byTopic[rec.Topic] = append(byTopic[rec.Topic], kafka.Message{
				Key: []byte(rec.PartitionKey), Value: rec.Payload,
				Headers: headers, Time: rec.CreatedAt,
			})
			idsByTopic[rec.Topic] = append(idsByTopic[rec.Topic], rec.ID)
		}

		var succeeded []int64
		var firstErr error

		for topic, msgs := range byTopic {
			if err := r.pub.Publish(ctx, topic, msgs); err != nil {
				// A failure on one topic must not block the others.
				_ = store.MarkOutboxFailed(ctx, tx, idsByTopic[topic], err.Error())
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			succeeded = append(succeeded, idsByTopic[topic]...)
		}

		if err := store.MarkPublished(ctx, tx, succeeded); err != nil {
			return err
		}
		published = len(succeeded)
		return firstErr
	})

	if backlog, bErr := r.store.OutboxBacklog(ctx); bErr == nil {
		platform.OutboxBacklog.Set(float64(backlog))
	}
	return published, err
}

// ---------------------------------------------------------------------------
// Consumer

type Consumer struct {
	reader *kafka.Reader
	pub    *Publisher
	engine *stock.Engine
	store  *store.Store
}

func NewConsumer(brokers []string, group string, pub *Publisher, e *stock.Engine, s *store.Store) *Consumer {
	return &Consumer{
		pub: pub, engine: e, store: s,
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			GroupID:  group,
			Topic:    TopicCommands,
			MinBytes: 1,
			MaxBytes: 10 << 20,
			MaxWait:  500 * time.Millisecond,
			// Commit explicitly after the handler succeeds. Auto-commit would
			// acknowledge messages we have not processed, and unlike a
			// duplicate, a LOST reserve command wedges an order.
			CommitInterval:    0,
			StartOffset:       kafka.FirstOffset,
			SessionTimeout:    30 * time.Second,
			RebalanceTimeout:  30 * time.Second,
			HeartbeatInterval: 3 * time.Second,
		}),
	}
}

// accepted are the only command types this service handles. The topic is
// shared with payment-service, so most traffic on it is not ours and the
// header filter avoids paying to deserialise it.
var accepted = map[string]bool{
	"souq.inventory.reserve.v1": true,
	"souq.inventory.release.v1": true,
	"souq.inventory.commit.v1":  true,
}

func (c *Consumer) Run(ctx context.Context) error {
	slog.InfoContext(ctx, "consumer started", slog.String("topic", TopicCommands))

	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				slog.InfoContext(ctx, "consumer stopping")
				return nil
			}
			slog.ErrorContext(ctx, "fetch failed", slog.String("error", err.Error()))
			// Do not spin: a broker that is down stays down for a while.
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil
			}
			continue
		}

		c.handle(ctx, msg)

		if err := c.reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			// Applied but the offset did not stick. It will be redelivered;
			// the inbox makes that harmless.
			slog.ErrorContext(ctx, "commit failed; message will be redelivered",
				slog.String("error", err.Error()), slog.Int64("offset", msg.Offset))
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg kafka.Message) {
	eventType := header(msg, "ce_type")
	if eventType != "" && !accepted[eventType] {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "filtered").Inc()
		return
	}

	var env struct {
		ID            string          `json:"id"`
		Type          string          `json:"type"`
		CorrelationID string          `json:"correlationid"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		c.toDLQ(ctx, msg, "malformed cloudevents envelope: "+err.Error(), 0)
		return
	}
	if !accepted[env.Type] {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "filtered").Inc()
		return
	}
	if env.ID == "" {
		// Without an id the message cannot be deduplicated, which is worse
		// than dropping it: it would be applied on every redelivery forever.
		c.toDLQ(ctx, msg, "event has no id and cannot be deduplicated", 0)
		return
	}

	var body struct {
		OrderID       string              `json:"orderId"`
		ReservationID string              `json:"reservationId"`
		ReasonCode    string              `json:"reasonCode"`
		TTLSeconds    int                 `json:"ttlSeconds"`
		Items         []stock.ReserveItem `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		c.toDLQ(ctx, msg, "unparseable command payload: "+err.Error(), 0)
		return
	}
	if body.OrderID == "" || body.ReservationID == "" {
		c.toDLQ(ctx, msg, "command is missing orderId or reservationId", 0)
		return
	}

	hctx := platform.WithCorrelationID(platform.WithRequestID(ctx, env.ID), env.CorrelationID)

	// Inbox claim. A duplicate is a no-op — the reason the outbox model needs
	// all three parts.
	var fresh bool
	if err := c.store.InTx(hctx, func(tx pgx.Tx) error {
		var err error
		fresh, err = store.ClaimEvent(hctx, tx, env.ID, ConsumerGroup)
		return err
	}); err != nil {
		slog.ErrorContext(hctx, "inbox claim failed; not committing the offset",
			slog.String("error", err.Error()))
		return
	}
	if !fresh {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "duplicate").Inc()
		return
	}

	if err := c.apply(hctx, env.Type, body.OrderID, body.ReservationID,
		body.ReasonCode, body.TTLSeconds, body.Items, env.CorrelationID); err != nil {
		if errors.Is(err, stock.ErrReleaseAfterCommit) {
			// Permanent AND alarming. The saga compensated past the point of
			// no return, which three independent mechanisms are supposed to
			// prevent.
			platform.CompensationAfterCommit.Inc()
			slog.ErrorContext(hctx, "RELEASE FOR A COMMITTED RESERVATION — see docs/DESIGN-INVARIANTS.md §1",
				slog.String("reservationId", body.ReservationID),
				slog.String("orderId", body.OrderID),
				slog.String("runbook", "docs/runbooks/illegal-saga-transition.md"))
			c.toDLQ(hctx, msg, err.Error(), 1)
			return
		}
		slog.ErrorContext(hctx, "command handler failed",
			slog.String("type", env.Type), slog.String("error", err.Error()))
		c.toDLQ(hctx, msg, err.Error(), 1)
		return
	}

	platform.EventsConsumed.WithLabelValues(msg.Topic, "applied").Inc()
}

func (c *Consumer) apply(ctx context.Context, eventType, orderID, reservationID, reason string, ttlSeconds int, items []stock.ReserveItem, correlationID string) error {
	switch eventType {
	case "souq.inventory.reserve.v1":
		ttl := stock.DefaultTTL
		if ttlSeconds > 0 {
			ttl = time.Duration(ttlSeconds) * time.Second
		}
		res, err := c.engine.Reserve(ctx, reservationID, orderID, items, ttl, correlationID)
		if err != nil {
			return err
		}
		platform.Reservations.WithLabelValues(string(res.Outcome)).Inc()
		for _, u := range res.Unavailable {
			platform.Stockouts.WithLabelValues(u.SKU).Inc()
		}
		return nil

	case "souq.inventory.release.v1":
		if reason == "" {
			reason = "SAGA_COMPENSATION"
		}
		tombstone, err := c.engine.Release(ctx, reservationID, orderID, reason, correlationID)
		if err != nil {
			return err
		}
		if tombstone {
			platform.Tombstones.Inc()
		}
		return nil

	case "souq.inventory.commit.v1":
		return c.engine.Commit(ctx, reservationID, orderID, correlationID)
	}
	return fmt.Errorf("unhandled command type %q", eventType)
}

// toDLQ parks a message and commits the offset. Blocking the partition on one
// bad message stalls every order behind it.
func (c *Consumer) toDLQ(ctx context.Context, msg kafka.Message, reason string, attempts int) {
	platform.EventsConsumed.WithLabelValues(msg.Topic, "dlq").Inc()

	slog.ErrorContext(ctx, "sending message to DLQ",
		slog.String("topic", msg.Topic), slog.Int64("offset", msg.Offset),
		slog.String("eventId", header(msg, "ce_id")),
		slog.String("eventType", header(msg, "ce_type")),
		slog.String("reason", reason))

	headers := append([]kafka.Header{}, msg.Headers...)
	headers = append(headers,
		kafka.Header{Key: "x-dlq-reason", Value: []byte(reason)},
		kafka.Header{Key: "x-dlq-original-topic", Value: []byte(msg.Topic)},
		kafka.Header{Key: "x-dlq-attempts", Value: []byte(fmt.Sprint(attempts))},
		kafka.Header{Key: "x-dlq-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	)

	// A fresh context: the parent may already be cancelled by shutdown, and
	// losing the DLQ copy is how a bug becomes unreproducible.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := c.pub.Publish(dctx, DLQ(msg.Topic), []kafka.Message{{
		Key: msg.Key, Value: msg.Value, Headers: headers,
	}}); err != nil {
		slog.ErrorContext(ctx, "DLQ publish failed; the message is being dropped",
			slog.String("error", err.Error()))
	}
}

func (c *Consumer) Close() error { return c.reader.Close() }
func (c *Consumer) Lag() int64   { return c.reader.Lag() }

func header(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
