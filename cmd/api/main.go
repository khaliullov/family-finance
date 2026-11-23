package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/account"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/fixture"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
	"github.com/khaliullov/family-finance/internal/transaction"
)

func main() {
	log.Println("[INFO] Starting Family Finance API Server...")

	// 1. Load Authentication Configuration
	authCfg, err := auth.LoadConfigFromEnv()
	if err != nil {
		log.Fatalf("[FATAL] Failed to load authentication configuration: %v", err)
	}

	// 2. Build Database Connection String
	connStr := getDatabaseConnString()

	// 3. Initialize Database Connection Pool with Fail-Closed Security Validation
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := database.NewPool(ctx, connStr)
	cancel()
	if err != nil {
		log.Fatalf("[FATAL] Failed to initialize secure database pool: %v", err)
	}
	defer pool.Close()
	log.Println("[INFO] Database pool successfully initialized and validated (fail-closed tenant isolation active).")

	// 4. Setup Router and Routes
	router, transactionSvc, err := setupRouter(pool, authCfg)
	if err != nil {
		log.Fatalf("[FATAL] Failed to initialize router: %v", err)
	}

	// 5. Start Background Queue Worker
	workerCfg := queue.DefaultWorkerConfig()
	worker := queue.NewWorker(pool, workerCfg)
	if transactionSvc != nil {
		worker.RegisterHandler(queue.JobTypeBankSync, transactionSvc.ProcessBankSyncJob)
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	worker.Start(workerCtx)
	log.Println("[INFO] Background queue worker successfully started.")

	// 6. Configure HTTP Server with strict timeouts
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// 6. Start Server in Background Goroutine
	serverErr := make(chan error, 1)
	go func() {
		log.Printf("[INFO] Family Finance API listening on %s (Environment: %s)", addr, authCfg.Environment)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// 7. Graceful Shutdown on SIGINT / SIGTERM
	shutdownSig := make(chan os.Signal, 1)
	signal.Notify(shutdownSig, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		log.Fatalf("[FATAL] Server failed: %v", err)
	case sig := <-shutdownSig:
		log.Printf("[INFO] Received signal %v; initiating graceful shutdown...", sig)
	}

	// Drain ongoing requests with 10s shutdown timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ERROR] Server graceful shutdown error: %v", err)
	} else {
		log.Println("[INFO] HTTP server drained and stopped cleanly.")
	}

	worker.Stop()
	workerCancel()
	log.Println("[INFO] Background worker stopped cleanly.")

	log.Println("[INFO] Server shutdown complete. Goodbye.")
}

func getDatabaseConnString() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("PGUSER")
	if user == "" {
		user = "family_app"
	}
	pass := os.Getenv("PGPASSWORD")
	if pass == "" {
		pass = "app_secret_placeholder"
	}
	db := os.Getenv("PGDATABASE")
	if db == "" {
		db = "family_finance"
	}
	sslMode := os.Getenv("PGSSLMODE")
	if sslMode == "" {
		sslMode = "disable"
	}

	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user, pass, host, port, db, sslMode)
}

func setupRouter(pool *pgxpool.Pool, authCfg *auth.Config) (http.Handler, *transaction.Service, error) {
	mux := http.NewServeMux()

	// 1. Health and Readiness Endpoints
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "UP",
			"service": "family-finance-api",
		})
	})

	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if pool == nil || pool.Ping(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":   "UNAVAILABLE",
				"database": "DOWN",
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":   "READY",
			"database": "UP",
		})
	})

	// 2. Wire Dependencies
	jwtMgr, err := auth.NewJWTManagerWithLeeway(authCfg.JWTSecret, authCfg.AccessTokenTTL, authCfg.ClockSkew)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize JWT manager: %w", err)
	}

	authRepo := auth.NewRepository(pool)
	authSvc := auth.NewService(authRepo, jwtMgr)
	authHandler := auth.NewHandlerWithRateLimiter(authSvc, nil, authCfg.RateLimits, authCfg.TrustedProxies...)

	accountRepo := account.NewRepository(pool)
	accountSvc := account.NewService(accountRepo)
	accountHandler := account.NewHandler(accountSvc)

	// 3. Ingestion & Bank Connector Registry
	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	transactionSvc := transaction.NewService(pool, registry)
	transactionHandler := transaction.NewHandler(pool, transactionSvc)

	// 4. Register Domain Routes Protected by AuthMiddleware
	authMw := auth.AuthMiddleware(jwtMgr, authRepo)

	authHandler.RegisterRoutes(mux, authMw)
	accountHandler.RegisterRoutes(mux, authMw)
	transactionHandler.RegisterRoutes(mux, authMw)

	return mux, transactionSvc, nil
}
