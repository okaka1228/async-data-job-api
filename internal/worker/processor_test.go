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

// TestProcessor_PermanentFailure_WebhookCompletedAt verifies that the webhook payload
// includes a non-nil CompletedAt when a job is permanently failed (Fix 2).
func TestProcessor_PermanentFailure_WebhookCompletedAt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	spy := &spyNotifier{}
	jobID := uuid.New()
	job := &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   ts.URL,
		Retries:    3,
		MaxRetries: 3,
	}
	repo := &mockJobRepo{job: job, retryCount: 3}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, spy)
	processor.Process(context.Background(), jobID.String(), 1)

	spy.mu.Lock()
	notifiedJob := spy.lastJob
	spy.mu.Unlock()

	if notifiedJob == nil {
		t.Fatal("expected Notify to be called, but it was not")
	}
	if notifiedJob.CompletedAt == nil {
		t.Error("expected CompletedAt to be set in webhook payload, got nil")
	}
}

// TestProcessor_ProcessJSON_BrokenObject verifies that a broken JSON object body
// is rejected rather than silently counted as 1 row (Fix 3).
func TestProcessor_ProcessJSON_BrokenObject(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key": broken}`))
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
		t.Error("expected failure entry for broken JSON object")
	}
}

// TestProcessor_ProcessJSON_ScalarRejected verifies that a bare scalar is not silently
// accepted as a 1-row success (Fix 3).
func TestProcessor_ProcessJSON_ScalarRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`42`))
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: ts.URL, Retries: 0, MaxRetries: 1}
	repo := &mockJobRepo{job: job}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 1, &NoopNotifier{})
	processor.Process(context.Background(), jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.updatedStatus == domain.StatusSucceeded {
		t.Error("expected scalar top-level value to not succeed silently")
	}
}

// controllableMockJobRepo wraps mockJobRepo and fires onRunning() the moment
// UpdateStatus is called with StatusRunning. This lets tests cancel the parent
// context *after* GetByID succeeds (reflecting real production behaviour) but
// *before* the cleanup DB writes, so the cleanupCtx and notifyCtx isolation
// properties are genuinely exercised.
type controllableMockJobRepo struct {
	mockJobRepo
	onRunning func() // called once when status transitions to StatusRunning
}

func (m *controllableMockJobRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status string) (bool, error) {
	result, err := m.mockJobRepo.UpdateStatus(ctx, id, status)
	if m.onRunning != nil && status == domain.StatusRunning {
		m.onRunning()
	}
	return result, err
}

// TestProcessor_CleanupCtxSurvivesParentCancel verifies that the final DB state write
// (MarkCompleted) is executed even when the parent context is cancelled mid-flight.
// The parent is cancelled the moment the job transitions to running (after GetByID
// succeeds), which is the earliest realistic point in production where a shutdown
// signal can arrive and still reach the cleanup path.
func TestProcessor_CleanupCtxSurvivesParentCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   ts.URL,
		Retries:    3, // already at max so handleFailure → MarkCompleted
		MaxRetries: 3,
	}

	parentCtx, cancel := context.WithCancel(context.Background())
	repo := &controllableMockJobRepo{
		mockJobRepo: mockJobRepo{job: job, retryCount: 3},
		onRunning:   cancel, // cancel parent the instant running state is persisted
	}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, &NoopNotifier{})
	processor.Process(parentCtx, jobID.String(), 1)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	// cleanupCtx (WithoutCancel + WithTimeout) must have shielded the DB write.
	if !repo.isCompleted {
		t.Error("expected MarkCompleted to be called even when parent context is cancelled during processing")
	}
	if repo.updatedStatus != domain.StatusFailed {
		t.Errorf("expected status failed, got %s", repo.updatedStatus)
	}
}

// TestProcessor_NotifyUsesIndependentContext verifies that the context passed to Notify
// is independent of the parent cancellation and carries a short deadline (~5 s).
// The parent is cancelled at the running transition (after GetByID) to reach the
// notify path under realistic shutdown conditions.
func TestProcessor_NotifyUsesIndependentContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	jobID := uuid.New()
	job := &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   ts.URL,
		Retries:    3,
		MaxRetries: 3,
	}
	cn := &contextCapturingNotifier{}

	parentCtx, cancel := context.WithCancel(context.Background())
	repo := &controllableMockJobRepo{
		mockJobRepo: mockJobRepo{job: job, retryCount: 3},
		onRunning:   cancel,
	}

	processor := NewProcessor(repo, testMetrics, slog.New(slog.NewJSONHandler(io.Discard, nil)), 5*time.Second, 3, cn)
	processor.Process(parentCtx, jobID.String(), 1)

	cn.mu.Lock()
	gotCtx := cn.captured
	errAtCall := cn.errAtCall
	hasDeadline := cn.hasDeadline
	remaining := cn.remainingAtCall
	cn.mu.Unlock()

	if gotCtx == nil {
		t.Fatal("expected Notify to be called, but it was not")
	}
	// At the moment Notify was called the context must not have been cancelled
	// (parent cancel must not propagate into the notify context).
	if errAtCall != nil {
		t.Errorf("expected notify context to be active at call time, got: %v", errAtCall)
	}
	// The notify context must carry a deadline (~5 s, not inheriting cleanupCtx 30 s).
	if !hasDeadline {
		t.Fatal("expected notify context to have a deadline")
	}
	if remaining <= 0 {
		t.Error("notify context deadline had already passed at call time")
	}
	if remaining > 5*time.Second {
		t.Errorf("notify context deadline too far: %v — expected ≤5 s", remaining)
	}
}

// spyNotifier records the last job passed to Notify.
type spyNotifier struct {
	mu      sync.Mutex
	lastJob *domain.Job
}

func (s *spyNotifier) Notify(_ context.Context, job *domain.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *job
	s.lastJob = &cp
	return nil
}

// contextCapturingNotifier records context state at the moment Notify is called.
// We capture err and deadline immediately rather than storing the context itself,
// because the notify context is cancelled by a deferred cancel in handleFailure
// as soon as the function returns — after which ctx.Err() would always be non-nil.
type contextCapturingNotifier struct {
	mu              sync.Mutex
	captured        context.Context
	errAtCall       error
	hasDeadline     bool
	remainingAtCall time.Duration
}

func (c *contextCapturingNotifier) Notify(ctx context.Context, _ *domain.Job) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = ctx
	c.errAtCall = ctx.Err()
	deadline, ok := ctx.Deadline()
	c.hasDeadline = ok
	if ok {
		c.remainingAtCall = time.Until(deadline)
	}
	return nil
}
