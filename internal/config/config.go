package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	DatabaseURL       string
	ServerPort        string
	WorkerConcurrency int
	WorkerTimeout     time.Duration
	WorkerMaxRetries  int
	LogLevel          string
}

func Load() *Config {
	return &Config{
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://jobapi:jobapi@localhost:5432/jobapi?sslmode=disable"),
		ServerPort:        getEnv("SERVER_PORT", "8080"),
		WorkerConcurrency: getEnvInt("WORKER_CONCURRENCY", 4),
		WorkerTimeout:     time.Duration(getEnvInt("WORKER_TIMEOUT_SEC", 300)) * time.Second,
		WorkerMaxRetries:  getEnvInt("WORKER_MAX_RETRIES", 3),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	s := os.Getenv(key)
	if s == "" {
		return fallback
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return v
}
