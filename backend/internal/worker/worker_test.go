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
	"sync"
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

// TestTwoLoopsPushEachVoucherExactlyOnce runs two full worker loops side by
// side — which the database cannot distinguish from two processes — and
// proves every voucher is posted to accounting exactly once, whichever loop
// claims it.
func TestTwoLoopsPushEachVoucherExactlyOnce(t *testing.T) {
	const n = 8
	ctx := context.Background()
	prefix := "wtest-" + t.Name() + "-%"

	var mu sync.Mutex
	hits := map[string]int{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientRef string `json:"client_ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		hits[req.ClientRef]++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"external_ref": "REF-" + req.ClientRef})
	}))
	defer stub.Close()

	ids := make([]int64, n)
	for i := range ids {
		ids[i] = insertPendingVoucher(t)
	}

	loops := []*Worker{
		New(store.New(testPool), accounting.NewClient(stub.URL, 2*time.Second), Options{
			MaxAttempts: 3, Concurrency: 2, PollInterval: 50 * time.Millisecond, VoucherType: testVoucherType,
		}),
		New(store.New(testPool), accounting.NewClient(stub.URL, 2*time.Second), Options{
			MaxAttempts: 3, Concurrency: 2, PollInterval: 50 * time.Millisecond, VoucherType: testVoucherType,
		}),
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	returned := make(chan error, len(loops))
	for _, w := range loops {
		go func(w *Worker) { returned <- w.Run(runCtx) }(w)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		var pushed int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM vouchers WHERE client_ref LIKE $1 AND status = 'pushed'`, prefix).Scan(&pushed); err != nil {
			t.Fatalf("count pushed: %v", err)
		}
		if pushed == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d vouchers pushed after 15s", pushed, n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	for range loops {
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("a worker loop did not stop after cancel")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != n {
		t.Errorf("accounting was asked about %d distinct vouchers, want %d", len(hits), n)
	}
	for ref, count := range hits {
		if count != 1 {
			t.Errorf("client_ref %s was pushed %d times, want exactly once", ref, count)
		}
	}
	for _, id := range ids {
		var status, clientRef, externalRef string
		if err := testPool.QueryRow(ctx,
			`SELECT status, client_ref, external_ref FROM vouchers WHERE id = $1`, id).
			Scan(&status, &clientRef, &externalRef); err != nil {
			t.Fatalf("load voucher %d: %v", id, err)
		}
		if status != "pushed" {
			t.Errorf("voucher %d status %q, want pushed", id, status)
		}
		if externalRef != "REF-"+clientRef {
			t.Errorf("voucher %d external_ref %q, want %q", id, externalRef, "REF-"+clientRef)
		}
	}
}

// TestSlowPushDoesNotStallSiblings holds one push inside a hanging HTTP call
// and proves the rest of the batch still lands — the mock's five-second hang
// must cost one five-second wait, not five seconds of head-of-line blocking.
func TestSlowPushDoesNotStallSiblings(t *testing.T) {
	ctx := context.Background()

	slowID := insertPendingVoucher(t)
	fastID := insertPendingVoucher(t)
	var slowRef string
	if err := testPool.QueryRow(ctx, `SELECT client_ref FROM vouchers WHERE id = $1`, slowID).Scan(&slowRef); err != nil {
		t.Fatalf("load client_ref: %v", err)
	}

	release := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ClientRef string `json:"client_ref"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ClientRef == slowRef {
			startedOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"external_ref": "REF-" + req.ClientRef})
	}))
	defer stub.Close()

	w := New(store.New(testPool), accounting.NewClient(stub.URL, 5*time.Second), Options{
		MaxAttempts: 3, Concurrency: 4, VoucherType: testVoucherType,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := w.runOnce(ctx); err != nil {
			t.Errorf("runOnce: %v", err)
		}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow push never reached the stub")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		if err := testPool.QueryRow(ctx, `SELECT status FROM vouchers WHERE id = $1`, fastID).Scan(&status); err != nil {
			t.Fatalf("load status: %v", err)
		}
		if status == "pushed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("voucher %d still %q while the slow push is held — the batch is head-of-line blocked", fastID, status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOnce did not finish after the slow call was released")
	}

	var status, externalRef string
	if err := testPool.QueryRow(ctx, `SELECT status, external_ref FROM vouchers WHERE id = $1`, slowID).
		Scan(&status, &externalRef); err != nil {
		t.Fatalf("load slow voucher: %v", err)
	}
	if status != "pushed" || externalRef != "REF-"+slowRef {
		t.Errorf("slow voucher ended status=%q external_ref=%q, want pushed with REF-%s", status, externalRef, slowRef)
	}
}
