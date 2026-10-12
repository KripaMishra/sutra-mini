// Command api serves the Sutra Mini HTTP API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KripaMishra/sutra-mini/backend/internal/ask"
	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/db"
	"github.com/KripaMishra/sutra-mini/backend/internal/httpapi"
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

	askSvc, err := ask.New(ask.Config{
		APIKey:      cfg.LLMAPIKey,
		Model:       cfg.LLMModel,
		BaseURL:     cfg.LLMBaseURL,
		Concurrency: cfg.AskConcurrency,
		QueueDepth:  cfg.AskQueueDepth,
		QueueWait:   cfg.AskQueueWait,
	}, cfg.DatabaseURL)
	if err != nil {
		// /ask is optional: a bad ASK_* value or a database problem on that one
		// read pool must not take receipts and vouchers down with it. The detail
		// stays in the log — a connection error can name the database host.
		log.Printf("ask: disabled: %v", err)
		askSvc = ask.Unavailable("ask is not configured correctly; see the server log")
	}
	if why := askSvc.Disabled(); why != "" {
		log.Printf("ask: disabled: %s", why)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httpapi.New(pool, httpapi.WithAsk(askSvc)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      65 * time.Second, // above /ask's 60s budget so chi can still write its 504
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("api listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	// Stop /ask first: a question is not durable work, so queued ones are
	// refused and in-flight ones cancelled rather than holding shutdown open for
	// up to the /ask budget. The HTTP server then drains what is left.
	askCtx, askCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer askCancel()
	if err := askSvc.Close(askCtx); err != nil {
		log.Printf("ask shutdown: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
