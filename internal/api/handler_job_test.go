package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/google/uuid"
	"log/slog"
)

// --- mock repository ---

type mockJobRepo struct {
	mu            sync.Mutex
	jobs          map[uuid.UUID]*domain.Job
	byKey         map[string]*domain.Job
	failedEntries map[uuid.UUID][]domain.FailedJobEntry
}

func newMockJobRepo() *mockJobRepo {
	return &mockJobRepo{
		jobs:          make(map[uuid.UUID]*domain.Job),
		byKey:         make(map[string]*domain.Job),
		failedEntries: make(map[uuid.UUID][]domain.FailedJobEntry),
	}
}

func (m *mockJobRepo) Create(_ context.Context, job *domain.Job) error {
	job.ID = uuid.New()
	job.Status = domain.StatusPending
	m.jobs[job.ID] = job
	if job.IdempotencyKey != "" {
		m.byKey[job.IdempotencyKey] = job
	}
	return nil
}

func (m *mockJobRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Job, error) {
	j, ok := m.jobs[id]
	if !ok {
		return nil, nil
	}
	return j, nil
}

func (m *mockJobRepo) GetByIdempotencyKey(_ context.Context, key string) (*domain.Job, error) {
	j, ok := m.byKey[key]
	if !ok {
		return nil, nil
	}
	return j, nil
}

func (m *mockJobRepo) List(_ context.Context, _ domain.ListJobsParams) ([]domain.Job, error) {
	var result []domain.Job
	for _, j := range m.jobs {
		result = append(result, *j)
	}
	return result, nil
}

func (m *mockJobRepo) UpdateStatus(_ context.Context, id uuid.UUID, status string) (bool, error) {
	if j, ok := m.jobs[id]; ok {
		j.Status = status
		return true, nil
	}
	return false, nil
}

func (m *mockJobRepo) UpdateProgress(_ context.Context, id uuid.UUID, processed, total int64) error {
	if j, ok := m.jobs[id]; ok {
		j.ProcessedRows = processed
		j.TotalRows = total
	}
	return nil
}

func (m *mockJobRepo) MarkCompleted(_ context.Context, id uuid.UUID, status, errMsg string) (bool, error) {
	if j, ok := m.jobs[id]; ok {
		if j.Status == "succeeded" || j.Status == "failed" || j.Status == "canceled" {
			return false, nil
		}
		j.Status = status
		j.ErrorMessage = errMsg
		return true, nil
	}
	return false, nil
}

func (m *mockJobRepo) IncrementRetry(_ context.Context, id uuid.UUID) (int, error) {
	if j, ok := m.jobs[id]; ok {
		j.Retries++
		return j.Retries, nil
	}
	return 0, nil
}

func (m *mockJobRepo) InsertFailedEntry(_ context.Context, _ *domain.FailedJobEntry) error {
	return nil
}

func (m *mockJobRepo) ListFailedEntries(_ context.Context, _ uuid.UUID) ([]domain.FailedJobEntry, error) {
	return nil, nil
}

func (m *mockJobRepo) FetchPendingJobs(_ context.Context, _ int) ([]domain.Job, error) {
	return nil, nil
}

func (m *mockJobRepo) TouchJob(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (m *mockJobRepo) CancelJob(_ context.Context, id uuid.UUID) (*domain.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, nil
	}
	if j.Status != domain.StatusPending && j.Status != domain.StatusRunning {
		return nil, nil
	}
	j.Status = domain.StatusCanceled
	j.ErrorMessage = "canceled by user"
	return j, nil
}

func (m *mockJobRepo) RetryJob(_ context.Context, id uuid.UUID) (*domain.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, nil
	}
	if j.Status != domain.StatusFailed {
		return nil, nil
	}
	j.Status = domain.StatusPending
	j.Retries = 0
	j.ErrorMessage = ""
	return j, nil
}

// --- shared test fixtures ---

var (
	testMetrics = observability.NewMetrics()
	testLogger  = slog.Default()
)

// --- tests ---

