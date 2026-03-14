package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/femto/async-data-job-api/internal/observability"
	"github.com/femto/async-data-job-api/internal/repository"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var tracer = otel.Tracer("worker")

// scannerBufPool reuses 1MB scanner buffers across concurrent workers.
// Stores *[]byte (pointer) to avoid heap allocation when boxing into any.
var scannerBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024*1024)
		return &b
	},
}

const contextCheckInterval = 1000

// Processor handles the actual data processing for a job.
type Processor struct {
	repo       repository.JobRepository
	metrics    *observability.Metrics
	logger     *slog.Logger
	timeout    time.Duration
	maxRetries int
	httpClient *http.Client
	notifier   Notifier
}

// NewProcessor creates a new processor.
func NewProcessor(
	repo repository.JobRepository,
	metrics *observability.Metrics,
	logger *slog.Logger,
	timeout time.Duration,
	maxRetries int,
	notifier Notifier,
) *Processor {
	return &Processor{
		repo:       repo,
		metrics:    metrics,
		logger:     logger,
		timeout:    timeout,
		maxRetries: maxRetries,
		notifier:   notifier,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Process processes a single job by ID.
func (p *Processor) Process(parentCtx context.Context, jobID string, workerID int) {
	ctx, span := tracer.Start(parentCtx, "ProcessJob")
	defer span.End()
	span.SetAttributes(attribute.String("job.id", jobID))

	logger := p.logger.With("job_id", jobID, "worker_id", workerID)
	start := time.Now()

	id, err := uuid.Parse(jobID)
	if err != nil {
		logger.Error("invalid job ID", "error", err)
		return
	}

	// Fetch job
	job, err := p.repo.GetByID(ctx, id)
	if err != nil || job == nil {
		logger.Error("failed to fetch job", "error", err)
		return
	}

	// Skip if already terminal or canceled
	if job.IsTerminal() {
		logger.Info("job already in terminal state", "status", job.Status)
		return
	}

	// Mark as running. If updated is false, another worker already picked this up.
	updated, err := p.repo.UpdateStatus(ctx, id, domain.StatusRunning)
	if err != nil {
		logger.Error("failed to update status to running", "error", err)
		return
	}
	if !updated {
		logger.Info("job already started by another worker", "status", job.Status)
		return
	}

	// Process with timeout
	processCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	processErr := p.processData(processCtx, job, logger)

	duration := time.Since(start)
	p.metrics.JobDuration.Observe(duration.Seconds())

	// Use a cancel-free context with a hard timeout for final state persistence.
	// WithoutCancel ensures a shutdown signal does not abort the DB write.
	// The explicit timeout prevents an indefinite hang if the DB is unresponsive,
	// which would block pool.Shutdown() from returning.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cleanupCancel()

	if processErr != nil {
		p.handleFailure(cleanupCtx, job, processErr, logger)
	} else {
		updated, err := p.repo.MarkCompleted(cleanupCtx, id, domain.StatusSucceeded, "")
		if err != nil {
			logger.Error("failed to mark job succeeded", "error", err)
			return
		}
		if updated {
			p.metrics.JobsCompleted.Inc()
			logger.Info("job completed", "duration", duration)
		} else {
			logger.Info("job already marked completed by another process")
		}
	}
}

func (p *Processor) processData(ctx context.Context, job *domain.Job, logger *slog.Logger) error {
	_, span := tracer.Start(ctx, "FetchAndParseData")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, job.InputURL, nil)
	if err != nil {
		return fmt.Errorf("create req: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch input: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	isNDJSON := strings.Contains(contentType, "ndjson") ||
		strings.HasSuffix(job.InputURL, ".ndjson") ||
		strings.HasSuffix(job.InputURL, ".jsonl")

	if isNDJSON {
		return p.processNDJSON(ctx, job, resp.Body, logger)
	}
	return p.processJSON(ctx, job, resp.Body, logger)
}

func (p *Processor) processNDJSON(ctx context.Context, job *domain.Job, r io.Reader, logger *slog.Logger) error {
	bufPtr := scannerBufPool.Get().(*[]byte)
	defer scannerBufPool.Put(bufPtr)

	scanner := bufio.NewScanner(r)
	scanner.Buffer(*bufPtr, 10*1024*1024) // 10MB max line
	var processed int64

	const batchSize = 5000
	metricsBatch := 0
	lastUpdate := time.Now()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Validate using standard library validation for accuracy (CPU optimized internally)
		if !json.Valid(line) {
			return fmt.Errorf("invalid json payload at line %d", processed+1)
		}
		processed++
		metricsBatch++

		if processed%contextCheckInterval == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("processing timeout: %w", ctx.Err())
			default:
			}
		}

		// Periodic metrics update
		if metricsBatch >= batchSize {
			p.metrics.RowsProcessed.Add(float64(metricsBatch))
			metricsBatch = 0

			// Throttle DB updates to max 1 per second
			now := time.Now()
			if now.Sub(lastUpdate) >= time.Second {
				if err := p.repo.UpdateProgress(ctx, job.ID, processed, 0); err != nil {
					logger.Warn("failed to update progress", "error", err)
				}
				lastUpdate = now
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}

	if metricsBatch > 0 {
		p.metrics.RowsProcessed.Add(float64(metricsBatch))
	}

	if err := p.repo.UpdateProgress(ctx, job.ID, processed, processed); err != nil {
		logger.Warn("failed to update final progress", "error", err)
	}

	logger.Info("NDJSON processing done", "rows", processed)
	return nil
}

func (p *Processor) processJSON(ctx context.Context, job *domain.Job, r io.Reader, logger *slog.Logger) error {
	decoder := json.NewDecoder(r)

	// Try to read opening bracket for array
	t, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}

	delim, isDelim := t.(json.Delim)
	if !isDelim || delim != '[' {
		// Non-array input — only a single JSON object is accepted.
		if !isDelim {
			// Scalar value (number, string, bool, null) — not a valid job payload.
			return fmt.Errorf("expected JSON object or array, got scalar value")
		}
		if delim != '{' {
			// Unexpected delimiter (e.g. '}', ']').
			return fmt.Errorf("unexpected token: %v", delim)
		}
		// Opening '{' was consumed by Token(); drain remaining tokens to detect broken JSON.
		depth := 1
		for depth > 0 {
			tok, tokErr := decoder.Token()
			if tokErr != nil {
				return fmt.Errorf("decode error: %w", tokErr)
			}
			if d, ok := tok.(json.Delim); ok {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		}
		// Reject trailing garbage after the single object.
		if decoder.More() {
			return fmt.Errorf("unexpected trailing content after single JSON value")
		}
		if err := p.repo.UpdateProgress(ctx, job.ID, 1, 1); err != nil {
			logger.Warn("failed to update progress", "error", err)
		}
		p.metrics.RowsProcessed.Add(1)
		logger.Info("JSON processing done (single object)", "rows", 1)
		return nil
	}

	var processed int64
	const batchSize = 5000
	metricsBatch := 0
	lastUpdate := time.Now()
	
	for decoder.More() {
		// Decode into empty struct to validate without allocating memory for fields
		var dummy struct{}
		if err := decoder.Decode(&dummy); err != nil {
			return fmt.Errorf("decode error at element %d: %w", processed+1, err)
		}
		processed++
		metricsBatch++

		if processed%contextCheckInterval == 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("processing timeout: %w", ctx.Err())
			default:
			}
		}

		if metricsBatch >= batchSize {
			p.metrics.RowsProcessed.Add(float64(metricsBatch))
			metricsBatch = 0

			// Throttle DB updates to max 1 per second
			now := time.Now()
			if now.Sub(lastUpdate) >= time.Second {
				if err := p.repo.UpdateProgress(ctx, job.ID, processed, 0); err != nil {
					logger.Warn("failed to update progress", "error", err)
				}
				lastUpdate = now
			}
		}
	}

	if metricsBatch > 0 {
		p.metrics.RowsProcessed.Add(float64(metricsBatch))
	}

	if err := p.repo.UpdateProgress(ctx, job.ID, processed, processed); err != nil {
		logger.Warn("failed to update final progress", "error", err)
	}

	logger.Info("JSON array processing done", "rows", processed)
	return nil
}

