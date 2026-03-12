package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/google/uuid"
)

// mockJobRepo for recording state changes during processor execution.
type mockJobRepo struct {
	mu            sync.Mutex
	job           *domain.Job
	failedEntries []domain.FailedJobEntry
	updatedStatus string
	isCompleted   bool
	finalErrorMsg string
	retryCount    int
}

func (m *mockJobRepo) Create(ctx context.Context, job *domain.Job) error { return nil }
func (m *mockJobRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job, nil
}
func (m *mockJobRepo) GetByIdempotencyKey(ctx context.Context, key string) (*domain.Job, error) {
	return nil, nil
}
func (m *mockJobRepo) List(ctx context.Context, params domain.ListJobsParams) ([]domain.Job, error) {
	return nil, nil
}
func (m *mockJobRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedStatus = status
	return true, nil
}
func (m *mockJobRepo) UpdateProgress(ctx context.Context, id uuid.UUID, processed, total int64) error {
	return nil
}
func (m *mockJobRepo) MarkCompleted(ctx context.Context, id uuid.UUID, status, errorMsg string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedStatus = status
	m.isCompleted = true
	m.finalErrorMsg = errorMsg
	return true, nil
}
func (m *mockJobRepo) IncrementRetry(ctx context.Context, id uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCount++
	m.job.Retries = m.retryCount
	return m.retryCount, nil
}
func (m *mockJobRepo) InsertFailedEntry(ctx context.Context, entry *domain.FailedJobEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failedEntries = append(m.failedEntries, *entry)
	return nil
}
func (m *mockJobRepo) ListFailedEntries(ctx context.Context, jobID uuid.UUID) ([]domain.FailedJobEntry, error) {
	return nil, nil
}
func (m *mockJobRepo) FetchPendingJobs(ctx context.Context, limit int) ([]domain.Job, error) {
	return nil, nil
}

var testMetrics = observability.NewMetrics()

func TestProcessor_FailureAndRetry(t *testing.T) {
	// Setup a failing test server (HTTP 500)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   ts.URL,
		Retries:    0,
		MaxRetries: 3,
	}

	repo := &mockJobRepo{job: job}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil)) // use io.Discard in real code, but let's just make it silent
	processor := NewProcessor(repo, testMetrics, logger, 5*time.Second, 3)

	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	// Since retries = 0 initially and it failed, it should increment retries and set status back to pending.
	if repo.retryCount != 1 {
		t.Errorf("expected retryCount=1, got %d", repo.retryCount)
	}
	if repo.updatedStatus != domain.StatusPending {
		t.Errorf("expected status to be reset to pending, got %s", repo.updatedStatus)
	}
	if len(repo.failedEntries) != 1 {
		t.Fatalf("expected 1 failed entry, got %d", len(repo.failedEntries))
	}
	if !containsStr(repo.failedEntries[0].ErrorMessage, "status code: 500") {
		t.Errorf("expected status code 500 error in failed entry, got %s", repo.failedEntries[0].ErrorMessage)
	}
}

func TestProcessor_PermanentFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   ts.URL,
		Retries:    3, // Already at max retries
		MaxRetries: 3,
	}

	repo := &mockJobRepo{job: job, retryCount: 3}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	processor := NewProcessor(repo, testMetrics, logger, 5*time.Second, 3)

	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	// It should reach max retries and be marked completely failed
	if repo.updatedStatus != domain.StatusFailed {
		t.Errorf("expected status failed, got %s", repo.updatedStatus)
	}
	if !repo.isCompleted {
		t.Error("expected MarkCompleted to be called")
	}
	if repo.finalErrorMsg == "" {
		t.Error("expected a final error message to be set")
	}
	if len(repo.failedEntries) != 1 {
		t.Fatalf("expected 1 failed entry for the final failure, got %d", len(repo.failedEntries))
	}
}

func containsStr(s, substr string) bool {
	// Simple string contains fallback avoiding extra imports
	return len(s) >= len(substr) && func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}()
}
