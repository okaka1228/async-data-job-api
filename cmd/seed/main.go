package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/femto/async-data-job-api/internal/config"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("database not ready: %w", err)
	}

	seeds := []struct {
		inputURL       string
		idempotencyKey string
	}{
		{"https://jsonplaceholder.typicode.com/posts", "seed-posts"},
		{"https://jsonplaceholder.typicode.com/comments", "seed-comments"},
		{"https://jsonplaceholder.typicode.com/users", "seed-users"},
		{"https://jsonplaceholder.typicode.com/todos", "seed-todos"},
		{"https://jsonplaceholder.typicode.com/albums", "seed-albums"},
	}

	query := `
		INSERT INTO jobs (id, idempotency_key, status, input_url, max_retries, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, 'pending', $2, 3, NOW(), NOW())
		ON CONFLICT (idempotency_key) DO NOTHING
	`

	for _, s := range seeds {
		if _, err := db.Exec(query, s.idempotencyKey, s.inputURL); err != nil {
			logger.Error("seed insert failed", "error", err, "url", s.inputURL)
		} else {
			logger.Info("seed inserted", "url", s.inputURL)
		}
	}

	fmt.Println("seed data complete")
	return nil
}
