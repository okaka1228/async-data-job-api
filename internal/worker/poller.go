package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/queue"
	"github.com/femto/async-data-job-api/internal/repository"
)

// Poller pulls pending jobs from the database and adds them to the queue
// if they were dropped (e.g., due to a full in-memory queue or server restart).
type Poller struct {
	repo     repository.JobRepository
	q        queue.Queue
	logger   *slog.Logger
	interval time.Duration
	limit    int
}

// NewPoller creates a new pending jobs poller.
func NewPoller(repo repository.JobRepository, q queue.Queue, logger *slog.Logger) *Poller {
	return &Poller{
		repo:     repo,
		q:        q,
		logger:   logger,
		interval: 10 * time.Second, // Poll every 10 seconds
		limit:    100,              // Fetch up to 100 pending jobs
	}
}

// Start runs the poller in the background until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	p.logger.Info("starting pending job poller", "interval", p.interval)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("stopping pending job poller")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	// Add a short timeout so DB queries don't hang indefinitely
	pollCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	jobs, err := p.repo.FetchPendingJobs(pollCtx, p.limit)
	if err != nil {
		p.logger.Error("failed to fetch pending jobs", "error", err)
		return
	}

	for _, job := range jobs {
		// Calculate time since it was created/updated.
		// If it's too new, it might still be in the channel queue from the API request.
		// We only pick up jobs that have been pending for longer than 5 seconds.
		age := time.Since(job.UpdatedAt)
		if age < 5*time.Second {
			continue
		}

		err := p.q.Enqueue(ctx, job.ID.String())
		if err != nil {
			if err == queue.ErrQueueFull {
				// Queue is full, stop polling for now
				p.logger.Debug("queue full during polling, stopping batch early")
				break
			}
			p.logger.Warn("failed to enqueue pending job during poll", "job_id", job.ID, "error", err)
		} else {
			// Update the timestamp so we don't pick it up immediately on the next poll
			// if the worker hasn't started it yet (e.g. if the queue is backed up for a minute).
			// We just update `updated_at` without changing the status.
			_, _ = p.repo.UpdateStatus(ctx, job.ID, domain.StatusPending)
			p.logger.Info("re-enqueued stuck pending job", "job_id", job.ID, "age", age.Round(time.Second))
		}
	}
}
