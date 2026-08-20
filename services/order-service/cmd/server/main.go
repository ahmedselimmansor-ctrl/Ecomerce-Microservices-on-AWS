// Command server runs order-service: the HTTP API, the saga consumer, the
// outbox relay and the timeout sweeper, in one process.
//
// They are one binary rather than four because they share a database
// connection pool and a saga state machine, and splitting them would mean
// versioning an internal contract between components that always deploy
// together. They are separate goroutines with independent lifecycles, so a
// wedged relay does not stop the API serving reads.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/souq/order-service/internal/eventbus"
	"github.com/souq/order-service/internal/httpapi"
	"github.com/souq/order-service/internal/orchestrator"
	"github.com/souq/order-service/internal/platform"
	"github.com/souq/order-service/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := platform.LoadConfig("order-service")
	if err != nil {
		// Before logging is configured, so write plainly.
		return err
	}
	platform.SetupLogging(cfg)

	slog.Info("starting",
		slog.String("addr", cfg.HTTPAddr),
		slog.Any("brokers", cfg.KafkaBrokers))

	// Cancelled on SIGTERM. Everything below hangs off it.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	bootCtx, cancelBoot := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancelBoot()

	db, err := store.Open(bootCtx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBStatementTimeout)
	if err != nil {
		return err
	}
	defer db.Close()

	publisher := eventbus.NewKafkaPublisher(cfg.KafkaBrokers)
	defer publisher.Close()

	orch := orchestrator.New(db)
	relay := eventbus.NewRelay(db, publisher, cfg.OutboxPollInterval, cfg.OutboxBatchSize)
	sweeper := orchestrator.NewSweeper(orch, db, cfg.SweeperInterval)

	consumer := eventbus.NewConsumer(eventbus.ConsumerConfig{
		Brokers: cfg.KafkaBrokers,
		Topics:  []string{eventbus.TopicInventoryEvents, eventbus.TopicPaymentEvents},
		GroupID: cfg.ConsumerGroup,
		// Only the events the saga reacts to. Filtering on the header means we
		// never pay to deserialise a stock_changed event, which is by far the
		// highest-volume message on the inventory topic.
		AcceptTypes: map[string]bool{
			"souq.inventory.reserved.v1":           true,
			"souq.inventory.reservation_failed.v1": true,
			"souq.inventory.released.v1":           true,
			"souq.inventory.committed.v1":          true,
			"souq.payment.authorized.v1":           true,
			"souq.payment.failed.v1":               true,
			"souq.payment.captured.v1":             true,
			"souq.payment.voided.v1":               true,
		},
	}, publisher)
	defer consumer.Close()

	health := platform.NewHealth()
	health.Register("postgres", db.Ping)
	health.Register("kafka", func(ctx context.Context) error {
		// Consumer lag is a readiness signal, not just a metric: a pod that
		// has fallen minutes behind should not be taking new checkout traffic
		// on top of the backlog it already owes.
		if lag := consumer.Lag(); lag > 10_000 {
			return errors.New("consumer lag is too high")
		}
		return nil
	})

	verifier := platform.NewJWKSVerifier(
		os.Getenv("SOUQ_JWKS_URL"),
		platformEnv("SOUQ_JWT_ISSUER", "https://auth.souq.dev"),
		platformEnv("SOUQ_JWT_AUDIENCE", "souq-api"),
	)
	verifier.Prime(bootCtx)

	api := httpapi.New(orch, db)
	router := buildRouter(api, health, verifier)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0, // 0 because of the SSE stream endpoint
		IdleTimeout:       120 * time.Second,
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); relay.Run(rootCtx) }()

	wg.Add(1)
	go func() { defer wg.Done(); sweeper.Run(rootCtx) }()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := consumer.Run(rootCtx, orch.Handle); err != nil {
			slog.Error("consumer exited", slog.String("error", err.Error()))
		}
	}()

	wg.Add(1)
	go func() { defer wg.Done(); runJanitor(rootCtx, db) }()

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

	// Graceful shutdown, in this order for a reason:
	//
	//  1. Fail readiness FIRST. The load balancer stops sending new requests
	//     while the pod is still perfectly able to finish the ones it has.
	//     Skipping this drops in-flight checkouts on every deploy.
	//  2. Wait out the LB's deregistration delay.
	//  3. Drain HTTP.
	//  4. Let the background workers finish; the root context is already
	//     cancelled, so they are winding down in parallel.
	slog.Info("shutdown signal received; draining")
	health.SetReady(false)

	time.Sleep(3 * time.Second)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancelDrain()

	if err := srv.Shutdown(drainCtx); err != nil {
		slog.Error("http drain timed out", slog.String("error", err.Error()))
	}

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

func platformEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func buildRouter(api *httpapi.API, health *platform.Health, v platform.TokenVerifier) chi.Router {
	r := chi.NewRouter()

	r.Use(platform.RequestID)
	r.Use(platform.Recover)

	// Probes and metrics sit outside the timeout and auth middleware. A
	// liveness probe that can be made to fail by a slow dependency causes
	// Kubernetes to restart every pod at once and turns a brownout into an
	// outage.
	r.Get("/health/live", health.LiveHandler)
	r.Get("/health/ready", health.ReadyHandler)
	r.Handle("/metrics", promhttp.Handler())

	r.Group(func(r chi.Router) {
		r.Use(platform.Timeout(10 * time.Second))
		api.Mount(r, platform.Authenticate(v))
	})

	return r
}

// runJanitor trims the tables that grow without bound. Hourly rather than
// nightly so a single run never has to delete a full day of rows and hold
// locks while it does.
func runJanitor(ctx context.Context, db *store.Store) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := db.PurgePublished(ctx, 7*24*time.Hour); err == nil && n > 0 {
				slog.Info("janitor purged published outbox rows", slog.Int64("rows", n))
			}
			if n, err := db.PurgeProcessed(ctx, 30*24*time.Hour); err == nil && n > 0 {
				slog.Info("janitor purged inbox rows", slog.Int64("rows", n))
			}
			if n, err := db.PurgeExpiredKeys(ctx); err == nil && n > 0 {
				slog.Info("janitor purged idempotency keys", slog.Int64("rows", n))
			}
		}
	}
}
