package domain

import (
	"testing"
)

func TestCreateJobRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateJobRequest
		wantErr bool
	}{
		{
			name:    "valid request",
			req:     CreateJobRequest{InputURL: "https://example.com/data.json"},
			wantErr: false,
		},
		{
			name:    "empty input_url",
			req:     CreateJobRequest{InputURL: ""},
			wantErr: true,
		},
		{
			name:    "input_url too long",
			req:     CreateJobRequest{InputURL: string(make([]byte, 2049))},
			wantErr: true,
		},
		{
			name:    "negative max_retries",
			req:     CreateJobRequest{InputURL: "https://example.com/data.json", MaxRetries: intPtr(-1)},
			wantErr: true,
		},
		{
			name:    "valid with idempotency key",
			req:     CreateJobRequest{InputURL: "https://example.com/data.json", IdempotencyKey: "key-123"},
			wantErr: false,
		},
		{
			name:    "idempotency_key too long",
			req:     CreateJobRequest{InputURL: "https://example.com/data.json", IdempotencyKey: string(make([]byte, 256))},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJobIsTerminal(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusPending, false},
		{StatusRunning, false},
		{StatusSucceeded, true},
		{StatusFailed, true},
		{StatusCanceled, true},
	}
	for _, tt := range tests {
		j := &Job{Status: tt.status}
		if got := j.IsTerminal(); got != tt.want {
			t.Errorf("IsTerminal() for %s = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestJobCanCancel(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusPending, true},
		{StatusRunning, true},
		{StatusSucceeded, false},
		{StatusFailed, false},
		{StatusCanceled, false},
	}
	for _, tt := range tests {
		j := &Job{Status: tt.status}
		if got := j.CanCancel(); got != tt.want {
			t.Errorf("CanCancel() for %s = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func intPtr(i int) *int { return &i }
