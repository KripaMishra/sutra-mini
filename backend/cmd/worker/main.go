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

	client := accounting.NewClient(cfg.AccountingBaseURL, cfg.AccountingTimeout)
	w := worker.New(store.New(pool), client, worker.Options{
		MaxAttempts: cfg.AccountingMaxAttempts,
		Concurrency: cfg.WorkerConcurrency,
	})

	log.Printf("worker pushing to %s (max %d attempts, concurrency %d)",
		cfg.AccountingBaseURL, cfg.AccountingMaxAttempts, cfg.WorkerConcurrency)
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("worker: %v", err)
	}
	log.Println("worker stopped")
}