func TestCreateJob_Success(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	handler := NewJobHandler(repo, q, testMetrics, testLogger)

	body := `{"input_url": "https://example.com/data.json"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.CreateJob(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp domain.Job
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID == uuid.Nil {
		t.Error("expected non-nil job ID")
	}
	if resp.Status != domain.StatusPending {
		t.Errorf("expected status pending, got %s", resp.Status)
	}
}

func TestCreateJob_InvalidBody(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	handler := NewJobHandler(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	handler.CreateJob(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422, got %d", rr.Code)
	}
}

func TestCreateJob_Idempotency(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	handler := NewJobHandler(repo, q, testMetrics, testLogger)

	body := `{"input_url": "https://example.com/data.json", "idempotency_key": "key-1"}`

	// First request
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	handler.CreateJob(rr1, req1)

	if rr1.Code != http.StatusAccepted {
		t.Fatalf("first request: expected 202, got %d", rr1.Code)
	}

	var first domain.Job
	_ = json.NewDecoder(rr1.Body).Decode(&first)

	// Second request with same key
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	handler.CreateJob(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("second request: expected 200, got %d", rr2.Code)
	}

	var second domain.Job
	_ = json.NewDecoder(rr2.Body).Decode(&second)

	if first.ID != second.ID {
		t.Error("idempotent requests should return the same job ID")
	}
}

func TestGetJob_NotFound(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+uuid.New().String(), nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestListJobs_Empty(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestHealthz(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestCancelJob_Success(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: "test"}
	repo.mu.Lock()
	repo.jobs[jobID] = job
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/cancel", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}
	
	repo.mu.Lock()
	updatedJob := repo.jobs[jobID]
	repo.mu.Unlock()
	if updatedJob.Status != domain.StatusCanceled {
		t.Errorf("expected status canceled, got %s", updatedJob.Status)
	}
}

func TestCancelJob_Uncancelable(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	job := &domain.Job{ID: jobID, Status: domain.StatusSucceeded, InputURL: "test"}
	repo.mu.Lock()
	repo.jobs[jobID] = job
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/cancel", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict if already successful, got %d", rr.Code)
	}
}

func TestGetJobFailures_Success(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	repo.mu.Lock()
	if repo.failedEntries == nil {
		repo.failedEntries = make(map[uuid.UUID][]domain.FailedJobEntry)
	}
	repo.failedEntries[jobID] = []domain.FailedJobEntry{
		{JobID: jobID, ErrorMessage: "some failure"},
	}
	repo.jobs[jobID] = &domain.Job{ID: jobID, Status: domain.StatusFailed, InputURL: "test"}
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID.String()+"/failures", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestGetJob_Found(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	repo.mu.Lock()
	repo.jobs[jobID] = &domain.Job{ID: jobID, Status: domain.StatusRunning, InputURL: "http://x"}
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+jobID.String(), nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	var resp domain.Job
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ID != jobID {
		t.Errorf("expected job ID %s, got %s", jobID, resp.ID)
	}
}

func TestGetJob_BadUUID(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/not-a-uuid", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestCancelJob_NotFound(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+uuid.New().String()+"/cancel", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestCancelJob_BadUUID(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/not-a-uuid/cancel", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestGetJobFailures_BadUUID(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/not-a-uuid/failures", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestCreateJob_MalformedJSON(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	handler := NewJobHandler(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(`{invalid}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.CreateJob(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d", rr.Code)
	}
}

func TestCreateJob_WithMaxRetries(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	handler := NewJobHandler(repo, q, testMetrics, testLogger)

	body := `{"input_url": "https://example.com/data.json", "max_retries": 5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.CreateJob(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rr.Code)
	}

	var resp domain.Job
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.MaxRetries != 5 {
		t.Errorf("expected max_retries 5, got %d", resp.MaxRetries)
	}
}

func TestListJobs_WithFilter(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	// Seed a job
	jobID := uuid.New()
	repo.mu.Lock()
	repo.jobs[jobID] = &domain.Job{ID: jobID, Status: domain.StatusPending, InputURL: "test"}
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs?limit=5&offset=0&status=pending", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestRetryJob_Success(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	repo.mu.Lock()
	repo.jobs[jobID] = &domain.Job{ID: jobID, Status: domain.StatusFailed, InputURL: "test", Retries: 3}
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/retry", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	repo.mu.Lock()
	updatedJob := repo.jobs[jobID]
	repo.mu.Unlock()
	if updatedJob.Status != domain.StatusPending {
		t.Errorf("expected status pending after retry, got %s", updatedJob.Status)
	}
	if updatedJob.Retries != 0 {
		t.Errorf("expected retries=0 after retry, got %d", updatedJob.Retries)
	}
}

func TestRetryJob_NotFound(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+uuid.New().String()+"/retry", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
}

func TestRetryJob_NotRetryable(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	jobID := uuid.New()
	repo.mu.Lock()
	repo.jobs[jobID] = &domain.Job{ID: jobID, Status: domain.StatusRunning, InputURL: "test"}
	repo.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID.String()+"/retry", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d", rr.Code)
	}
}

func TestRetryJob_BadUUID(t *testing.T) {
	repo := newMockJobRepo()
	q := queue.NewChannelQueue(10)
	defer q.Close()

	router := NewRouter(repo, q, testMetrics, testLogger)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/not-a-uuid/retry", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}

func TestRecoverMiddleware_PanicRecovery(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	recoverMw := RecoverMiddleware(logger)
	handler := recoverMw(panicHandler)

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rr := httptest.NewRecorder()

	// Should not panic
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 after panic recovery, got %d", rr.Code)
	}
}
