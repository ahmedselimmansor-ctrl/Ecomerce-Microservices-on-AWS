// Package platform holds the cross-cutting machinery: the error envelope,
// config loading, structured logging, metrics and health.
//
// Deliberately a copy of order-service's rather than a shared module. These
// are ~250 lines that change roughly never, and a shared library across
// independently-deployed services couples their release cycles for very little
// saved. docs/CONTRACTS.md is the thing that keeps them in agreement.
package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// ---------------------------------------------------------------------- config

type Config struct {
	ServiceName        string
	Version            string
	Env                string
	HTTPAddr           string
	DatabaseURL        string
	DBMaxConns         int32
	DBStatementTimeout time.Duration
	KafkaBrokers       []string
	ConsumerGroup      string
	ShutdownGrace      time.Duration
	OutboxPollInterval time.Duration
	OutboxBatchSize    int
	SweepInterval      time.Duration
}

// LoadConfig fails fast on anything missing. A service that boots with half
// its configuration and discovers the gap on the first real request is
// strictly worse than one that refuses to start.
func LoadConfig(serviceName string) (Config, error) {
	c := Config{
		ServiceName:        serviceName,
		Version:            env("SOUQ_VERSION", "dev"),
		Env:                env("SOUQ_ENV", "local"),
		HTTPAddr:           env("SOUQ_HTTP_ADDR", ":8085"),
		DatabaseURL:        os.Getenv("SOUQ_DB_URL"),
		DBMaxConns:         int32(envInt("SOUQ_DB_MAX_CONNS", 20)),
		DBStatementTimeout: envDuration("SOUQ_DB_STATEMENT_TIMEOUT", 3*time.Second),
		ConsumerGroup:      env("SOUQ_CONSUMER_GROUP", "inventory-service.saga-commands"),
		ShutdownGrace:      envDuration("SOUQ_SHUTDOWN_GRACE", 20*time.Second),
		OutboxPollInterval: envDuration("SOUQ_OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		OutboxBatchSize:    envInt("SOUQ_OUTBOX_BATCH_SIZE", 100),
		SweepInterval:      envDuration("SOUQ_SWEEP_INTERVAL", 30*time.Second),
	}
	if b := env("SOUQ_KAFKA_BROKERS", ""); b != "" {
		c.KafkaBrokers = strings.Split(b, ",")
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

// --------------------------------------------------------------------- logging

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

// --------------------------------------------------------------------- context

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxCorrelationID
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

func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --------------------------------------------------------------------- metrics

var (
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_requests_total",
		Help: "HTTP requests by route, method and status class.",
	}, []string{"route", "method", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_server_requests_seconds",
		Help:    "HTTP request duration.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"route", "method"})

	Reservations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_reservations_total",
		Help: "Reservation attempts by outcome.",
	}, []string{"outcome"})

	// A sustained rise here is a merchandising signal — we are selling things
	// we do not have — long before it is an engineering one.
	Stockouts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_stockouts_total",
		Help: "Reservations rejected for insufficient stock, by SKU.",
	}, []string{"sku"})

	// Releases that arrived before their reserve. Expected to be rare but
	// never zero; a spike means order-service is timing out early.
	Tombstones = promauto.NewCounter(prometheus.CounterOpts{
		Name: "souq_reservation_tombstones_total",
		Help: "Releases that arrived before the matching reserve.",
	})

	// Should be near zero: the saga releases explicitly, so this firing means
	// one did not.
	TTLExpiries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "souq_reservation_ttl_expiries_total",
		Help: "Reservations released by the TTL sweeper rather than by the saga.",
	})

	// If this is ever non-zero, docs/DESIGN-INVARIANTS.md §1 has been violated
	// in production. Wired to a page, not a dashboard.
	CompensationAfterCommit = promauto.NewCounter(prometheus.CounterOpts{
		Name: "souq_inventory_release_after_commit_total",
		Help: "Release commands received for an already-committed reservation. Must always be zero.",
	})

	OutboxBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "souq_outbox_unpublished",
		Help: "Rows awaiting publication.",
	})

	EventsConsumed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "souq_events_consumed_total",
		Help: "Kafka messages consumed by topic and outcome.",
	}, []string{"topic", "outcome"})
)

// ---------------------------------------------------------------------- health

type Health struct {
	checks map[string]func(context.Context) error
	ready  bool
}

func NewHealth() *Health { return &Health{checks: map[string]func(context.Context) error{}} }

func (h *Health) Register(name string, check func(context.Context) error) { h.checks[name] = check }