func (p *Processor) handleFailure(ctx context.Context, job *domain.Job, processErr error, logger *slog.Logger) {
	errMsg := processErr.Error()
	logger.Error("job processing failed", "error", errMsg)

	// Record DLQ entry
	entry := &domain.FailedJobEntry{
		JobID:        job.ID,
		ErrorMessage: errMsg,
		Attempt:      job.Retries + 1,
	}
	if err := p.repo.InsertFailedEntry(ctx, entry); err != nil {
		logger.Error("failed to insert DLQ entry", "error", err)
	}

	// Check retry
	retries, err := p.repo.IncrementRetry(ctx, job.ID)
	if err != nil {
		logger.Error("failed to increment retry", "error", err)
		return
	}

	maxRetries := job.MaxRetries
	if maxRetries <= 0 {
		maxRetries = p.maxRetries
	}

	if retries < maxRetries {
		// Re-enqueue by resetting to pending
		if _, err := p.repo.UpdateStatus(ctx, job.ID, domain.StatusPending); err != nil {
			logger.Error("failed to reset job to pending for retry", "error", err)
		}
		logger.Info("job requeued for retry", "attempt", retries+1, "max", maxRetries)
		p.metrics.JobsRetried.Inc()
	} else {
		// Permanently failed
		updated, err := p.repo.MarkCompleted(ctx, job.ID, domain.StatusFailed, errMsg)
		if err != nil {
			logger.Error("failed to mark job as failed", "error", err)
		}
		if updated {
			p.metrics.JobsFailed.Inc()
			logger.Error("job permanently failed after max retries", "retries", retries)
			now := time.Now().UTC()
			job.Status = domain.StatusFailed
			job.ErrorMessage = errMsg
			job.Retries = retries
			job.CompletedAt = &now
			// Notify with a short independent context so that webhook delivery does not
			// extend graceful shutdown. cleanupCtx (30 s, no-cancel) is intentionally
			// not reused here: DB writes need the full budget, but a fire-and-forget
			// HTTP call should not block pool.Shutdown() for more than a few seconds.
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer notifyCancel()
			if err := p.notifier.Notify(notifyCtx, job); err != nil {
				logger.Warn("failed to send failure notification", "job_id", job.ID, "error", err)
			}
		}
	}
}
