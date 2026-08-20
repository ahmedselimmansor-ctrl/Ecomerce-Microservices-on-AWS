package eventbus

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"

	"github.com/souq/order-service/internal/platform"
	"github.com/souq/order-service/internal/store"
)

// Publisher is the narrow slice of Kafka the relay needs. An interface here
// keeps the relay testable without a broker.
type Publisher interface {
	Publish(ctx context.Context, topic string, msgs []kafka.Message) error
	Close() error
}

// Relay drains the transactional outbox into Kafka.
//
// It is at-least-once by construction and that is deliberate, not a compromise.
// The sequence is: claim rows (locking them), publish, mark published, commit.
// If the process dies after the publish and before the commit, the rows stay
// pending and are published again on the next tick. Making that impossible
// would require a distributed transaction across Postgres and Kafka, which is
// exactly the thing this pattern exists to avoid.
//
// The duplicates are absorbed by consumer inboxes (docs/CONTRACTS.md §5.2).
// TestWithoutTheInboxTheSideEffectRunsTwice shows what happens without them.
type Relay struct {
	store     *store.Store
	pub       Publisher
	interval  time.Duration
	batchSize int
}

func NewRelay(s *store.Store, pub Publisher, interval time.Duration, batchSize int) *Relay {
	return &Relay{store: s, pub: pub, interval: interval, batchSize: batchSize}
}

// Run blocks until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	slog.InfoContext(ctx, "outbox relay started",
		slog.Duration("interval", r.interval),
		slog.Int("batchSize", r.batchSize))

	// Jitter the first tick so that N replicas starting together after a
	// deploy do not all hit the outbox in the same millisecond.
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
				// Back off so a broker outage does not turn into a tight loop
				// that also saturates the database.
				backoff = min(backoff*2, 30*time.Second)
				slog.ErrorContext(ctx, "outbox relay tick failed",
					slog.String("error", err.Error()),
					slog.Duration("backoff", backoff))
				ticker.Reset(backoff)
			case n == r.batchSize:
				// Full batch means there is a backlog; come back immediately
				// rather than waiting out the interval.
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
		batch, err := store.ClaimBatch(ctx, tx, r.batchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		// Group by topic: kafka-go writes one topic per call, and batching
		// per topic is what keeps the produce request count sane under load.
		byTopic := map[string][]kafka.Message{}
		idsByTopic := map[string][]int64{}

		for _, rec := range batch {
			headers := make([]kafka.Header, 0, len(rec.Headers))
			for k, v := range rec.Headers {
				headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
			}
			byTopic[rec.Topic] = append(byTopic[rec.Topic], kafka.Message{
				Key:     []byte(rec.PartitionKey),
				Value:   rec.Payload,
				Headers: headers,
				Time:    rec.CreatedAt,
			})
			idsByTopic[rec.Topic] = append(idsByTopic[rec.Topic], rec.ID)
		}

		var succeeded []int64
		var firstErr error

		for topic, msgs := range byTopic {
			if err := r.pub.Publish(ctx, topic, msgs); err != nil {
				// A failure on one topic must not block the others. Record it
				// and carry on; the failed rows stay pending.
				if markErr := store.MarkFailed(ctx, tx, idsByTopic[topic], err.Error()); markErr != nil {
					slog.WarnContext(ctx, "could not record outbox failure",
						slog.String("error", markErr.Error()))
				}
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

		for _, rec := range batch {
			platform.OutboxPublishLatency.Observe(time.Since(rec.CreatedAt).Seconds())
		}
		return firstErr
	})

	if backlog, bErr := r.store.Backlog(ctx); bErr == nil {
		platform.OutboxBacklog.Set(float64(backlog))
	}

	return published, err
}

// ---------------------------------------------------------------------------

// KafkaPublisher writes to MSK. One writer per process, shared across topics.
type KafkaPublisher struct {
	writer *kafka.Writer
}

func NewKafkaPublisher(brokers []string) *KafkaPublisher {
	return &KafkaPublisher{
		writer: &kafka.Writer{
			Addr: kafka.TCP(brokers...),
			// Hash by key so all events for one order land on one partition
			// and are therefore ordered relative to each other. Ordering
			// across orders is neither provided nor needed.
			Balancer: &kafka.Hash{},
			// RequireAll: a produce is not acknowledged until every in-sync
			// replica has it. Anything weaker means a broker failure can lose
			// an event that the relay has already marked published, which
			// breaks the NoLostEvent property the outbox exists to provide.
			RequiredAcks: kafka.RequireAll,
			Compression:  kafka.Snappy,
			MaxAttempts:  3,
			BatchTimeout: 10 * time.Millisecond,
			WriteTimeout: 5 * time.Second,
			Async:        false, // synchronous: the relay must know it succeeded
			ErrorLogger: kafka.LoggerFunc(func(msg string, args ...any) {
				slog.Error("kafka writer", slog.String("msg", msg), slog.Any("args", args))
			}),
		},
	}
}

func (p *KafkaPublisher) Publish(ctx context.Context, topic string, msgs []kafka.Message) error {
	for i := range msgs {
		msgs[i].Topic = topic
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return p.writer.WriteMessages(ctx, msgs...)
}

func (p *KafkaPublisher) Close() error { return p.writer.Close() }

// EnsureTopics creates the topics this service owns if they are absent. Only
// used locally — in AWS the topics are managed by Terraform so their partition
// counts and retention are reviewed rather than accidental.
func EnsureTopics(ctx context.Context, brokers []string, topics map[string]int) error {
	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return err
	}
	defer conn.Close()

	controller, err := conn.Controller()
	if err != nil {
		return err
	}
	ctrlConn, err := kafka.DialContext(ctx, "tcp",
		net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return err
	}
	defer ctrlConn.Close()

	cfgs := make([]kafka.TopicConfig, 0, len(topics))
	for name, partitions := range topics {
		cfgs = append(cfgs, kafka.TopicConfig{
			Topic:             name,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		})
	}
	if err := ctrlConn.CreateTopics(cfgs...); err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return err
	}
	return nil
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
