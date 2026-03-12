//go:build integration

package repository

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("database not available: %v", err)
	}
	return db
}

func TestJobRepo_CreateAndGetByID(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	repo := NewJobRepository(db)
	ctx := context.Background()

	job := &domain.Job{
		InputURL:   "https://example.com/test.json",
		MaxRetries: 3,
	}

	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if job.ID == uuid.Nil {
		t.Fatal("expected non-nil ID after create")
	}

	got, err := repo.GetByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got == nil {
		t.Fatal("expected job, got nil")
	}
	if got.InputURL != job.InputURL {
		t.Errorf("expected input_url %s, got %s", job.InputURL, got.InputURL)
	}
	if got.Status != domain.StatusPending {
		t.Errorf("expected status pending, got %s", got.Status)
	}

	// Cleanup
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_IdempotencyKey(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	repo := NewJobRepository(db)
	ctx := context.Background()

	key := "test-idem-" + uuid.New().String()

	job := &domain.Job{
		InputURL:       "https://example.com/test.json",
		IdempotencyKey: key,
		MaxRetries:     3,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := repo.GetByIdempotencyKey(ctx, key)
	if err != nil {
		t.Fatalf("GetByIdempotencyKey() error = %v", err)
	}
	if got == nil {
		t.Fatal("expected job, got nil")
	}
	if got.ID != job.ID {
		t.Error("IDs should match")
	}

	// Cleanup
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_ListAndUpdateStatus(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	repo := NewJobRepository(db)
	ctx := context.Background()

	job := &domain.Job{
		InputURL:   "https://example.com/list-test.json",
		MaxRetries: 3,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// List
	jobs, err := repo.List(ctx, domain.ListJobsParams{Limit: 10})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(jobs) == 0 {
		t.Fatal("expected at least one job")
	}

	// Update status
	if _, err := repo.UpdateStatus(ctx, job.ID, domain.StatusRunning); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	got, _ := repo.GetByID(ctx, job.ID)
	if got.Status != domain.StatusRunning {
		t.Errorf("expected running, got %s", got.Status)
	}

	// Cleanup
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_DLQ(t *testing.T) {
	db := setupDB(t)
	defer db.Close()

	repo := NewJobRepository(db)
	ctx := context.Background()

	job := &domain.Job{
		InputURL:   "https://example.com/dlq-test.json",
		MaxRetries: 1,
	}
	if err := repo.Create(ctx, job); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	entry := &domain.FailedJobEntry{
		JobID:        job.ID,
		ErrorMessage: "test failure",
		Attempt:      1,
	}
	if err := repo.InsertFailedEntry(ctx, entry); err != nil {
		t.Fatalf("InsertFailedEntry() error = %v", err)
	}

	entries, err := repo.ListFailedEntries(ctx, job.ID)
	if err != nil {
		t.Fatalf("ListFailedEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ErrorMessage != "test failure" {
		t.Errorf("unexpected error_message: %s", entries[0].ErrorMessage)
	}

	// Cleanup
	db.ExecContext(ctx, "DELETE FROM failed_job_entries WHERE job_id = $1", job.ID)
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_UpdateProgressAndMarkCompleted(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := NewJobRepository(db)
	ctx := context.Background()

	job := &domain.Job{InputURL: "http://example.com/progress", Status: domain.StatusPending}
	_ = repo.Create(ctx, job)

	err := repo.UpdateProgress(ctx, job.ID, 50, 100)
	if err != nil {
		t.Fatalf("UpdateProgress() error = %v", err)
	}

	got, _ := repo.GetByID(ctx, job.ID)
	if got.ProcessedRows != 50 || got.TotalRows != 100 {
		t.Errorf("expected 50/100, got %d/%d", got.ProcessedRows, got.TotalRows)
	}

	ok, err := repo.MarkCompleted(ctx, job.ID, domain.StatusSucceeded, "")
	if err != nil || !ok {
		t.Fatalf("MarkCompleted() failed: %v", err)
	}

	gotFinal, _ := repo.GetByID(ctx, job.ID)
	if gotFinal.Status != domain.StatusSucceeded {
		t.Errorf("expected status succeeded, got %s", gotFinal.Status)
	}
	
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_IncrementRetry(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := NewJobRepository(db)
	ctx := context.Background()

	job := &domain.Job{InputURL: "test-retry", Status: domain.StatusPending}
	_ = repo.Create(ctx, job)

	count, err := repo.IncrementRetry(ctx, job.ID)
	if err != nil {
		t.Fatalf("IncrementRetry() failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected retry 1, got %d", count)
	}

	got, _ := repo.GetByID(ctx, job.ID)
	if got.Retries != 1 {
		t.Errorf("expected retries in db 1, got %d", got.Retries)
	}
	
	db.ExecContext(ctx, "DELETE FROM jobs WHERE id = $1", job.ID)
}

func TestJobRepo_FetchPendingJobs(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	repo := NewJobRepository(db)
	ctx := context.Background()

	// Create an old pending job
	job1 := &domain.Job{InputURL: "fetch-1", Status: domain.StatusPending}
	_ = repo.Create(ctx, job1)
	db.ExecContext(ctx, "UPDATE jobs SET updated_at = NOW() - INTERVAL '6 minutes' WHERE id = $1", job1.ID)

	// Create a new pending job (should be excluded if grace period is 5m)
	job2 := &domain.Job{InputURL: "fetch-2", Status: domain.StatusPending}
	_ = repo.Create(ctx, job2)
	db.ExecContext(ctx, "UPDATE jobs SET updated_at = NOW() WHERE id = $1", job2.ID)

	jobs, err := repo.FetchPendingJobs(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPendingJobs failed: %v", err)
	}

	// Should only find job1
	found := false
	for _, j := range jobs {
		if j.ID == job1.ID {
			found = true
		}
		if j.ID == job2.ID {
			t.Errorf("job2 should not be fetched, it was updated recently")
		}
	}
	if !found {
		t.Errorf("job1 should be fetched")
	}

	db.ExecContext(ctx, "DELETE FROM jobs WHERE id IN ($1, $2)", job1.ID, job2.ID)
}
