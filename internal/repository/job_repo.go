package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/google/uuid"
)

// JobRepository defines persistence operations for jobs.
type JobRepository interface {
	Create(ctx context.Context, job *domain.Job) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Job, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*domain.Job, error)
	List(ctx context.Context, params domain.ListJobsParams) ([]domain.Job, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status string) (bool, error)
	UpdateProgress(ctx context.Context, id uuid.UUID, processedRows, totalRows int64) error
	MarkCompleted(ctx context.Context, id uuid.UUID, status, errorMsg string) (bool, error)
	IncrementRetry(ctx context.Context, id uuid.UUID) (int, error)
	InsertFailedEntry(ctx context.Context, entry *domain.FailedJobEntry) error
	ListFailedEntries(ctx context.Context, jobID uuid.UUID) ([]domain.FailedJobEntry, error)
	FetchPendingJobs(ctx context.Context, limit int) ([]domain.Job, error)
}

type jobRepo struct {
	db *sql.DB
}

// NewJobRepository creates a new repository backed by a *sql.DB.
func NewJobRepository(db *sql.DB) JobRepository {
	return &jobRepo{db: db}
}

func (r *jobRepo) Create(ctx context.Context, job *domain.Job) error {
	query := `
		INSERT INTO jobs (id, idempotency_key, status, input_url, max_retries, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	job.ID = uuid.New()
	now := time.Now().UTC()
	job.CreatedAt = now
	job.UpdatedAt = now
	job.Status = domain.StatusPending

	var idemKey *string
	if job.IdempotencyKey != "" {
		idemKey = &job.IdempotencyKey
	}

	// Use WithoutCancel so that the DB insert isn't rolled back or interrupted
	// by a client disconnect mid-flight, which would skip metric incrementing.
	insertCtx := context.WithoutCancel(ctx)
	_, err := r.db.ExecContext(insertCtx, query,
		job.ID, idemKey, job.Status, job.InputURL, job.MaxRetries, job.CreatedAt, job.UpdatedAt,
	)
	return err
}

func (r *jobRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Job, error) {
	query := `
		SELECT id, idempotency_key, status, input_url, total_rows, processed_rows,
		       retries, max_retries, error_message, created_at, updated_at, completed_at
		FROM jobs WHERE id = $1
	`
	row := r.db.QueryRowContext(ctx, query, id)
	return scanJob(row)
}

func (r *jobRepo) GetByIdempotencyKey(ctx context.Context, key string) (*domain.Job, error) {
	query := `
		SELECT id, idempotency_key, status, input_url, total_rows, processed_rows,
		       retries, max_retries, error_message, created_at, updated_at, completed_at
		FROM jobs WHERE idempotency_key = $1
	`
	row := r.db.QueryRowContext(ctx, query, key)
	return scanJob(row)
}

func (r *jobRepo) List(ctx context.Context, params domain.ListJobsParams) ([]domain.Job, error) {
	query := `
		SELECT id, idempotency_key, status, input_url, total_rows, processed_rows,
		       retries, max_retries, error_message, created_at, updated_at, completed_at
		FROM jobs
	`
	args := []interface{}{}
	argIdx := 1

	if params.Status != "" {
		query += fmt.Sprintf(" WHERE status = $%d", argIdx)
		args = append(args, params.Status)
		argIdx++
	}

	query += " ORDER BY created_at DESC"

	if params.Limit <= 0 {
		params.Limit = 20
	}
	if params.Limit > 100 {
		params.Limit = 100
	}
	query += fmt.Sprintf(" LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, params.Limit, params.Offset)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.Job
	for rows.Next() {
		j, err := scanJobFromRows(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *j)
	}
	return jobs, rows.Err()
}

func (r *jobRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status string) (bool, error) {
	// 実行中（Running）への更新時は、既に他のWorkerが着手していないか（pending/failedからのリトライか）を保証する
	var query string
	if status == "running" {
		query = `UPDATE jobs SET status = $1, updated_at = NOW() WHERE id = $2 AND status = 'pending'`
	} else {
		query = `UPDATE jobs SET status = $1, updated_at = NOW() WHERE id = $2`
	}
	res, err := r.db.ExecContext(ctx, query, status, id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

func (r *jobRepo) UpdateProgress(ctx context.Context, id uuid.UUID, processedRows, totalRows int64) error {
	query := `UPDATE jobs SET processed_rows = $1, total_rows = $2, updated_at = NOW() WHERE id = $3`
	_, err := r.db.ExecContext(ctx, query, processedRows, totalRows, id)
	return err
}

func (r *jobRepo) MarkCompleted(ctx context.Context, id uuid.UUID, status, errorMsg string) (bool, error) {
	// Only update if it's not already terminal
	query := `
		UPDATE jobs 
		SET status = $1, error_message = $2, completed_at = NOW(), updated_at = NOW() 
		WHERE id = $3 AND status NOT IN ('succeeded', 'failed', 'canceled')
	`
	res, err := r.db.ExecContext(ctx, query, status, errorMsg, id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

func (r *jobRepo) IncrementRetry(ctx context.Context, id uuid.UUID) (int, error) {
	query := `UPDATE jobs SET retries = retries + 1, updated_at = NOW() WHERE id = $1 RETURNING retries`
	var retries int
	err := r.db.QueryRowContext(ctx, query, id).Scan(&retries)
	return retries, err
}

func (r *jobRepo) InsertFailedEntry(ctx context.Context, entry *domain.FailedJobEntry) error {
	query := `
		INSERT INTO failed_job_entries (id, job_id, error_message, attempt, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`
	entry.ID = uuid.New()
	entry.CreatedAt = time.Now().UTC()
	_, err := r.db.ExecContext(ctx, query, entry.ID, entry.JobID, entry.ErrorMessage, entry.Attempt, entry.CreatedAt)
	return err
}

func (r *jobRepo) ListFailedEntries(ctx context.Context, jobID uuid.UUID) ([]domain.FailedJobEntry, error) {
	query := `
		SELECT id, job_id, error_message, attempt, created_at
		FROM failed_job_entries WHERE job_id = $1 ORDER BY attempt ASC
	`
	rows, err := r.db.QueryContext(ctx, query, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []domain.FailedJobEntry
	for rows.Next() {
		var e domain.FailedJobEntry
		if err := rows.Scan(&e.ID, &e.JobID, &e.ErrorMessage, &e.Attempt, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (r *jobRepo) FetchPendingJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	query := `
		SELECT id, idempotency_key, status, input_url, total_rows, processed_rows,
		       retries, max_retries, error_message, created_at, updated_at, completed_at
		FROM jobs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1
	`
	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []domain.Job
	for rows.Next() {
		j, err := scanJobFromRows(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *j)
	}
	return jobs, rows.Err()
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...interface{}) error
}

func scanJob(row scannable) (*domain.Job, error) {
	var j domain.Job
	var idemKey, errMsg sql.NullString
	var completedAt sql.NullTime

	err := row.Scan(
		&j.ID, &idemKey, &j.Status, &j.InputURL,
		&j.TotalRows, &j.ProcessedRows,
		&j.Retries, &j.MaxRetries,
		&errMsg, &j.CreatedAt, &j.UpdatedAt, &completedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	if idemKey.Valid {
		j.IdempotencyKey = idemKey.String
	}
	if errMsg.Valid {
		j.ErrorMessage = errMsg.String
	}
	if completedAt.Valid {
		j.CompletedAt = &completedAt.Time
	}
	return &j, nil
}

func scanJobFromRows(rows *sql.Rows) (*domain.Job, error) {
	return scanJob(rows)
}