// SetReady flips the readiness gate. Set false on SIGTERM so the load balancer
// drains this pod before the process stops accepting connections.
func (h *Health) SetReady(v bool) { h.ready = v }

// LiveHandler never touches a dependency. A liveness probe a database blip can
// fail restarts every pod at once and turns a brownout into an outage.
func (h *Health) LiveHandler(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, r, http.StatusOK, map[string]string{"status": "UP"})
}

func (h *Health) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if !h.ready {
		WriteJSON(w, r, http.StatusServiceUnavailable, map[string]any{"status": "DOWN", "reason": "draining"})
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

	status, body := http.StatusOK, "UP"
	if !healthy {
		status, body = http.StatusServiceUnavailable, "DOWN"
	}
	WriteJSON(w, r, status, map[string]any{"status": body, "checks": results})
}

// ----------------------------------------------------------- error envelope

// Problem is the RFC 9457 envelope from docs/CONTRACTS.md §2.2. Every service
// in every language emits this shape, so the frontend has one error path.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	Code      string         `json:"code"`
	RequestID string         `json:"requestId"`
	Timestamp string         `json:"timestamp"`
	Extra     map[string]any `json:"-"`
}

const (
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeInsufficientStock  = "INVENTORY_INSUFFICIENT_STOCK"
	CodeReservationMissing = "RESERVATION_NOT_FOUND"
	CodeNotCommittable     = "RESERVATION_NOT_COMMITTABLE"
	CodeAlreadyCommitted   = "RESERVATION_ALREADY_COMMITTED"
	CodeAdjustmentRejected = "STOCK_ADJUSTMENT_REJECTED"
	CodeInternal           = "INTERNAL_ERROR"
)

func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, cause error, extra map[string]any) {
	reqID := RequestIDFrom(r.Context())

	body := map[string]any{
		"type":      "https://errors.souq.dev/inventory/" + slugify(code),
		"title":     titleFor(code),
		"status":    status,
		"code":      code,
		"instance":  r.URL.Path,
		"requestId": reqID,
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	attrs := []any{
		slog.String("code", code), slog.Int("status", status),
		slog.String("path", r.URL.Path), slog.String("requestId", reqID),
	}
	if status >= 500 {
		if cause != nil {
			attrs = append(attrs, slog.String("error", cause.Error()))
		}
		slog.ErrorContext(r.Context(), "request failed", attrs...)
		// Never leak the cause on a 5xx: it can carry SQL, a hostname, or
		// another customer's data from an adjacent row.
		body["detail"] = "The request could not be completed. Quote the requestId to support."
	} else {
		slog.InfoContext(r.Context(), "request rejected", attrs...)
		if detail != "" {
			body["detail"] = detail
		}
	}
	for k, v := range extra {
		body[k] = v
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", RequestIDFrom(r.Context()))
	w.WriteHeader(status)
	if body != nil {
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.ErrorContext(r.Context(), "failed to encode response", slog.String("error", err.Error()))
		}
	}
}

func titleFor(code string) string {
	switch code {
	case CodeValidationFailed:
		return "Validation failed"
	case CodeInsufficientStock:
		return "Insufficient stock"
	case CodeReservationMissing:
		return "Reservation not found"
	case CodeNotCommittable:
		return "Reservation is not in a committable state"
	case CodeAlreadyCommitted:
		return "Reservation is already committed"
	case CodeAdjustmentRejected:
		return "Stock adjustment rejected"
	default:
		return "Internal error"
	}
}

func slugify(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c == '_':
			out = append(out, '-')
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// ------------------------------------------------------------------ middleware

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = NewID()
		}
		ctx := WithRequestID(r.Context(), id)
		if corr := r.Header.Get("X-Correlation-Id"); corr != "" {
			ctx = WithCorrelationID(ctx, corr)
		} else {
			ctx = WithCorrelationID(ctx, id)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Observe records RED metrics. `route` must be the TEMPLATED path, never the
// concrete one, or metric cardinality grows without bound.
func Observe(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			HTTPRequests.WithLabelValues(route, r.Method, statusClass(rec.status)).Inc()
			HTTPDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		})
	}
}

func statusClass(code int) string {
	switch {
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", v), slog.String("path", r.URL.Path))
				WriteProblem(w, r, http.StatusInternalServerError, CodeInternal, "", nil, nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"code":"UPSTREAM_TIMEOUT","status":504}`)
	}
}

var ErrShuttingDown = errors.New("shutting down")
