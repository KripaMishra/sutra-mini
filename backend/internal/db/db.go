// Package db opens the shared Postgres pool. Neon requires TLS and terminates
// it at the pooler, so the DATABASE_URL (pooled endpoint) is used as given.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect parses databaseURL, builds a small pool, and verifies it with a ping.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// The receipt guard relies on READ COMMITTED statement snapshots (the
	// trigger rejects stronger isolation outright). Pin it here so no code
	// path or role setting can silently change the semantics.
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "read committed"
	cfg.MaxConns = 8
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
