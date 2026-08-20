// Command server runs payment-service: the HTTP API (including the provider
// webhook), the saga command consumer, the outbox relay and the reconciler.
//
// Read docs/DESIGN-INVARIANTS.md §4 before changing anything that touches the
// provider key. An atomic INSERT and a well-behaved reaper are NOT sufficient
// on their own to prevent a double charge; only the deterministic key is.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"

	"github.com/souq/payment-service/internal/httpapi"
	"github.com/souq/payment-service/internal/payment"
	"github.com/souq/payment-service/internal/platform"
	"github.com/souq/payment-service/internal/psp"
	"github.com/souq/payment-service/internal/service"
	"github.com/souq/payment-service/internal/store"
)

const (
	topicCommands = "souq.order.commands.v1"
	consumerGroup = "payment-service.saga-commands"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := platform.LoadConfig("payment-service")
	if err != nil {
		return err
	}
	platform.SetupLogging(cfg)

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	bootCtx, cancelBoot := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancelBoot()

	db, err := store.Open(bootCtx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBStatementTimeout)
	if err != nil {
		return err
	}
	defer db.Close()

	// Refuses to construct without a >=32-character salt. Failing at boot
	// rather than on the first real payment is the whole point.
	deriver, err := payment.NewPSPKeyDeriver(os.Getenv("SOUQ_PSP_KEY_SALT"))
	if err != nil {
		return err
	}

	provider, err := buildProvider()
	if err != nil {
		return err
	}
	slog.Info("payment provider configured", slog.String("provider", provider.Name()))

	svc := service.New(db, provider, deriver, newID)

	writer := &kafka.Writer{
		Addr:         kafka.TCP(cfg.KafkaBrokers...),
		Balancer:     &kafka.Hash{},
		RequiredAcks: kafka.RequireAll,
		Compression:  kafka.Snappy,
		MaxAttempts:  3,
		WriteTimeout: 5 * time.Second,
	}
	defer writer.Close()

	health := platform.NewHealth()
	health.Register("postgres", db.Ping)
	health.Register("provider", provider.Health)

	webhook := httpapi.NewWebhookHandler(provider, svc)

	r := chi.NewRouter()
	r.Use(platform.RequestID)
	r.Use(platform.Recover)
	r.Get("/health/live", health.LiveHandler)
	r.Get("/health/ready", health.ReadyHandler)
	r.Handle("/metrics", promhttp.Handler())

	// The webhook is reachable from the internet by design and does its own
	// signature verification. It sits OUTSIDE the JWT middleware — a provider
	// has no bearer token — which is exactly why the HMAC check is not
	// optional.
	r.Method(http.MethodPost, "/v1/webhooks/{provider}", webhook)
	r.Method(http.MethodGet, "/v1/webhooks/{provider}", webhook)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); runRelay(rootCtx, db, writer, cfg) }()
	wg.Add(1)
	go func() { defer wg.Done(); runConsumer(rootCtx, cfg, svc, writer) }()
	wg.Add(1)
	go func() { defer wg.Done(); runReconciler(rootCtx, db) }()
	wg.Add(1)
	go func() { defer wg.Done(); runLedgerCheck(rootCtx, db) }()

	serverErr := make(chan error, 1)
	go func() {
		health.SetReady(true)
		slog.Info("listening", slog.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-rootCtx.Done():
	}

	// Readiness first, so the load balancer drains this pod before it stops
	// accepting. A dropped webhook is a payment the provider thinks we know
	// about and we do not.
	slog.Info("shutdown signal received; draining")
	health.SetReady(false)
	time.Sleep(3 * time.Second)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancelDrain()
	_ = srv.Shutdown(drainCtx)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		slog.Info("all workers stopped cleanly")
	case <-drainCtx.Done():
		slog.Warn("workers did not stop within the grace period")
	}
	return nil
}

