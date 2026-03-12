-- Composite partial index to speed up FetchPendingJobs query:
--   WHERE status = 'pending' AND updated_at < NOW() - INTERVAL '5 minutes'
--   ORDER BY created_at ASC
CREATE INDEX idx_jobs_status_updated_at ON jobs(status, updated_at) WHERE status = 'pending';
