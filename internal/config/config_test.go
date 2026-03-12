package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear environments
	os.Clearenv()

	cfg := Load()

	if cfg.ServerPort != "8080" {
		t.Errorf("expected default port 8080, got %s", cfg.ServerPort)
	}
	if cfg.WorkerConcurrency != 4 {
		t.Errorf("expected default concurrency 4, got %d", cfg.WorkerConcurrency)
	}
	if cfg.WorkerTimeout != 300*time.Second {
		t.Errorf("expected default timeout 300s, got %v", cfg.WorkerTimeout)
	}
	if cfg.WorkerMaxRetries != 3 {
		t.Errorf("expected default max retries 3, got %d", cfg.WorkerMaxRetries)
	}
}

func TestLoad_Envs(t *testing.T) {
	os.Clearenv()
	os.Setenv("SERVER_PORT", "9090")
	os.Setenv("WORKER_CONCURRENCY", "5")
	os.Setenv("WORKER_TIMEOUT_SEC", "60")
	os.Setenv("WORKER_MAX_RETRIES", "1")

	cfg := Load()

	if cfg.ServerPort != "9090" {
		t.Errorf("expected port 9090, got %s", cfg.ServerPort)
	}
	if cfg.WorkerConcurrency != 5 {
		t.Errorf("expected concurrency 5, got %d", cfg.WorkerConcurrency)
	}
	if cfg.WorkerTimeout != 60*time.Second {
		t.Errorf("expected timeout 60s, got %v", cfg.WorkerTimeout)
	}
	if cfg.WorkerMaxRetries != 1 {
		t.Errorf("expected max retries 1, got %d", cfg.WorkerMaxRetries)
	}
}

func TestLoad_InvalidIntFallback(t *testing.T) {
	os.Clearenv()
	os.Setenv("WORKER_CONCURRENCY", "invalid")

	cfg := Load()

	if cfg.WorkerConcurrency != 4 {
		t.Errorf("expected parsed string 'invalid' to fallback to default 4, got %d", cfg.WorkerConcurrency)
	}
}
