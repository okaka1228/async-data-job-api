package queue

import (
	"context"
	"testing"
	"time"
)

func TestChannelQueue_EnqueueDequeue(t *testing.T) {
	q := NewChannelQueue(10)
	defer q.Close()

	ctx := context.Background()

	if err := q.Enqueue(ctx, "job-1"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := q.Enqueue(ctx, "job-2"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	ch := q.Dequeue(ctx)

	select {
	case id := <-ch:
		if id != "job-1" {
			t.Errorf("expected job-1, got %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for dequeue")
	}

	select {
	case id := <-ch:
		if id != "job-2" {
			t.Errorf("expected job-2, got %s", id)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for dequeue")
	}
}

func TestChannelQueue_Full(t *testing.T) {
	q := NewChannelQueue(2)
	defer q.Close()

	ctx := context.Background()

	_ = q.Enqueue(ctx, "job-1")
	_ = q.Enqueue(ctx, "job-2")

	err := q.Enqueue(ctx, "job-3")
	if err != ErrQueueFull {
		t.Errorf("expected ErrQueueFull, got %v", err)
	}
}

func TestChannelQueue_ClosedEnqueue(t *testing.T) {
	q := NewChannelQueue(10)
	q.Close()

	err := q.Enqueue(context.Background(), "job-1")
	if err != ErrQueueClosed {
		t.Errorf("expected ErrQueueClosed, got %v", err)
	}
}

func TestChannelQueue_DoubleClose(t *testing.T) {
	q := NewChannelQueue(10)
	q.Close()
	q.Close() // should not panic
}

func TestNewChannelQueue_InvalidBufSize(t *testing.T) {
	q := NewChannelQueue(0)
	defer q.Close()

	// Should default to 100
	cap := cap(q.ch)
	if cap != 100 {
		t.Errorf("expected default buffer 100, got %d", cap)
	}
}
