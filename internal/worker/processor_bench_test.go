package worker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/femto/async-data-job-api/internal/domain"
	"github.com/google/uuid"
)

// ndjsonGenerator streams N bytes of NDJSON into pw.
func ndjsonGenerator(pw *io.PipeWriter, targetBytes int) {
	line := []byte(`{"id":1234567890,"name":"benchmark-test","value":0.123456789,"active":true}` + "\n")
	written := 0
	for written < targetBytes {
		n, err := pw.Write(line)
		if err != nil {
			break
		}
		written += n
	}
	pw.Close()
}

// jsonArrayGenerator streams N bytes of a JSON array into pw.
func jsonArrayGenerator(pw *io.PipeWriter, targetBytes int) {
	elem := []byte(`{"id":1234567890,"name":"benchmark-test","value":0.123456789,"active":true}`)
	comma := []byte(",")
	written := 0

	pw.Write([]byte("["))
	written++
	first := true
	for written < targetBytes-1 {
		if !first {
			pw.Write(comma)
			written++
		}
		n, err := pw.Write(elem)
		if err != nil {
			break
		}
		written += n
		first = false
	}
	pw.Write([]byte("]"))
	pw.Close()
}

func newBenchProcessor() *Processor {
	return &Processor{
		repo:       &mockJobRepo{job: &domain.Job{ID: uuid.New()}},
		metrics:    testMetrics, // reuse package-level metrics to avoid duplicate registration
		logger:     slog.Default(),
		timeout:    10 * time.Minute,
		maxRetries: 3,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
	}
}

func benchmarkNDJSON(b *testing.B, sizeBytes int) {
	b.Helper()
	proc := newBenchProcessor()
	job := &domain.Job{ID: uuid.New(), InputURL: "benchmark"}

	b.ResetTimer()
	b.SetBytes(int64(sizeBytes))

	for i := 0; i < b.N; i++ {
		pr, pw := io.Pipe()
		go ndjsonGenerator(pw, sizeBytes)

		if err := proc.processNDJSON(context.Background(), job, pr, slog.Default()); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkJSON(b *testing.B, sizeBytes int) {
	b.Helper()
	proc := newBenchProcessor()
	job := &domain.Job{ID: uuid.New(), InputURL: "benchmark"}

	b.ResetTimer()
	b.SetBytes(int64(sizeBytes))

	for i := 0; i < b.N; i++ {
		pr, pw := io.Pipe()
		go jsonArrayGenerator(pw, sizeBytes)

		if err := proc.processJSON(context.Background(), job, pr, slog.Default()); err != nil {
			b.Fatal(err)
		}
	}
}

// --- 1GB benchmarks ---

func BenchmarkProcessNDJSON_1GB(b *testing.B) {
	benchmarkNDJSON(b, 1024*1024*1024)
}

func BenchmarkProcessJSON_1GB(b *testing.B) {
	benchmarkJSON(b, 1024*1024*1024)
}

// --- Smaller sizes for warmup / comparison ---

func BenchmarkProcessNDJSON_100MB(b *testing.B) {
	benchmarkNDJSON(b, 100*1024*1024)
}

func BenchmarkProcessJSON_100MB(b *testing.B) {
	benchmarkJSON(b, 100*1024*1024)
}

// --- HTTP round-trip benchmark (via test server) ---

func BenchmarkProcessNDJSON_1GB_ViaHTTP(b *testing.B) {
	const sizeBytes = 1024 * 1024 * 1024
	line := fmt.Sprintf(`{"id":1234567890,"name":"benchmark-test","value":0.123456789,"active":true}` + "\n")
	lineBytes := []byte(line)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		written := 0
		for written < sizeBytes {
			n, err := w.Write(lineBytes)
			if err != nil {
				return
			}
			written += n
		}
	}))
	defer ts.Close()

	proc := newBenchProcessor()
	job := &domain.Job{ID: uuid.New(), InputURL: ts.URL}

	b.ResetTimer()
	b.SetBytes(int64(sizeBytes))

	for i := 0; i < b.N; i++ {
		if err := proc.processData(context.Background(), job, slog.Default()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProcessJSON_1GB_ViaHTTP(b *testing.B) {
	const sizeBytes = 1024 * 1024 * 1024
	elem := []byte(`{"id":1234567890,"name":"benchmark-test","value":0.123456789,"active":true}`)
	comma := []byte(",")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		written := 0
		w.Write([]byte("["))
		written++
		first := true
		for written < sizeBytes-1 {
			if !first {
				w.Write(comma)
				written++
			}
			w.Write(elem)
			written += len(elem)
			first = false
		}
		w.Write([]byte("]"))
	}))
	defer ts.Close()

	proc := newBenchProcessor()
	job := &domain.Job{ID: uuid.New(), InputURL: ts.URL}

	b.ResetTimer()
	b.SetBytes(int64(sizeBytes))

	for i := 0; i < b.N; i++ {
		if err := proc.processData(context.Background(), job, slog.Default()); err != nil {
			b.Fatal(err)
		}
	}
}
