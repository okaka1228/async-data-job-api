package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/google/uuid"
)

func TestWebhookNotifier_Success(t *testing.T) {
	var received WebhookPayload
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	now := time.Now().UTC()
	jobID := uuid.New()
	job := &domain.Job{
		ID:           jobID,
		Status:       domain.StatusFailed,
		ErrorMessage: "something went wrong",
		Retries:      3,
		CallbackURL:  ts.URL,
		CompletedAt:  &now,
	}

	n := NewWebhookNotifier(&http.Client{Timeout: 5 * time.Second})
	if err := n.Notify(context.Background(), job); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if received.JobID != jobID.String() {
		t.Errorf("expected job_id %s, got %s", jobID, received.JobID)
	}
	if received.Status != domain.StatusFailed {
		t.Errorf("expected status failed, got %s", received.Status)
	}
	if received.ErrorMessage != "something went wrong" {
		t.Errorf("expected error message, got %s", received.ErrorMessage)
	}
	if received.Retries != 3 {
		t.Errorf("expected retries 3, got %d", received.Retries)
	}
	if received.FailedAt == nil {
		t.Error("expected failed_at to be set")
	}
}

func TestWebhookNotifier_NoCallbackURL(t *testing.T) {
	n := NewWebhookNotifier(&http.Client{Timeout: 5 * time.Second})
	job := &domain.Job{ID: uuid.New(), Status: domain.StatusFailed}
	// No CallbackURL — should be a no-op
	if err := n.Notify(context.Background(), job); err != nil {
		t.Fatalf("expected no error for empty callback URL, got %v", err)
	}
}

func TestWebhookNotifier_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	n := NewWebhookNotifier(&http.Client{Timeout: 5 * time.Second})
	job := &domain.Job{ID: uuid.New(), Status: domain.StatusFailed, CallbackURL: ts.URL}
	err := n.Notify(context.Background(), job)
	if err == nil {
		t.Error("expected error for non-2xx status, got nil")
	}
}

func TestWebhookNotifier_ConnectionRefused(t *testing.T) {
	n := NewWebhookNotifier(&http.Client{Timeout: 1 * time.Second})
	job := &domain.Job{
		ID:          uuid.New(),
		Status:      domain.StatusFailed,
		CallbackURL: "http://127.0.0.1:1", // nothing listening
	}
	if err := n.Notify(context.Background(), job); err == nil {
		t.Error("expected error for unreachable server, got nil")
	}
}

func TestNoopNotifier(t *testing.T) {
	n := &NoopNotifier{}
	job := &domain.Job{ID: uuid.New(), Status: domain.StatusFailed, CallbackURL: "http://example.com"}
	if err := n.Notify(context.Background(), job); err != nil {
		t.Fatalf("NoopNotifier should never return error, got %v", err)
	}
}
