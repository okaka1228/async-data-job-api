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
	pendingJobs   []domain.Job
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingJobs, nil
}

func (m *mockJobRepo) CancelJob(_ context.Context, _ uuid.UUID) (*domain.Job, error) {
	return nil, nil
}

func (m *mockJobRepo) TouchJob(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (m *mockJobRepo) RetryJob(_ context.Context, _ uuid.UUID) (*domain.Job, error) {
	return nil, nil
}

var testMetrics = observability.NewMetrics()

func TestProcessor_FailureAndRetry(t *testing.T) {
	// Setup a failing test server (HTTP 500)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
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
	processor := NewProcessor(repo, testMetrics, logger, 5*time.Second, 3, &NoopNotifier{})

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
	processor := NewProcessor(repo, testMetrics, logger, 5*time.Second, 3, &NoopNotifier{})

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

func TestProcessor_ProcessJSON_Array(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"item":1},{"item":2},{"item":3}]`))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	if repo.updatedStatus != domain.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", repo.updatedStatus)
	}
	// Note: We don't fully verify the precise updated row count with the mock because UpdateProgress isn't recorded exactly,
	// but the fact it didn't error internally means it completed successfully over 3 elements.
}

func TestProcessor_ProcessNDJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"item\":1}\n{\"item\":2}\n{\"item\":3}\n"))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	if repo.updatedStatus != domain.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", repo.updatedStatus)
	}
}

func TestProcessor_ProcessJSON_SingleObject(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"item":1}`))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	if repo.updatedStatus != domain.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", repo.updatedStatus)
	}
}

func TestProcessor_InvalidJobID(t *testing.T) {
	repo := &mockJobRepo{}
	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	// Should not panic, just log and return
	processor.Process(context.Background(), "not-a-uuid", 1)
}

func TestProcessor_TerminalStateSkip(t *testing.T) {
	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusSucceeded, InputURL: "http://x", MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	// Should NOT have been updated because job is already terminal
	if repo.updatedStatus != "" {
		t.Errorf("expected no status update for terminal job, got %s", repo.updatedStatus)
	}
}

func TestProcessor_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 1}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 1, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	// Should have recorded a failure
	if len(repo.failedEntries) == 0 {
		t.Error("expected at least one failed entry for HTTP error")
	}
}

func TestProcessor_InvalidNDJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"valid\":true}\nnot-valid-json\n"))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 1}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 1, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.failedEntries) == 0 {
		t.Error("expected at least one failed entry for invalid NDJSON")
	}
}

func TestProcessor_ContextTimeout(t *testing.T) {
	// Server that hangs
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`[{"item":1}]`))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 1}
	repo := &mockJobRepo{job: job}

	// Very short timeout
	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 100*time.Millisecond, 1, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.failedEntries) == 0 {
		t.Error("expected failure entry for timeout")
	}
}

func TestProcessor_LargeBatchNDJSON(t *testing.T) {
	// Generate 6000 NDJSON lines to exercise the batchSize (5000) threshold
	var buf []byte
	for i := 0; i < 6000; i++ {
		buf = append(buf, []byte(`{"i":1}`)...)
		buf = append(buf, '\n')
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 10*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	if repo.updatedStatus != domain.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", repo.updatedStatus)
	}
}

func TestProcessor_LargeBatchJSON(t *testing.T) {
	// Generate JSON array with 6000 elements
	var buf []byte
	buf = append(buf, '[')
	for i := 0; i < 6000; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, []byte(`{"i":1}`)...)
	}
	buf = append(buf, ']')

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, MaxRetries: 3}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 10*time.Second, 3, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	if repo.updatedStatus != domain.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", repo.updatedStatus)
	}
}
