package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// ---------------------------------------------------------------------------
// Config

type Config struct {
	ServiceName string
	Version     string
	Env         string
	HTTPAddr    string

	DatabaseURL        string
	DBMaxConns         int32
	DBStatementTimeout time.Duration

	KafkaBrokers  []string
	ConsumerGroup string

	ShutdownGrace time.Duration

	// Relay tuning. 200ms/100 keeps p99 publish latency under a second while
	// staying comfortably inside a single Postgres round trip per tick.
	OutboxPollInterval time.Duration
	OutboxBatchSize    int

	SweeperInterval time.Duration
}

// LoadConfig reads SOUQ_* environment variables and fails fast on anything
// missing. A service that boots with half its configuration and discovers the
// gap on the first real request is strictly worse than one that refuses to
// start (docs/CONTRACTS.md §10).
func LoadConfig(serviceName string) (Config, error) {
	c := Config{
		ServiceName:        serviceName,
		Version:            env("SOUQ_VERSION", "dev"),
		Env:                env("SOUQ_ENV", "local"),
		HTTPAddr:           env("SOUQ_HTTP_ADDR", ":8084"),
		DatabaseURL:        os.Getenv("SOUQ_DB_URL"),
		DBMaxConns:         int32(envInt("SOUQ_DB_MAX_CONNS", 20)),
		DBStatementTimeout: envDuration("SOUQ_DB_STATEMENT_TIMEOUT", 3*time.Second),
		ConsumerGroup:      env("SOUQ_CONSUMER_GROUP", serviceName+".saga-events"),
		ShutdownGrace:      envDuration("SOUQ_SHUTDOWN_GRACE", 20*time.Second),
		OutboxPollInterval: envDuration("SOUQ_OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		OutboxBatchSize:    envInt("SOUQ_OUTBOX_BATCH_SIZE", 100),
		SweeperInterval:    envDuration("SOUQ_SWEEPER_INTERVAL", 5*time.Second),
	}

	brokers := env("SOUQ_KAFKA_BROKERS", "")
	if brokers != "" {
		c.KafkaBrokers = strings.Split(brokers, ",")
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "SOUQ_DB_URL")
	}
	if len(c.KafkaBrokers) == 0 {
		missing = append(missing, "SOUQ_KAFKA_BROKERS")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		slog.Warn("ignoring unparseable integer env var", slog.String("key", k), slog.String("value", v))
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("ignoring unparseable duration env var", slog.String("key", k), slog.String("value", v))
	}
	return def
}

// ---------------------------------------------------------------------------
// Logging

// SetupLogging installs a JSON slog handler with the mandatory fields from
// docs/CONTRACTS.md §9. Fluent Bit ships stdout to CloudWatch; anything not in
// this shape is unqueryable once it gets there.
func SetupLogging(c Config) {
	level := slog.LevelInfo
	if c.Env == "local" || os.Getenv("SOUQ_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			return a
		},
	})

	slog.SetDefault(slog.New(h).With(
		slog.String("service", c.ServiceName),
		slog.String("version", c.Version),
		slog.String("env", c.Env),
	))
}

// ---------------------------------------------------------------------------
// Request context

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxCorrelationID
	ctxUserID
	ctxRoles
)

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxCorrelationID, id)
}

func CorrelationIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxCorrelationID).(string); ok {
		return v
	}
	return RequestIDFrom(ctx)
}

func WithUser(ctx context.Context, userID string, roles []string) context.Context {
	return context.WithValue(context.WithValue(ctx, ctxUserID, userID), ctxRoles, roles)
}

func UserIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUserID).(string); ok {
		return v
	}
	return ""
}

func RolesFrom(ctx context.Context) []string {
	if v, ok := ctx.Value(ctxRoles).([]string); ok {
		return v
	}
	return nil
}

func HasRole(ctx context.Context, role string) bool {
	for _, r := range RolesFrom(ctx) {
		if r == role {
			return true
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Metrics

var (
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_requests_total",
		Help: "HTTP requests by route, method and status class.",
	}, []string{"route", "method", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_server_requests_seconds",
		Help:    "HTTP request duration.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"route", "method"})

	SagaTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_saga_transitions_total",
		Help: "Saga state transitions by from-state, trigger and to-state.",
	}, []string{"from", "trigger", "to"})

	SagaDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "souq_saga_duration_seconds",
		Help:    "Time from order acceptance to a terminal saga state.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 300},
	}, []string{"outcome"})

	// If this is ever non-zero, docs/DESIGN-INVARIANTS.md §1 has been violated in
	// production. It is wired to a page, not a dashboard.
	SagaIllegalTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_saga_illegal_transitions_total",
		Help: "Transitions the state machine refused. Should always be zero.",
	}, []string{"from", "trigger"})

	SagaStuck = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "souq_saga_stuck_orders",
		Help: "Orders past their deadline in a non-terminal state.",
	})

	OutboxBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "souq_outbox_unpublished",
		Help: "Rows in the outbox awaiting publication. Sustained growth means the relay is losing.",
	})

	OutboxPublishLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "souq_outbox_publish_lag_seconds",
		Help:    "Time between an outbox row being written and published.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 15, 60},
	})

	EventsConsumed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_events_consumed_total",
		Help: "Kafka messages consumed by topic and outcome (applied/duplicate/dlq).",
	}, []string{"topic", "outcome"})
)

// ---------------------------------------------------------------------------
// Health

// Health tracks readiness. Liveness is deliberately dumb — it never touches a
// dependency, because a database blip must not cause Kubernetes to restart
// every pod at once and turn a brownout into an outage.
type Health struct {
	checks map[string]func(context.Context) error
	ready  bool
}

func NewHealth() *Health {
	return &Health{checks: map[string]func(context.Context) error{}}
}

func (h *Health) Register(name string, check func(context.Context) error) {
	h.checks[name] = check
}

// SetReady flips the readiness gate. Set false on SIGTERM so the load balancer
// drains this pod before the process stops accepting connections.
func (h *Health) SetReady(v bool) { h.ready = v }

func (h *Health) LiveHandler(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, map[string]string{"status": "UP"})
}

func (h *Health) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if !h.ready {
		WriteJSON(w, r, http.StatusServiceUnavailable,
			map[string]any{"status": "DOWN", "reason": "draining"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	results := make(map[string]string, len(h.checks))
	healthy := true
	for name, check := range h.checks {
		if err := check(ctx); err != nil {
			results[name] = "DOWN: " + err.Error()
			healthy = false
			continue
		}
		results[name] = "UP"
	}

	status := http.StatusOK
	body := "UP"
	if !healthy {
		status = http.StatusServiceUnavailable
		body = "DOWN"
	}
	WriteJSON(w, r, status, map[string]any{"status": body, "checks": results})
}
