package worker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/femto/async-data-job-api/internal/queue"
)

// Pool manages a fixed number of goroutine workers.
type Pool struct {
	concurrency int
	queue       queue.Queue
	processor   *Processor
	wg          sync.WaitGroup
	logger      *slog.Logger
}

// NewPool creates a worker pool.
func NewPool(concurrency int, q queue.Queue, p *Processor, logger *slog.Logger) *Pool {
	return &Pool{
		concurrency: concurrency,
		queue:       q,
		processor:   p,
		logger:      logger,
	}
}

// Start launches worker goroutines. They run until ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	ch := p.queue.Dequeue(ctx)
	for i := 0; i < p.concurrency; i++ {
		p.wg.Add(1)
		go func(workerID int) {
			defer p.wg.Done()
			p.logger.Info("worker started", "worker_id", workerID)
			for {
				select {
				case <-ctx.Done():
					p.logger.Info("worker stopping", "worker_id", workerID)
					return
				case jobID, ok := <-ch:
					if !ok {
						p.logger.Info("worker queue closed", "worker_id", workerID)
						return
					}
					p.processor.Process(ctx, jobID, workerID)
				}
			}
		}(i)
	}
}

// Shutdown waits for all workers to finish.
func (p *Pool) Shutdown() {
	p.queue.Close()
	p.wg.Wait()
	p.logger.Info("all workers stopped")
}
