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

func TestPoller_StartAndPoll(t *testing.T) {
	q := queue.NewChannelQueue(10)
	defer q.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	repo := &mockJobRepo{
		pendingJobs: []domain.Job{
			{ID: uuid.New(), Status: domain.StatusPending, UpdatedAt: time.Now().Add(-10 * time.Minute)},
			{ID: uuid.New(), Status: domain.StatusPending, UpdatedAt: time.Now().Add(-10 * time.Minute)},
		},
	}

	poller := NewPoller(repo, q, logger)

	// Call it synchronously once to avoid ticker loop re-fetching the mock data
	poller.poll(context.Background())

	// 2 jobs should have been enqueued successfully.
	// We read from the queue context to verify.
	count := 0
	readCtx, readCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer readCancel()

	ch := q.Dequeue(readCtx)
loop:
	for {
		select {
		case <-ch:
			count++
		case <-readCtx.Done():
			break loop
		}
	}

	if count != 2 {
		t.Errorf("expected 2 jobs enqueued by poller, got %d", count)
	}
}

func TestPoller_StartAndCancel(t *testing.T) {
	q := queue.NewChannelQueue(10)
	defer q.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	repo := &mockJobRepo{}

	poller := NewPoller(repo, q, logger)
	poller.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		poller.Start(ctx)
		close(done)
	}()

	// Let it tick a few times
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK: Start returned after cancel
	case <-time.After(time.Second):
		t.Error("Poller.Start did not return after context cancel")
	}
}

func TestPoller_QueueFull(t *testing.T) {
	// Queue with capacity 1
	q := queue.NewChannelQueue(1)
	defer q.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	repo := &mockJobRepo{
		pendingJobs: []domain.Job{
			{ID: uuid.New(), Status: domain.StatusPending, UpdatedAt: time.Now().Add(-10 * time.Minute)},
			{ID: uuid.New(), Status: domain.StatusPending, UpdatedAt: time.Now().Add(-10 * time.Minute)},
			{ID: uuid.New(), Status: domain.StatusPending, UpdatedAt: time.Now().Add(-10 * time.Minute)},
		},
	}

	poller := NewPoller(repo, q, logger)
	poller.poll(context.Background())

	// Only 1 should have been enqueued since queue capacity is 1
	count := 0
	readCtx, readCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer readCancel()
	ch := q.Dequeue(readCtx)
loop:
	for {
		select {
		case <-ch:
			count++
		case <-readCtx.Done():
			break loop
		}
	}

	if count != 1 {
		t.Errorf("expected 1 job enqueued (queue full), got %d", count)
	}
}
