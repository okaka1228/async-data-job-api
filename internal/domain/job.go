package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Job statuses
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Job represents an async data processing job.
type Job struct {
	ID             uuid.UUID  `json:"id"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	Status         string     `json:"status"`
	InputURL       string     `json:"input_url"`
	TotalRows      int64      `json:"total_rows"`
	ProcessedRows  int64      `json:"processed_rows"`
	Retries        int        `json:"retries"`
	MaxRetries     int        `json:"max_retries"`
	ErrorMessage   string     `json:"error_message,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// CreateJobRequest is the input for creating a new job.
type CreateJobRequest struct {
	InputURL       string `json:"input_url"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	MaxRetries     *int   `json:"max_retries,omitempty"`
}

// Validate validates the create job request.
func (r *CreateJobRequest) Validate() error {
	if r.InputURL == "" {
		return errors.New("input_url is required")
	}
	if len(r.InputURL) > 2048 {
		return errors.New("input_url must be at most 2048 characters")
	}
	if r.MaxRetries != nil && *r.MaxRetries < 0 {
		return errors.New("max_retries must be non-negative")
	}
	if r.IdempotencyKey != "" && len(r.IdempotencyKey) > 255 {
		return errors.New("idempotency_key must be at most 255 characters")
	}
	return nil
}

// IsTerminal returns true if the job is in a terminal state.
func (j *Job) IsTerminal() bool {
	return j.Status == StatusSucceeded || j.Status == StatusFailed || j.Status == StatusCanceled
}

// CanCancel returns true if the job can be canceled from its current state.
func (j *Job) CanCancel() bool {
	return j.Status == StatusPending || j.Status == StatusRunning
}

// ListJobsParams holds query parameters for listing jobs.
type ListJobsParams struct {
	Status string
	Limit  int
	Offset int
}

// JobResult is an enriched response for API output.
type JobResult struct {
	Job
	DurationMs *int64 `json:"duration_ms,omitempty"`
}

// FailedJobEntry represents an entry in the DLQ (dead-letter queue) table.
type FailedJobEntry struct {
	ID           uuid.UUID `json:"id"`
	JobID        uuid.UUID `json:"job_id"`
	ErrorMessage string    `json:"error_message"`
	Attempt      int       `json:"attempt"`
	CreatedAt    time.Time `json:"created_at"`
}
