// Package config loads runtime settings from the environment, falling back to
// a .env file for local development.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL           string
	Port                  string
	AccountingBaseURL     string
	AccountingTimeout     time.Duration
	AccountingMaxAttempts int
	WorkerConcurrency     int
}

// Load reads configuration from the process environment. Values from a local
// .env file fill in anything that is not already set; real environment values
// always win. The .env search walks up to three directories above the working
// directory, so commands run from the repo root, backend/, or a nested
// package directory (as in go test ./...) all find it.
func Load() (*Config, error) {
	loadDotEnv(".env")
	loadDotEnv("../.env")
	loadDotEnv("../../.env")
	loadDotEnv("../../../.env")

	cfg := &Config{
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		Port:              envOr("PORT", "8080"),
		AccountingBaseURL: envOr("ACCOUNTING_BASE_URL", "http://localhost:8090"),
	}

	var err error
	if cfg.AccountingTimeout, err = durationOr("ACCOUNTING_TIMEOUT", 8*time.Second); err != nil {
		return nil, err
	}
	if cfg.AccountingMaxAttempts, err = intOr("ACCOUNTING_MAX_ATTEMPTS", 5); err != nil {
		return nil, err
	}
	if cfg.WorkerConcurrency, err = intOr("WORKER_CONCURRENCY", 4); err != nil {
		return nil, err
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func intOr(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// loadDotEnv sets KEY=VALUE pairs from a dotenv file without overriding
// variables that are already present in the environment.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}
