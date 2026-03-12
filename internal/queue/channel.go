package queue

import (
	"context"
	"sync"
)

// Queue is an abstraction over job dispatch.
type Queue interface {
	Enqueue(ctx context.Context, jobID string) error
	Dequeue(ctx context.Context) <-chan string
	Close()
}

// ChannelQueue is an in-memory, channel-based queue.
type ChannelQueue struct {
	ch     chan string
	closed bool
	mu     sync.Mutex
}

// NewChannelQueue creates a buffered channel queue.
func NewChannelQueue(bufSize int) *ChannelQueue {
	if bufSize <= 0 {
		bufSize = 100
	}
	return &ChannelQueue{
		ch: make(chan string, bufSize),
	}
}

func (q *ChannelQueue) Enqueue(_ context.Context, jobID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	select {
	case q.ch <- jobID:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *ChannelQueue) Dequeue(_ context.Context) <-chan string {
	return q.ch
}

func (q *ChannelQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}
