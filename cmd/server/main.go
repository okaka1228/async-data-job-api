package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/femto/async-data-job-api/internal/api"
	"github.com/femto/async-data-job-api/internal/config"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/femto/async-data-job-api/internal/repository"
	"github.com/femto/async-data-job-api/internal/worker"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	// Logger
	logLevel := slog.LevelInfo
	cfg := config.Load()
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	// Database
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Wait for DB readiness
	for i := 0; i < 30; i++ {
		if pingErr := db.Ping(); pingErr == nil {
			break
		}
		logger.Info("waiting for database...", "attempt", i+1)
		time.Sleep(1 * time.Second)
	}
	if pingErr := db.Ping(); pingErr != nil {
		logger.Error("database not ready", "error", pingErr)
		os.Exit(1) //nolint:gocritic // expected hard exit on startup failure
	}
	logger.Info("database connected")

	repo := repository.NewJobRepository(db)

	// Observability
	metrics := observability.NewMetrics()
	shutdownTracer, err := observability.InitTracer("async-data-job-api", cfg.OTELEndpoint, logger)
	if err != nil {
		logger.Error("failed to init tracer", "error", err)
		os.Exit(1)
	}

	// Queue & Worker
	q := queue.NewChannelQueue(1000)
	processor := worker.NewProcessor(repo, metrics, logger, cfg.WorkerTimeout, cfg.WorkerMaxRetries)
	pool := worker.NewPool(cfg.WorkerConcurrency, q, processor, logger)
	poller := worker.NewPoller(repo, q, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)
	go poller.Start(ctx)

	// HTTP router
	router := api.NewRouter(repo, q, metrics, logger)

	srv := &http.Server{
		Addr:         ":" + cfg.ServerPort,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server
	go func() {
		logger.Info("server starting", "port", cfg.ServerPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// Stop accepting new requests
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}

	// Stop workers
	cancel()
	pool.Shutdown()

	// Shutdown tracer
	if err := shutdownTracer(shutdownCtx); err != nil {
		logger.Error("tracer shutdown error", "error", err)
	}

	logger.Info("server stopped")
}
