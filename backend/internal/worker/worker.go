// Package worker pushes pending vouchers to the accounting API: leases from
// the queue, pushes concurrently with per-request timeouts, and retries with
// backoff until a voucher is pushed or parked as failed.
package worker

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/kripa5661/srs/backend/internal/accounting"
	"github.com/kripa5661/srs/backend/internal/store"
)

type Options struct {
	MaxAttempts  int
	Concurrency  int
	PollInterval time.Duration
	Lease        time.Duration
	VoucherType  string
}

type Worker struct {
	store        *store.Store
	accounting   *accounting.Client
	maxAttempts  int
	concurrency  int
	poll         time.Duration
	leaseSeconds int
	voucherType  string
	logger       *log.Logger
}

func New(st *store.Store, ac *accounting.Client, opts Options) *Worker {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.Lease <= 0 {
		opts.Lease = 15 * time.Second
	}
	if opts.VoucherType == "" {
		opts.VoucherType = "jobwork_receipt"
	}
	return &Worker{
		store:        st,
		accounting:   ac,
		maxAttempts:  opts.MaxAttempts,
		concurrency:  opts.Concurrency,
		poll:         opts.PollInterval,
		leaseSeconds: int(opts.Lease / time.Second),
		voucherType:  opts.VoucherType,
		logger:       log.New(os.Stdout, "worker lease="+opts.Lease.String()+" ", log.LstdFlags),
	}
}

// Run pushes until ctx is canceled. Any number of Run loops may run at once —
// in this process or another — because claiming is a SKIP LOCKED lease: two
// workers never push the same voucher simultaneously, and a worker that is
// killed mid-push leaves only an expiring lease behind.
func (w *Worker) Run(ctx context.Context) error {
	for {
		claimed, err := w.runOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.logger.Printf("claim: %v", err)
			if !sleepCtx(ctx, w.poll) {
				return ctx.Err()
			}
			continue
		}
		if claimed == 0 && !sleepCtx(ctx, w.poll) {
			return ctx.Err()
		}
	}
}

// runOnce claims one batch and pushes it, returning how many vouchers were
// claimed so the caller can keep going while there is work.
func (w *Worker) runOnce(ctx context.Context) (int, error) {
	claimed, err := w.store.ClaimVouchers(ctx, w.voucherType, w.concurrency, w.leaseSeconds)
	if err != nil {
		return 0, err
	}
	if len(claimed) == 0 {
		return 0, nil
	}
	w.pushAll(ctx, claimed)
	return len(claimed), nil
}

// pushAll pushes the batch concurrently — the fan-out is bounded by the claim
// size — so one slow call (the mock's 5s hang) holds up only itself.
func (w *Worker) pushAll(ctx context.Context, vouchers []store.ClaimedVoucher) {
	var wg sync.WaitGroup
	for _, v := range vouchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.push(ctx, v)
		}()
	}
	wg.Wait()
}

func (w *Worker) push(ctx context.Context, v store.ClaimedVoucher) {
	attempt := v.Attempts + 1

	result, err := w.accounting.Push(ctx, v.ClientRef, v.AmountPaise)
	if err == nil {
		if ok, uerr := w.store.MarkPushed(ctx, v.ID, v.LeaseToken, result.ExternalRef); uerr != nil {
			w.logger.Printf("voucher %d: mark pushed: %v", v.ID, uerr)
		} else if !ok {
			w.logger.Printf("voucher %d: push succeeded but the lease was lost; another worker reconciles via the 409 path", v.ID)
		} else if result.Duplicate {
			w.logger.Printf("voucher %d: duplicate reconciled, external_ref %s", v.ID, result.ExternalRef)
		} else {
			w.logger.Printf("voucher %d: pushed, external_ref %s", v.ID, result.ExternalRef)
		}
		return
	}

	// Shutting down mid-push: leave the lease to expire so a restarted worker
	// re-claims it. Recovering from a crash between HTTP call and DB update is
	// exactly what the accounting side's 409 path exists for.
	if ctx.Err() != nil {
		w.logger.Printf("voucher %d: interrupted mid-push (%v); lease will expire", v.ID, ctx.Err())
		return
	}

	msg := err.Error()
	if attempt >= w.maxAttempts {
		if ok, uerr := w.store.MarkFailed(ctx, v.ID, v.LeaseToken, attempt, msg); uerr != nil {
			w.logger.Printf("voucher %d: mark failed: %v", v.ID, uerr)
		} else if !ok {
			w.logger.Printf("voucher %d: lease lost before parking it; another worker owns it now", v.ID)
		} else {
			w.logger.Printf("voucher %d: parked as failed after %d attempts: %s", v.ID, attempt, msg)
		}
		return
	}

	delay := backoff(attempt)
	if ok, uerr := w.store.MarkRetry(ctx, v.ID, v.LeaseToken, attempt, delay, msg); uerr != nil {
		w.logger.Printf("voucher %d: mark retry: %v", v.ID, uerr)
	} else if !ok {
		w.logger.Printf("voucher %d: lease lost before scheduling a retry; another worker owns it now", v.ID)
	} else {
		w.logger.Printf("voucher %d: attempt %d failed (%s), retrying in %s", v.ID, attempt, msg, delay)
	}
}

// backoff doubles per attempt (1s, 2s, 4s, …), capped at 30s.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 {
		return 30 * time.Second
	}
	d := time.Second << uint(attempt-1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
