package api

import (
	"log/slog"
	"net/http"

	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/femto/async-data-job-api/internal/repository"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRouter creates a chi router with all routes and middleware.
func NewRouter(
	repo repository.JobRepository,
	q queue.Queue,
	metrics *observability.Metrics,
	logger *slog.Logger,
) http.Handler {
	r := chi.NewRouter()

	// Global middleware
	r.Use(RecoverMiddleware(logger))
	r.Use(RequestIDMiddleware)
	r.Use(LoggingMiddleware(logger))
	r.Use(MetricsMiddleware(metrics))

	handler := NewJobHandler(repo, q, metrics, logger)

	// Health check
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Prometheus metrics
	r.Handle("/metrics", promhttp.Handler())

	// API v1
	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/jobs", func(r chi.Router) {
			r.Post("/", handler.CreateJob)
			r.Get("/", handler.ListJobs)
			r.Get("/{id}", handler.GetJob)
			r.Post("/{id}/cancel", handler.CancelJob)
			r.Post("/{id}/retry", handler.RetryJob)
			r.Get("/{id}/failures", handler.GetJobFailures)
		})
	})

	return r
}
