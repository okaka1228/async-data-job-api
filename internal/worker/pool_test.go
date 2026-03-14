package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/google/uuid"
)

func TestPool_StartAndShutdown(t *testing.T) {
	q := queue.NewChannelQueue(10)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	repo := &mockJobRepo{}
	
	baseProcessor := NewProcessor(repo, testMetrics, logger, 5*time.Second, 3, &NoopNotifier{})
	
	// Wait, we need to override the actual method if we want polymorphism, but Processor is a struct.
	// Since Pool takes *Processor directly, we can't easily mock Process without an interface.
	// That's fine, we will just use the real processor with the mockJobRepo and verify the repo is updated!
	
	realPool := NewPool(2, q, baseProcessor, logger)
	ctxPool, cancelPool := context.WithCancel(context.Background())
	
	// Start pool in background
	go realPool.Start(ctxPool)

	// Feed a job
	jobID := uuid.New()
	baseProcessor.repo.(*mockJobRepo).job = &domain.Job{
		ID:         jobID,
		Status:     domain.StatusPending,
		InputURL:   "http://invalid",
		MaxRetries: 3,
	}
	
	_ = q.Enqueue(context.Background(), jobID.String())

	// Give it a moment to process (it will hit permanent failure due to http://invalid)
	time.Sleep(100 * time.Millisecond)

	// Trigger shutdown
	cancelPool()
	realPool.Shutdown()

	// Verify the processor was actually called and manipulated the repo
	repo.mu.Lock()
	defer repo.mu.Unlock()
	
	if repo.retryCount == 0 && repo.updatedStatus == "" {
		t.Error("worker pool did not process the enqueued job")
	}
}
