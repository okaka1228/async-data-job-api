package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/google/uuid"
	"log/slog"
)

// --- mock repository ---

type mockJobRepo struct {
	jobs map[uuid.UUID]*domain.Job
	byKey map[string]*domain.Job
}

func newMockJobRepo() *mockJobRepo {
	return &mockJobRepo{
		jobs:  make(map[uuid.UUID]*domain.Job),
		byKey: make(map[string]*domain.Job),
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
