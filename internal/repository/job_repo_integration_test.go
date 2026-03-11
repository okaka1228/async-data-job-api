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
