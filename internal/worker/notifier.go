package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
)

// Notifier sends job failure notifications.
// Implement this interface to support different delivery mechanisms
// (e.g., fire-and-forget, queue-backed retry, SNS, etc.).
type Notifier interface {
	Notify(ctx context.Context, job *domain.Job) error
}

// WebhookPayload is the request body posted to the job's callback URL.
type WebhookPayload struct {
	JobID        string     `json:"job_id"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Retries      int        `json:"retries"`
	FailedAt     *time.Time `json:"failed_at,omitempty"`
}

// WebhookNotifier sends a fire-and-forget HTTP POST to the job's callback URL.
// Errors are returned to the caller for logging; no automatic retry is performed.
// To add reliable delivery, implement Notifier with a queue-backed mechanism.
type WebhookNotifier struct {
	client *http.Client
}

// NewWebhookNotifier creates a WebhookNotifier using the given HTTP client.
func NewWebhookNotifier(client *http.Client) *WebhookNotifier {
	return &WebhookNotifier{client: client}
}

func (n *WebhookNotifier) Notify(ctx context.Context, job *domain.Job) error {
	if job.CallbackURL == "" {
		return nil
	}

	payload := WebhookPayload{
		JobID:        job.ID.String(),
		Status:       job.Status,
		ErrorMessage: job.ErrorMessage,
		Retries:      job.Retries,
		FailedAt:     job.CompletedAt,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// NoopNotifier discards all notifications. Used when no callback URL is needed.
type NoopNotifier struct{}

func (n *NoopNotifier) Notify(_ context.Context, _ *domain.Job) error { return nil }
