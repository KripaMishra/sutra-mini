// Command worker pushes pending vouchers to the accounting API until stopped.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kripa5661/srs/backend/internal/accounting"
	"github.com/kripa5661/srs/backend/internal/config"
	"github.com/kripa5661/srs/backend/internal/db"
	"github.com/kripa5661/srs/backend/internal/store"
	"github.com/kripa5661/srs/backend/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is not set — copy .env.example to .env and fill it in")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if cfg.AccountingTimeout <= 0 {
		log.Fatal("ACCOUNTING_TIMEOUT must be positive — a zero timeout means unbounded hangs")
	}
	if cfg.WorkerLease <= cfg.AccountingTimeout {
		log.Fatalf("WORKER_LEASE (%s) must be longer than ACCOUNTING_TIMEOUT (%s) so a slow push cannot outlive its lease",
			cfg.WorkerLease, cfg.AccountingTimeout)
	}

	client := accounting.NewClient(cfg.AccountingBaseURL, cfg.AccountingTimeout)
	w := worker.New(store.New(pool), client, worker.Options{
		MaxAttempts: cfg.AccountingMaxAttempts,
		Concurrency: cfg.WorkerConcurrency,
		Lease:       cfg.WorkerLease,
	})

	log.Printf("worker pushing to %s (timeout %s, lease %s, max %d attempts, concurrency %d)",
		cfg.AccountingBaseURL, cfg.AccountingTimeout, cfg.WorkerLease, cfg.AccountingMaxAttempts, cfg.WorkerConcurrency)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("worker: %v", err)
	}
	log.Println("worker stopped")
}
