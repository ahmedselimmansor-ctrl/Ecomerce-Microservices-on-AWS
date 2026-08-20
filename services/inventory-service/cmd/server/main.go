// Command server runs inventory-service: the HTTP API, the saga command
// consumer, the outbox relay and the reservation TTL sweeper, in one process.
//
// Read docs/DESIGN-INVARIANTS.md §2 and §3 before changing internal/stock.
// The safe reservation strategy and the broken one differ by about ten
// characters of SQL, and the broken one passes any test that only checks
// `reserved <= on_hand`.
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

	"github.com/souq/inventory-service/internal/eventbus"
	"github.com/souq/inventory-service/internal/httpapi"
	"github.com/souq/inventory-service/internal/platform"
	"github.com/souq/inventory-service/internal/stock"
	"github.com/souq/inventory-service/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := platform.LoadConfig("inventory-service")
	if err != nil {
		return err
	}
	platform.SetupLogging(cfg)

	slog.Info("starting", slog.String("addr", cfg.HTTPAddr), slog.Any("brokers", cfg.KafkaBrokers))

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	bootCtx, cancelBoot := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancelBoot()

	db, err := store.Open(bootCtx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBStatementTimeout)
	if err != nil {
		return err
	}
	defer db.Close()

	publisher := eventbus.NewPublisher(cfg.KafkaBrokers)
	defer publisher.Close()

	engine := stock.New(db)
	relay := eventbus.NewRelay(db, publisher, cfg.OutboxPollInterval, cfg.OutboxBatchSize)
	consumer := eventbus.NewConsumer(cfg.KafkaBrokers, cfg.ConsumerGroup, publisher, engine, db)
	defer consumer.Close()

	health := platform.NewHealth()
	health.Register("postgres", db.Ping)
	health.Register("kafka", func(context.Context) error {
		// Lag is a readiness signal, not just a metric: a pod minutes behind
		// should not take new traffic on top of the backlog it already owes.
		if consumer.Lag() > 10_000 {
			return errors.New("consumer lag is too high")
		}
		return nil
	})

	api := httpapi.New(engine, db)

	r := chi.NewRouter()
	r.Use(platform.RequestID)
	r.Use(platform.Recover)

	// Probes and metrics sit outside the timeout middleware. A liveness probe
	// a slow dependency can fail restarts every pod at once.
	r.Get("/health/live", health.LiveHandler)
	r.Get("/health/ready", health.ReadyHandler)
	r.Handle("/metrics", promhttp.Handler())

	r.Group(func(r chi.Router) {
		r.Use(platform.Timeout(5 * time.Second))
		api.Mount(r)
	})

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
	go func() { defer wg.Done(); relay.Run(rootCtx) }()

	wg.Add(1)
	go func() { defer wg.Done(); runSweeper(rootCtx, engine, cfg.SweepInterval) }()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := consumer.Run(rootCtx); err != nil {
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

	// Shutdown order matters. Readiness is failed FIRST so the load balancer
	// stops sending new requests while this pod is still perfectly able to
	// finish the ones it has. Skipping that drops in-flight reservations on
	// every deploy.
	slog.Info("shutdown signal received; draining")
	health.SetReady(false)
	time.Sleep(3 * time.Second) // outlive the ALB's deregistration delay

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

// runSweeper releases reservations past their TTL.
//
// Defence in depth, not the primary mechanism — the saga releases explicitly
// and the state-space model proves termination without this. It exists because
// "the saga released it" assumes the saga is running, and that assumption
// fails during an incident, which is exactly when stock is most contended.
func runSweeper(ctx context.Context, engine *stock.Engine, interval time.Duration) {
	slog.InfoContext(ctx, "reservation TTL sweeper started", slog.Duration("interval", interval))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "reservation TTL sweeper stopping")
			return
		case <-ticker.C:
			n, err := engine.SweepExpired(ctx, 100)
			if err != nil {
				if ctx.Err() == nil {
					slog.ErrorContext(ctx, "sweeper tick failed", slog.String("error", err.Error()))
				}
				continue
			}
			if n > 0 {
				platform.TTLExpiries.Add(float64(n))
				slog.WarnContext(ctx, "released expired reservations",
					slog.Int("count", n),
					slog.String("hint", "the saga should have released these; check order-service"))
			}
		}
	}
}

// runJanitor trims the tables that grow without bound. Hourly rather than
// nightly so no single run has to delete a full day of rows while holding locks.
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
		}
	}
}
