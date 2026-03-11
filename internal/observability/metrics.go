package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the application.
type Metrics struct {
	JobsCreated   prometheus.Counter
	JobsCompleted prometheus.Counter
	JobsFailed    prometheus.Counter
	JobsCanceled  prometheus.Counter
	JobsRetried   prometheus.Counter
	RowsProcessed prometheus.Counter

	JobDuration prometheus.Histogram

	HTTPRequestDuration *prometheus.HistogramVec
	HTTPRequestsTotal   *prometheus.CounterVec
}

// NewMetrics registers and returns all metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		JobsCreated: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jobs_created_total",
			Help: "Total number of jobs created",
		}),
		JobsCompleted: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jobs_completed_total",
			Help: "Total number of jobs completed successfully",
		}),
		JobsFailed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jobs_failed_total",
			Help: "Total number of jobs that permanently failed",
		}),
		JobsCanceled: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jobs_canceled_total",
			Help: "Total number of jobs canceled",
		}),
		JobsRetried: promauto.NewCounter(prometheus.CounterOpts{
			Name: "jobs_retried_total",
			Help: "Total number of job retries",
		}),
		RowsProcessed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "rows_processed_total",
			Help: "Total number of rows processed across all jobs",
		}),
		JobDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "job_processing_duration_seconds",
			Help:    "Histogram of job processing duration",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 15),
		}),
		HTTPRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Histogram of HTTP request duration",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path", "status"}),
		HTTPRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests",
		}, []string{"method", "path", "status"}),
	}
}
