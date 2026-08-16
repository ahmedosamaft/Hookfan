// Command hookfan is a webhook fan-out gateway: it receives webhooks from
// providers such as the Meta / WhatsApp Cloud API, persists them, and forwards
// them to registered backend services with retries and delivery tracking.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aosama/hookfan/internal/api"
	"github.com/aosama/hookfan/internal/config"
	"github.com/aosama/hookfan/internal/crypto"
	"github.com/aosama/hookfan/internal/dispatch"
	"github.com/aosama/hookfan/internal/ingest"
	"github.com/aosama/hookfan/internal/planner"
	"github.com/aosama/hookfan/internal/retention"
	"github.com/aosama/hookfan/internal/store"
)

func main() {
	// `hookfan migrate <up|down|status>` operates on the schema and exits;
	// with no arguments the gateway starts normally (migrating on the way up).
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := runMigrate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

// runMigrate handles the migrate subcommand. It needs only DATABASE_URL, so it
// works even when the other required settings are absent.
func runMigrate(args []string) error {
	sub := "up"
	if len(args) > 0 {
		sub = args[0]
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required for `hookfan migrate`")
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "up":
		return store.Migrate(ctx, dsn, log)
	case "down":
		// Rolls back one migration. Destructive, so it is never automatic.
		return store.MigrateDown(ctx, dsn, log)
	case "status":
		return store.MigrationStatus(ctx, dsn)
	default:
		return fmt.Errorf("unknown migrate subcommand %q; expected up, down, or status", sub)
	}
}

// writeTimeout applies a per-request write deadline to every route except the
// SSE stream, which is long-lived by design.
//
// A server-wide http.Server.WriteTimeout cannot express that exception: it
// would disconnect streaming clients on a fixed interval regardless of health.
func writeTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/stream" {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		// Best effort: a connection type without deadline support just runs
		// without one rather than failing the request.
		_ = rc.SetWriteDeadline(time.Now().Add(d))
		next.ServeHTTP(w, r)
	})
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Printed plain rather than logged as JSON: this is a multi-line list
		// of every configuration mistake, and JSON-escaping the newlines would
		// make it unreadable exactly when the operator needs it most.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	log.Info("starting hookfan",
		"port", cfg.Port,
		"workers", cfg.WorkerConcur,
		"retention_days", cfg.RetentionDays,
		"allow_private_targets", cfg.AllowPrivate,
		"allowed_origins", cfg.AllowedOrigins,
	)

	// Signals cancel the root context, which unwinds startup and shutdown alike.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, cfg.DatabaseURL, log); err != nil {
		return err
	}

	st, err := store.New(ctx, cfg.DatabaseURL, int32(cfg.WorkerConcur)+8)
	if err != nil {
		return err
	}
	defer st.Close()

	cipher, err := crypto.NewCipher(cfg.SecretKey)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()

	// --- Public: no auth. Providers call these unauthenticated. ---
	health := &api.Health{Store: st, Log: log}
	mux.HandleFunc("GET /healthz", health.Healthz)
	mux.HandleFunc("GET /readyz", health.Readyz)

	guard := dispatch.NewSSRFGuard(cfg.AllowPrivate)

	// Broadcasts live activity to the UI. Publishing never blocks, so a slow
	// browser cannot stall ingest or delivery.
	broker := api.NewBroker(log)
	defer broker.Close()

	// The planner turns received events into delivery sets off the request
	// goroutine, so ingest stays a single INSERT.
	plan := planner.New(st, log)
	dispatcher := dispatch.NewDispatcher(st, cipher, guard, log, cfg.WorkerConcur)
	dispatcher.Stream = broker
	// Newly planned deliveries wake a worker immediately instead of waiting
	// for its next poll tick.
	plan.OnPlanned = func(int) { dispatcher.Notify() }

	ingestHandler := &ingest.Handler{
		Store: st, Cipher: cipher, Log: log, Notify: plan, Stream: broker,
	}
	mux.HandleFunc("GET /hooks/{slug}", ingestHandler.Challenge)
	mux.HandleFunc("POST /hooks/{slug}", ingestHandler.Receive)

	// --- Admin: bearer token, CORS-restricted. ---
	verifier := dispatch.NewVerifier(guard)

	adminMux := http.NewServeMux()
	listeners := &api.Listeners{Store: st, Cipher: cipher, Log: log}
	adminMux.HandleFunc("GET /api/listeners", listeners.List)
	adminMux.HandleFunc("POST /api/listeners", listeners.Create)
	adminMux.HandleFunc("GET /api/listeners/{id}", listeners.Get)
	adminMux.HandleFunc("PATCH /api/listeners/{id}", listeners.Update)
	adminMux.HandleFunc("DELETE /api/listeners/{id}", listeners.Delete)

	services := &api.Services{
		Store: st, Cipher: cipher, Verifier: verifier, Guard: guard, Log: log,
	}
	adminMux.HandleFunc("GET /api/services", services.List)
	adminMux.HandleFunc("POST /api/services", services.Create)
	adminMux.HandleFunc("GET /api/services/{id}", services.Get)
	adminMux.HandleFunc("PATCH /api/services/{id}", services.Update)
	adminMux.HandleFunc("DELETE /api/services/{id}", services.Delete)
	adminMux.HandleFunc("POST /api/services/{id}/verify", services.Verify)
	adminMux.HandleFunc("POST /api/services/{id}/rotate-token", services.RotateToken)

	subscriptions := &api.Subscriptions{Store: st, Log: log}
	adminMux.HandleFunc("GET /api/subscriptions", subscriptions.List)
	adminMux.HandleFunc("POST /api/subscriptions", subscriptions.Create)
	adminMux.HandleFunc("PATCH /api/subscriptions/{id}", subscriptions.Update)
	adminMux.HandleFunc("DELETE /api/subscriptions/{id}", subscriptions.Delete)

	deliveries := &api.Deliveries{Store: st, Dispatcher: dispatcher, Log: log}
	adminMux.HandleFunc("POST /api/deliveries/{id}/retry", deliveries.Retry)

	events := &api.Events{Store: st, Planner: plan, Log: log}
	adminMux.HandleFunc("GET /api/events", events.List)
	adminMux.HandleFunc("GET /api/events/{id}", events.Get)
	adminMux.HandleFunc("POST /api/events/{id}/replay", events.Replay)
	adminMux.HandleFunc("GET /api/stats", events.Stats)

	adminMux.HandleFunc("GET /api/stream", broker.Stream)

	mux.Handle("/api/", api.CORS(cfg.AllowedOrigins,
		api.RequireAuth(cfg.AdminToken, log, adminMux)))

	go plan.Run(ctx)
	go dispatcher.Run(ctx)
	go (&retention.Job{Store: st, Log: log, RetentionDays: cfg.RetentionDays}).Run(ctx)

	srv := &http.Server{
		Addr: cfg.Addr(),
		// Every route except the SSE stream carries its own write deadline;
		// see writeTimeout below.
		Handler: writeTimeout(30*time.Second, mux),
		// Generous relative to the sub-10ms ingest path, but the provider is on
		// the public internet and may be slow to send a body.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No server-wide WriteTimeout: it would sever the SSE stream mid-flight
		// at whatever value it was set to, since a long-lived stream never
		// finishes writing.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
		return srv.Close()
	}
	log.Info("shutdown complete")
	return nil
}
