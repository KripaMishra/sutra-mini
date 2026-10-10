// Worker tests run against the real Postgres from .env with a stub accounting
// API in-process. Fixtures use their own voucher type ('wtest') and claims
// are type-scoped, so parallel test binaries can never claim each other's
// rows. This package is white-box so the tests can drive runOnce directly.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kripa5661/srs/backend/internal/accounting"
	"github.com/kripa5661/srs/backend/internal/config"
	"github.com/kripa5661/srs/backend/internal/db"
	"github.com/kripa5661/srs/backend/internal/store"
)

const testVoucherType = "wtest"

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is empty — these tests run against the Postgres configured in .env")
		os.Exit(1)
	}
	pool, err := db.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		os.Exit(1)
	}
	testPool = pool
	code := m.Run()
	pool.Close()
	os.Exit(code)
}

func insertPendingVoucher(t *testing.T) int64 {
	t.Helper()
	ref := fmt.Sprintf("wtest-%s-%d", t.Name(), time.Now().UnixNano())
	var id int64
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO vouchers (type, source_id, amount_paise, status, client_ref)
		VALUES ($1, $2, 500, 'pending', $3)
		RETURNING id`, testVoucherType, time.Now().UnixNano(), ref).Scan(&id); err != nil {
		t.Fatalf("insert voucher: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vouchers WHERE client_ref = $1`, ref)
	})
	return id
}

// TestRunOnceReconciles409AndMarksPushed reproduces the crash window that
// makes the 409 path necessary: accounting already holds the voucher, the
// worker's next push is answered with the original reference, and the voucher
// lands as pushed — reconciled, not failed, and posted exactly once.
func TestRunOnceReconciles409AndMarksPushed(t *testing.T) {
	var hits atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"external_ref": "MOCK-ORIGINAL"})
	}))
	defer stub.Close()

	id := insertPendingVoucher(t)
	w := New(store.New(testPool), accounting.NewClient(stub.URL, 2*time.Second), Options{
		MaxAttempts: 3,
		Concurrency: 4,
		VoucherType: testVoucherType,
	})
	if _, err := w.runOnce(context.Background()); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	var status, ref string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status, external_ref FROM vouchers WHERE id = $1`, id).Scan(&status, &ref); err != nil {
		t.Fatalf("load voucher: %v", err)
	}
	if status != "pushed" {
		t.Errorf("status %q, want pushed (a 409 duplicate must reconcile, not fail)", status)
	}
	if ref != "MOCK-ORIGINAL" {
		t.Errorf("external_ref %q, want MOCK-ORIGINAL", ref)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("accounting saw %d requests, want 1", got)
	}
}

// TestVoucherParksAsFailedAfterMaxAttempts drives the attempt budget to its
// end: the voucher ends failed, with the attempt count and the reason on it.
func TestVoucherParksAsFailedAfterMaxAttempts(t *testing.T) {
	var failures atomic.Int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failures.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "injected"})
	}))
	defer stub.Close()

	id := insertPendingVoucher(t)
	w := New(store.New(testPool), accounting.NewClient(stub.URL, 2*time.Second), Options{
		MaxAttempts: 2,
		Concurrency: 4,
		VoucherType: testVoucherType,
	})

	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if _, err := w.runOnce(ctx); err != nil {
			t.Fatalf("runOnce %d: %v", i, err)
		}
		var status string
		if err := testPool.QueryRow(ctx, `SELECT status FROM vouchers WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("load status: %v", err)
		}
		if status == "failed" {
			break
		}
		if _, err := testPool.Exec(ctx, `UPDATE vouchers SET next_attempt_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("make due: %v", err)
		}
	}

	var status, lastErr string
	var attempts int
	if err := testPool.QueryRow(ctx, `SELECT status, attempts, last_error FROM vouchers WHERE id = $1`, id).
		Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("load voucher: %v", err)
	}
	if status != "failed" {
		t.Fatalf("status %q, want failed after several passes", status)
	}
	if attempts != 2 {
		t.Errorf("attempts %d, want 2 (parked at the budget)", attempts)
	}
	if lastErr == "" {
		t.Errorf("last_error is empty; the failure reason must be recorded")
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second,
	}
	for i, wnt := range want {
		if got := backoff(i + 1); got != wnt {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, wnt)
		}
	}
}