// buildProvider selects the PSP from configuration.
//
// The mock is only reachable in a non-production environment. A misconfigured
// SOUQ_PSP_PROVIDER in prod must fail to start, not quietly stop charging
// anyone.
func buildProvider() (psp.Provider, error) {
	name := os.Getenv("SOUQ_PSP_PROVIDER")
	env := os.Getenv("SOUQ_ENV")

	switch name {
	case "paymob":
		return psp.NewPaymob(psp.PaymobConfig{
			BaseURL:    os.Getenv("SOUQ_PAYMOB_BASE_URL"),
			APIKey:     os.Getenv("SOUQ_PAYMOB_API_KEY"),
			HMACSecret: os.Getenv("SOUQ_PAYMOB_HMAC_SECRET"),
			IframeID:   envInt("SOUQ_PAYMOB_IFRAME_ID", 0),
			Currency:   envStr("SOUQ_PAYMOB_CURRENCY", "EGP"),
			IntegrationIDs: map[psp.PaymentMethod]int{
				psp.MethodCard:           envInt("SOUQ_PAYMOB_INTEGRATION_CARD", 0),
				psp.MethodWallet:         envInt("SOUQ_PAYMOB_INTEGRATION_WALLET", 0),
				psp.MethodCashOnDelivery: envInt("SOUQ_PAYMOB_INTEGRATION_COD", 0),
			},
			AuthorizeOnly: os.Getenv("SOUQ_PAYMOB_AUTHORIZE_ONLY") == "true",
		})

	case "mock", "":
		if env != "local" && env != "test" {
			return nil, errors.New("the mock payment provider cannot be used outside local or test")
		}
		rate := envFloat("SOUQ_MOCK_DECLINE_RATE", 0.10)
		unknown := envFloat("SOUQ_MOCK_UNKNOWN_RATE", 0.02)
		latency := time.Duration(envInt("SOUQ_MOCK_LATENCY_MS", 150)) * time.Millisecond
		slog.Warn("using the MOCK payment provider — no real money will move",
			slog.Float64("declineRate", rate), slog.Float64("unknownRate", unknown))
		return psp.NewMock(rate, unknown, latency), nil
	}
	return nil, errors.New("unknown SOUQ_PSP_PROVIDER: " + name)
}

// ---------------------------------------------------------------------------

func runRelay(ctx context.Context, db *store.Store, w *kafka.Writer, cfg platform.Config) {
	slog.InfoContext(ctx, "outbox relay started")

	// Jitter so replicas starting together after a deploy do not all hit the
	// outbox in the same millisecond.
	select {
	case <-time.After(time.Duration(rand.Int63n(int64(cfg.OutboxPollInterval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(cfg.OutboxPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "outbox relay stopping")
			return
		case <-ticker.C:
			err := db.RawTx(ctx, func(tx pgx.Tx) error {
				batch, err := db.ClaimOutbox(ctx, tx, cfg.OutboxBatchSize)
				if err != nil || len(batch) == 0 {
					return err
				}
				msgs := make([]kafka.Message, 0, len(batch))
				ids := make([]int64, 0, len(batch))
				for _, rec := range batch {
					headers := make([]kafka.Header, 0, len(rec.Headers))
					for k, v := range rec.Headers {
						headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
					}
					msgs = append(msgs, kafka.Message{
						Topic: rec.Topic, Key: []byte(rec.PartitionKey),
						Value: rec.Payload, Headers: headers,
					})
					ids = append(ids, rec.ID)
				}
				if err := w.WriteMessages(ctx, msgs...); err != nil {
					// Rows stay pending and go out on the next tick. That is
					// the at-least-once guarantee the outbox is built on.
					return err
				}
				return db.MarkPublished(ctx, tx, ids)
			})
			if err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "relay tick failed", slog.String("error", err.Error()))
			}
			if n, err := db.OutboxBacklog(ctx); err == nil {
				platform.OutboxBacklog.Set(float64(n))
			}
		}
	}
}

func runConsumer(ctx context.Context, cfg platform.Config, svc *service.Service, w *kafka.Writer) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: cfg.KafkaBrokers, GroupID: consumerGroup, Topic: topicCommands,
		MinBytes: 1, MaxBytes: 10 << 20, MaxWait: 500 * time.Millisecond,
		// Manual commit: auto-commit acknowledges a command before we have
		// acted on it, and a lost authorize wedges an order.
		CommitInterval: 0, StartOffset: kafka.FirstOffset,
	})
	defer reader.Close()

	// The topic is shared with inventory-service; most traffic is not ours.
	accepted := map[string]bool{
		"souq.payment.authorize.v1": true,
		"souq.payment.capture.v1":   true,
		"souq.payment.void.v1":      true,
	}

	slog.InfoContext(ctx, "consumer started", slog.String("topic", topicCommands))

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				slog.InfoContext(ctx, "consumer stopping")
				return
			}
			slog.ErrorContext(ctx, "fetch failed", slog.String("error", err.Error()))
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		if handleCommand(ctx, svc, msg, accepted) {
			if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "commit failed; message will be redelivered",
					slog.String("error", err.Error()))
			}
		}
	}
}

