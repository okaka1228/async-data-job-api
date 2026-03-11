package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/femto/async-data-job-api/internal/repository"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// JobHandler holds dependencies for job-related HTTP handlers.
type JobHandler struct {
	repo    repository.JobRepository
	queue   queue.Queue
	metrics *observability.Metrics
	logger  *slog.Logger
}

// NewJobHandler creates a new JobHandler.
func NewJobHandler(repo repository.JobRepository, q queue.Queue, m *observability.Metrics, logger *slog.Logger) *JobHandler {
	return &JobHandler{repo: repo, queue: q, metrics: m, logger: logger}
}

// CreateJob handles POST /api/v1/jobs
func (h *JobHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	if err := req.Validate(); err != nil {
		Error(w, http.StatusUnprocessableEntity, "validation error", err.Error())
		return
	}

	ctx := r.Context()

	// Idempotency check
	if req.IdempotencyKey != "" {
		existing, err := h.repo.GetByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil {
			h.logger.Error("idempotency lookup failed", "error", err)
			Error(w, http.StatusInternalServerError, "internal error", "")
			return
		}
		if existing != nil {
			JSON(w, http.StatusOK, existing)
			return
		}
	}

	maxRetries := 3
	if req.MaxRetries != nil {
		maxRetries = *req.MaxRetries
	}

	job := &domain.Job{
		InputURL:       req.InputURL,
		IdempotencyKey: req.IdempotencyKey,
		MaxRetries:     maxRetries,
	}

	if err := h.repo.Create(ctx, job); err != nil {
		h.logger.Error("failed to create job", "error", err)
		Error(w, http.StatusInternalServerError, "failed to create job", "")
		return
	}

	// Enqueue
	if err := h.queue.Enqueue(ctx, job.ID.String()); err != nil {
		h.logger.Error("failed to enqueue job", "error", err, "job_id", job.ID)
		// Job is created but not enqueued — it will be picked up by the poller
	}

	h.metrics.JobsCreated.Inc()
	h.logger.Info("job created", "job_id", job.ID)
	JSON(w, http.StatusAccepted, job)
}

// GetJob handles GET /api/v1/jobs/{id}
func (h *JobHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(w, http.StatusBadRequest, "invalid job ID", "must be a valid UUID")
		return
	}

	job, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to get job", "error", err)
		Error(w, http.StatusInternalServerError, "internal error", "")
		return
	}
	if job == nil {
		Error(w, http.StatusNotFound, "job not found", "")
		return
	}

	JSON(w, http.StatusOK, job)
}

// ListJobs handles GET /api/v1/jobs
func (h *JobHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	status := q.Get("status")

	if limit <= 0 {
		limit = 20
	}

	params := domain.ListJobsParams{
		Status: status,
		Limit:  limit,
		Offset: offset,
	}

	jobs, err := h.repo.List(r.Context(), params)
	if err != nil {
		h.logger.Error("failed to list jobs", "error", err)
		Error(w, http.StatusInternalServerError, "internal error", "")
		return
	}

	if jobs == nil {
		jobs = []domain.Job{}
	}

	JSON(w, http.StatusOK, map[string]interface{}{
		"jobs":   jobs,
		"limit":  limit,
		"offset": offset,
	})
}

// CancelJob handles POST /api/v1/jobs/{id}/cancel
func (h *JobHandler) CancelJob(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(w, http.StatusBadRequest, "invalid job ID", "must be a valid UUID")
		return
	}

	ctx := r.Context()
	job, err := h.repo.GetByID(ctx, id)
	if err != nil {
		h.logger.Error("failed to get job for cancel", "error", err)
		Error(w, http.StatusInternalServerError, "internal error", "")
		return
	}
	if job == nil {
		Error(w, http.StatusNotFound, "job not found", "")
		return
	}

	if !job.CanCancel() {
		Error(w, http.StatusConflict, "job cannot be canceled", "current status: "+job.Status)
		return
	}

	updated, err := h.repo.MarkCompleted(ctx, id, domain.StatusCanceled, "canceled by user")
	if err != nil {
		h.logger.Error("failed to cancel job", "error", err)
		Error(w, http.StatusInternalServerError, "failed to cancel job", "")
		return
	}

	if updated {
		h.metrics.JobsCanceled.Inc()
		h.logger.Info("job canceled", "job_id", id)
	}

	job.Status = domain.StatusCanceled
	JSON(w, http.StatusOK, job)
}

// GetJobFailures handles GET /api/v1/jobs/{id}/failures
func (h *JobHandler) GetJobFailures(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		Error(w, http.StatusBadRequest, "invalid job ID", "must be a valid UUID")
		return
	}

	entries, err := h.repo.ListFailedEntries(r.Context(), id)
	if err != nil {
		h.logger.Error("failed to list failures", "error", err)
		Error(w, http.StatusInternalServerError, "internal error", "")
		return
	}
	if entries == nil {
		entries = []domain.FailedJobEntry{}
	}

	JSON(w, http.StatusOK, map[string]interface{}{
		"failures": entries,
	})
}
