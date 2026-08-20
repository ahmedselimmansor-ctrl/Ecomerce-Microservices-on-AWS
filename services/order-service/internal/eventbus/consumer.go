package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/souq/order-service/internal/platform"
)

// Handler processes one event. Returning nil means "applied, commit the
// offset". Returning an error means "retry"; after MaxAttempts the message
// goes to the DLQ and the offset is committed anyway, because a poison message
// must never block the partition behind it.
type Handler func(ctx context.Context, e Envelope) error

// ErrPermanent tells the consumer not to bother retrying — the message is
// malformed or references something that will never exist. Straight to DLQ.
var ErrPermanent = errors.New("permanent failure")

type ConsumerConfig struct {
	Brokers     []string
	Topics      []string
	GroupID     string
	MaxAttempts int
	// Types this consumer cares about. Empty means all. Filtering on the
	// header avoids deserialising the ~50% of messages on the shared command
	// topic that belong to another service.
	AcceptTypes map[string]bool
}

type Consumer struct {
	cfg    ConsumerConfig
	reader *kafka.Reader
	dlq    Publisher
}

func NewConsumer(cfg ConsumerConfig, dlq Publisher) *Consumer {
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 5
	}
	return &Consumer{
		cfg: cfg,
		dlq: dlq,
		reader: kafka.NewReader(kafka.ReaderConfig{
			Brokers:     cfg.Brokers,
			GroupTopics: cfg.Topics,
			GroupID:     cfg.GroupID,
			MinBytes:    1,
			MaxBytes:    10 << 20,
			MaxWait:     500 * time.Millisecond,
			// Commit explicitly after the handler succeeds. Auto-commit would
			// acknowledge messages we have not processed yet, so a crash would
			// lose them — and unlike a duplicate, a lost saga event wedges an
			// order permanently.
			CommitInterval: 0,
			StartOffset:    kafka.FirstOffset,
			// A slow handler must not be mistaken for a dead consumer and
			// trigger a rebalance mid-transaction.
			SessionTimeout:    30 * time.Second,
			RebalanceTimeout:  30 * time.Second,
			HeartbeatInterval: 3 * time.Second,
			ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
				slog.Error("kafka reader", slog.String("msg", msg), slog.Any("args", args))
			}),
		}),
	}
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	slog.InfoContext(ctx, "consumer started",
		slog.String("group", c.cfg.GroupID),
		slog.Any("topics", c.cfg.Topics))

	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				slog.InfoContext(ctx, "consumer stopping", slog.String("group", c.cfg.GroupID))
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

		c.handle(ctx, msg, h)

		if err := c.reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			// The message has been applied but the offset did not stick. It
			// will be redelivered; the inbox makes that harmless.
			slog.ErrorContext(ctx, "commit failed; message will be redelivered",
				slog.String("error", err.Error()),
				slog.String("topic", msg.Topic),
				slog.Int64("offset", msg.Offset))
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg kafka.Message, h Handler) {
	eventType := headerValue(msg, "ce_type")

	// Cheap header filter before we pay for JSON.
	if len(c.cfg.AcceptTypes) > 0 && eventType != "" && !c.cfg.AcceptTypes[eventType] {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "filtered").Inc()
		return
	}

	var e Envelope
	if err := json.Unmarshal(msg.Value, &e); err != nil {
		c.toDLQ(ctx, msg, "malformed cloudevents envelope: "+err.Error(), 0)
		return
	}
	if err := e.Validate(); err != nil {
		c.toDLQ(ctx, msg, "invalid envelope: "+err.Error(), 0)
		return
	}
	if len(c.cfg.AcceptTypes) > 0 && !c.cfg.AcceptTypes[e.Type] {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "filtered").Inc()
		return
	}

	// Propagate the correlation id so the whole saga shows up under one id in
	// the logs, even though it spans four services and two hours.
	hctx := platform.WithRequestID(ctx, e.ID)
	if e.CorrelationID != "" {
		hctx = platform.WithCorrelationID(hctx, e.CorrelationID)
	}

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		err := h(hctx, e)
		if err == nil {
			platform.EventsConsumed.WithLabelValues(msg.Topic, "applied").Inc()
			return
		}
		lastErr = err

		if errors.Is(err, ErrPermanent) {
			c.toDLQ(hctx, msg, err.Error(), attempt)
			return
		}
		if hctx.Err() != nil {
			return // shutting down; let the message be redelivered
		}

		// Exponential backoff with full jitter (docs/CONTRACTS.md §5.4).
		// Full jitter rather than fixed backoff because otherwise every
		// consumer that failed on the same downstream outage retries in
		// lockstep and re-creates the thundering herd that caused it.
		base := 100 * time.Millisecond * time.Duration(1<<uint(attempt-1))
		if base > 2*time.Second {
			base = 2 * time.Second
		}
		wait := time.Duration(rand.Int63n(int64(base) + 1))

		slog.WarnContext(hctx, "event handler failed, retrying",
			slog.String("eventId", e.ID),
			slog.String("eventType", e.Type),
			slog.Int("attempt", attempt),
			slog.Duration("backoff", wait),
			slog.String("error", err.Error()))

		select {
		case <-time.After(wait):
		case <-hctx.Done():
			return
		}
	}

	c.toDLQ(hctx, msg, fmt.Sprintf("exhausted %d attempts: %v", c.cfg.MaxAttempts, lastErr), c.cfg.MaxAttempts)
}

// toDLQ parks a message and commits the offset. Blocking the partition on one
// bad message would stall every other order behind it, which turns a single
// broken payload into an outage.
func (c *Consumer) toDLQ(ctx context.Context, msg kafka.Message, reason string, attempts int) {
	platform.EventsConsumed.WithLabelValues(msg.Topic, "dlq").Inc()

	slog.ErrorContext(ctx, "sending message to DLQ",
		slog.String("topic", msg.Topic),
		slog.Int("partition", msg.Partition),
		slog.Int64("offset", msg.Offset),
		slog.String("eventId", headerValue(msg, "ce_id")),
		slog.String("eventType", headerValue(msg, "ce_type")),
		slog.Int("attempts", attempts),
		slog.String("reason", reason))

	if c.dlq == nil {
		return
	}

	headers := append([]kafka.Header{}, msg.Headers...)
	headers = append(headers,
		kafka.Header{Key: "x-dlq-reason", Value: []byte(reason)},
		kafka.Header{Key: "x-dlq-original-topic", Value: []byte(msg.Topic)},
		kafka.Header{Key: "x-dlq-attempts", Value: []byte(fmt.Sprint(attempts))},
		kafka.Header{Key: "x-dlq-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
	)

	// Use a fresh context: the parent may already be cancelled by shutdown,
	// and losing the DLQ copy is how a bug becomes unreproducible.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := c.dlq.Publish(dctx, DLQ(msg.Topic), []kafka.Message{{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	}}); err != nil {
		slog.ErrorContext(ctx, "DLQ publish failed; message is being dropped",
			slog.String("error", err.Error()),
			slog.String("originalTopic", msg.Topic),
			slog.Int64("offset", msg.Offset))
	}
}

func (c *Consumer) Close() error { return c.reader.Close() }

// Lag reports consumer lag for the readiness probe and the dashboard.
func (c *Consumer) Lag() int64 { return c.reader.Lag() }

func headerValue(m kafka.Message, key string) string {
	for _, h := range m.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