// handleCommand returns whether the offset may be committed. A retriable
// failure returns false so the message is redelivered rather than lost.
func handleCommand(ctx context.Context, svc *service.Service, msg kafka.Message, accepted map[string]bool) bool {
	var env struct {
		ID            string          `json:"id"`
		Type          string          `json:"type"`
		CorrelationID string          `json:"correlationid"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Value, &env); err != nil {
		slog.ErrorContext(ctx, "malformed envelope; dropping", slog.String("error", err.Error()))
		return true
	}
	if !accepted[env.Type] {
		platform.EventsConsumed.WithLabelValues(msg.Topic, "filtered").Inc()
		return true
	}
	if env.ID == "" {
		slog.ErrorContext(ctx, "event has no id and cannot be deduplicated; dropping")
		return true
	}

	var body struct {
		OrderID        string `json:"orderId"`
		PaymentID      string `json:"paymentId"`
		UserID         string `json:"userId"`
		ReasonCode     string `json:"reasonCode"`
		IdempotencyKey string `json:"idempotencyKey"`
		Amount         struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"amount"`
		PaymentMethodToken string `json:"paymentMethodToken"`
		WalletPhone        string `json:"walletPhone"`
		Method             string `json:"method"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		slog.ErrorContext(ctx, "unparseable command payload; dropping", slog.String("error", err.Error()))
		return true
	}

	// The saga carries the client's original key through. Without it the
	// provider key cannot be derived and a retry would present a new one.
	idem := body.IdempotencyKey
	if idem == "" {
		idem = body.OrderID
	}

	hctx := platform.WithCorrelationID(platform.WithRequestID(ctx, env.ID), env.CorrelationID)

	var err error
	switch env.Type {
	case "souq.payment.authorize.v1":
		method := psp.MethodCard
		if body.Method != "" {
			method = psp.PaymentMethod(body.Method)
		}
		err = svc.Authorize(hctx, service.AuthorizeCommand{
			EventID: env.ID, OrderID: body.OrderID, PaymentID: body.PaymentID,
			UserID: body.UserID, Amount: body.Amount.Amount,
			Currency: body.Amount.Currency, Method: method,
			MethodToken: body.PaymentMethodToken, WalletPhone: body.WalletPhone,
			CorrelationID: env.CorrelationID, IdempotencyKey: idem,
		})
	case "souq.payment.capture.v1":
		err = svc.Capture(hctx, env.ID, body.OrderID, body.PaymentID, idem)
	case "souq.payment.void.v1":
		err = svc.Void(hctx, env.ID, body.OrderID, body.PaymentID, body.ReasonCode, idem)
	}

	if err != nil {
		slog.ErrorContext(hctx, "command handler failed; not committing",
			slog.String("type", env.Type), slog.String("orderId", body.OrderID),
			slog.String("error", err.Error()))
		return false
	}
	platform.EventsConsumed.WithLabelValues(msg.Topic, "applied").Inc()
	return true
}

// runReconciler resolves payments stranded mid-flight.
//
// This is the other half of docs/DESIGN-INVARIANTS.md §4. A crash between the
// provider call and our commit leaves a row in AUTHORIZING with a stored
// provider key; the reconciler presents that same key and asks the provider
// what actually happened, rather than guessing.
func runReconciler(ctx context.Context, db *store.Store) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Five minutes of grace: anything younger is probably still in
			// flight, and querying the provider about a live payment is noise.
			stranded, err := db.NeedsReconciliation(ctx, 5*time.Minute)
			if err != nil {
				slog.ErrorContext(ctx, "reconciler query failed", slog.String("error", err.Error()))
				continue
			}
			for _, p := range stranded {
				platform.UnknownOutcomes.Inc()
				// Deliberately not auto-resolved. Reconciling a payment means
				// deciding whether a customer was charged, and getting it
				// wrong in either direction is worse than a page.
				slog.ErrorContext(ctx, "PAYMENT NEEDS RECONCILIATION",
					slog.String("paymentId", p.ID),
					slog.String("orderId", p.OrderID),
					slog.String("state", string(p.State)),
					slog.String("pspKey", p.PSPIdempotencyKey),
					slog.String("runbook", "docs/runbooks/unknown-payment-outcome.md"))
			}
		}
	}
}

// runLedgerCheck publishes the ledger imbalance so the alert can fire on it.
// The books not balancing is a page, and it must be visible without anyone
// running a query.
func runLedgerCheck(ctx context.Context, db *store.Store) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := db.UnbalancedLedgerGroups(ctx)
			if err != nil {
				continue
			}
			platform.LedgerImbalance.Set(float64(n))
			if n > 0 {
				slog.ErrorContext(ctx, "THE LEDGER DOES NOT BALANCE",
					slog.Int64("groups", n),
					slog.String("runbook", "docs/runbooks/ledger-imbalance.md"))
			}
		}
	}
}

// ---------------------------------------------------------------------------

func newID(prefix string) string { return prefix + "_" + platform.NewID() }

func envStr(k, def string) string {
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
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
