package main

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	concurrency := 10
	duration := 2 * time.Second
	targetURL := "http://localhost:8080/api/v1/jobs"

	fmt.Printf("Starting benchmark: %d concurrent workers for %s\n", concurrency, duration)

	var successCount int64
	var errorCount int64

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(concurrency)

	start := time.Now()

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 2 * time.Second}
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				// Generate unique idempotency key
				body := []byte(fmt.Sprintf(`{"input_url": "https://jsonplaceholder.typicode.com/posts?dummy=%d", "idempotency_key": "bench-%d"}`, 
					rand.Int(), rand.Int63()))

				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")

				resp, err := client.Do(req)
				if err != nil {
					atomic.AddInt64(&errorCount, 1)
					continue
				}
				resp.Body.Close()

				if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
					atomic.AddInt64(&successCount, 1)
				} else {
					atomic.AddInt64(&errorCount, 1)
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start).Seconds()

	reqsPerSec := float64(successCount) / elapsed

	fmt.Printf("\n--- Benchmark Results ---\n")
	fmt.Printf("Total time:       %.2f seconds\n", elapsed)
	fmt.Printf("Successful reqs:  %d\n", successCount)
	fmt.Printf("Failed reqs:      %d\n", errorCount)
	fmt.Printf("Requests/sec:     %.2f req/s\n", reqsPerSec)
	fmt.Println("-------------------------")
}
