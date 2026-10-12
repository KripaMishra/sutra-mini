// These tests exercise the guarantees the case study grades hardest, plus the
// boundaries around them. They run against the real Postgres from .env — the
// guarantees live in the database, so a fake would test nothing. Each test
// builds its own SKU/party/issue and removes them again when it ends.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/db"
	"github.com/KripaMishra/sutra-mini/backend/internal/httpapi"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is empty — these tests run against the Postgres configured in .env (copy .env.example to .env)")
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

type receiptBody struct {
	Receipt struct {
		ID             int64  `json:"id"`
		IdempotencyKey string `json:"idempotency_key"`
	} `json:"receipt"`
	Voucher struct {
		ID          int64  `json:"id"`
		ClientRef   string `json:"client_ref"`
		AmountPaise int64  `json:"amount_paise"`
	} `json:"voucher"`
	Replayed bool `json:"replayed"`
}

type fixture struct {
	issueID int64
	partyID int64
	skuID   int64
}

// newFixture inserts an isolated SKU + party + open issue and removes them
// again when the test ends.
func newFixture(t *testing.T, issuedQty int) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var f fixture
	if err := testPool.QueryRow(ctx,
		`INSERT INTO skus (code, name, image_url, rate_paise) VALUES ($1, 'Test SKU', '', 1000) RETURNING id`,
		fmt.Sprintf("T-%d", suffix)).Scan(&f.skuID); err != nil {
		t.Fatalf("insert sku: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO parties (name, phone) VALUES ($1, '') RETURNING id`,
		fmt.Sprintf("Test Party %d", suffix)).Scan(&f.partyID); err != nil {
		t.Fatalf("insert party: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on) VALUES ($1, $2, $3, current_date) RETURNING id`,
		f.partyID, f.skuID, issuedQty).Scan(&f.issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		exec := func(sql string, args ...any) { _, _ = testPool.Exec(ctx, sql, args...) }
		exec(`DELETE FROM vouchers WHERE type = 'jobwork_receipt' AND source_id IN (SELECT id FROM jobwork_receipts WHERE issue_id = $1)`, f.issueID)
		exec(`DELETE FROM jobwork_receipts WHERE issue_id = $1`, f.issueID)
		exec(`DELETE FROM jobwork_issues WHERE id = $1`, f.issueID)
		exec(`DELETE FROM parties WHERE id = $1`, f.partyID)
		exec(`DELETE FROM skus WHERE id = $1`, f.skuID)
	})
	return f
}

// postReceiptRaw is safe to call from goroutines (no t.Fatal inside).
func postReceiptRaw(srv *httptest.Server, issueID int64, qty int, key string) (int, receiptBody, error) {
	body, err := json.Marshal(map[string]any{"issue_id": issueID, "qty": qty, "received_on": "2026-10-10"})
	if err != nil {
		return 0, receiptBody{}, err
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/jobwork/receipts", bytes.NewReader(body))
	if err != nil {
		return 0, receiptBody{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)

	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, receiptBody{}, err
	}
	defer resp.Body.Close()

	var parsed receiptBody
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return resp.StatusCode, receiptBody{}, err
	}
	return resp.StatusCode, parsed, nil
}

func postReceipt(t *testing.T, srv *httptest.Server, issueID int64, qty int, key string) (int, receiptBody) {
	t.Helper()
	code, parsed, err := postReceiptRaw(srv, issueID, qty, key)
	if err != nil {
		t.Fatalf("post receipt (%s): %v", key, err)
	}
	return code, parsed
}

func sumReceived(t *testing.T, issueID int64) int64 {
	t.Helper()
	var received int64
	if err := testPool.QueryRow(context.Background(),
		`SELECT COALESCE(sum(qty), 0) FROM jobwork_receipts WHERE issue_id = $1`, issueID).Scan(&received); err != nil {
		t.Fatalf("sum receipts: %v", err)
	}
	return received
}

// TestNoOvershootUnderConcurrentReceipts fires ten receipts at once, each of
// them big enough to fill the whole issue, and proves the database holds the
// invariant: exactly one lands, total received never exceeds issued. Remove
// the guard trigger and this test fails (several inserts would succeed and
// the sum would overshoot).
func TestNoOvershootUnderConcurrentReceipts(t *testing.T) {
	const issued = 10
	f := newFixture(t, issued)
	srv := httptest.NewServer(httpapi.New(testPool))
	defer srv.Close()

	const racers = 10
	codes := make([]int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, err := postReceiptRaw(srv, f.issueID, issued, fmt.Sprintf("race-%d-%d", f.issueID, i))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				codes[i] = -1
				return
			}
			codes[i] = code
		}(i)
	}
	wg.Wait()

	created, conflicts := 0, 0
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("request %d: unexpected status %d", i, code)
		}
	}
	if created != 1 {
		t.Errorf("exactly one of %d concurrent receipts should fit an issue of %d; got %d created, %d conflicts",
			racers, issued, created, conflicts)
	}

	if received := sumReceived(t, f.issueID); received > issued {
		t.Fatalf("OVERSHOOT: received %d exceeds issued %d", received, issued)
	} else if received != issued {
		t.Errorf("received %d, want %d", received, issued)
	}
}

// TestIdempotentReplayCreatesNothing posts one key twice sequentially and a
// second key twice concurrently, and proves every replay returns the stored
// receipt and creates nothing twice. Without the UNIQUE key and the replay
// path, duplicate receipts would appear and the counts below would fail.
func TestIdempotentReplayCreatesNothing(t *testing.T) {
	f := newFixture(t, 100)
	srv := httptest.NewServer(httpapi.New(testPool))
	defer srv.Close()

	key := fmt.Sprintf("replay-%d", f.issueID)

	code1, first := postReceipt(t, srv, f.issueID, 7, key)
	if code1 != http.StatusCreated {
		t.Fatalf("first post: status %d, want 201", code1)
	}
	if first.Replayed {
		t.Errorf("first post must not be a replay")
	}
	if first.Voucher.AmountPaise != 7000 {
		t.Errorf("voucher amount %d paise, want 7000 (7 × 1000)", first.Voucher.AmountPaise)
	}

	code2, second := postReceipt(t, srv, f.issueID, 7, key)
	if code2 != http.StatusOK {
		t.Fatalf("replay: status %d, want 200", code2)
	}
	if !second.Replayed {
		t.Errorf("replay must be flagged replayed=true")
	}
	if second.Receipt.ID != first.Receipt.ID || second.Voucher.ID != first.Voucher.ID {
		t.Errorf("replay returned a different receipt/voucher (%d/%d, want %d/%d)",
			second.Receipt.ID, second.Voucher.ID, first.Receipt.ID, first.Voucher.ID)
	}

	// Same key, different body: rejected loudly, never silently echoed.
	if code, _ := postReceipt(t, srv, f.issueID, 8, key); code != http.StatusConflict {
		t.Errorf("same key with a different body: status %d, want 409", code)
	}

	// Two simultaneous first-time uses of one key must collapse to a single
	// receipt: one request creates it, the other replays it.
	const raceKey = "race-replay"
	var (
		raceCodes  [2]int
		raceBodies [2]receiptBody
		raceWait   sync.WaitGroup
	)
	for i := 0; i < 2; i++ {
		raceWait.Add(1)
		go func(i int) {
			defer raceWait.Done()
			code, parsed, err := postReceiptRaw(srv, f.issueID, 3, raceKey)
			if err != nil {
				t.Errorf("concurrent post %d: %v", i, err)
				raceCodes[i] = -1
				return
			}
			raceCodes[i], raceBodies[i] = code, parsed
		}(i)
	}
	raceWait.Wait()

	created, replayed := 0, 0
	for i, code := range raceCodes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			replayed++
			if !raceBodies[i].Replayed {
				t.Errorf("status 200 on a duplicate must carry replayed=true")
			}
		default:
			t.Errorf("concurrent post %d: unexpected status %d", i, code)
		}
	}
	if created != 1 || replayed != 1 {
		t.Errorf("concurrent duplicates: got %d created / %d replayed, want 1 / 1", created, replayed)
	}
	if raceBodies[0].Receipt.ID != raceBodies[1].Receipt.ID {
		t.Errorf("concurrent duplicates got different receipts: %d vs %d",
			raceBodies[0].Receipt.ID, raceBodies[1].Receipt.ID)
	}

	// Database truth: one row per key, one voucher per receipt.
	checks := []struct {
		name string
		sql  string
		arg  any
		want int
	}{
		{"receipts for the replayed key", `SELECT count(*) FROM jobwork_receipts WHERE idempotency_key = $1`, key, 1},
		{"vouchers for the replayed receipt", `SELECT count(*) FROM vouchers WHERE type = 'jobwork_receipt' AND source_id = $1`, first.Receipt.ID, 1},
		{"receipts for the concurrent key", `SELECT count(*) FROM jobwork_receipts WHERE idempotency_key = $1`, raceKey, 1},
		{"vouchers for the concurrent receipt", `SELECT count(*) FROM vouchers WHERE type = 'jobwork_receipt' AND source_id = $1`, raceBodies[0].Receipt.ID, 1},
	}
	for _, check := range checks {
		var got int
		if err := testPool.QueryRow(context.Background(), check.sql, check.arg).Scan(&got); err != nil {
			t.Fatalf("%s: %v", check.name, err)
		}
		if got != check.want {
			t.Errorf("%s: got %d, want %d", check.name, got, check.want)
		}
	}

	if received := sumReceived(t, f.issueID); received != 10 {
		t.Errorf("received %d, want 10 (7 + 3, nothing doubled)", received)
	}
}

// TestReceiptsFillToTheBoundaryThenStop walks an issue to exactly full,
// receipt by receipt, proves the next unit is rejected, and proves the guard
// fails safe at isolation levels stronger than READ COMMITTED instead of
// silently skipping its check.
func TestReceiptsFillToTheBoundaryThenStop(t *testing.T) {
	const issued = 10
	f := newFixture(t, issued)
	srv := httptest.NewServer(httpapi.New(testPool))
	defer srv.Close()

	if code, _ := postReceipt(t, srv, f.issueID, 6, "fill-6"); code != http.StatusCreated {
		t.Fatalf("6 of %d: status %d, want 201", issued, code)
	}
	if code, _ := postReceipt(t, srv, f.issueID, 4, "fill-4"); code != http.StatusCreated {
		t.Fatalf("4 of %d: status %d, want 201 (exactly fills the issue)", issued, code)
	}
	if code, _ := postReceipt(t, srv, f.issueID, 1, "fill-1"); code != http.StatusConflict {
		t.Fatalf("1 over: status %d, want 409", code)
	}

	if received := sumReceived(t, f.issueID); received != issued {
		t.Errorf("received %d, want %d", received, issued)
	}

	// A REPEATABLE READ writer keeps a stale snapshot, which the guard cannot
	// see through — so it refuses rather than risking a silent overshoot.
	tx, err := testPool.BeginTx(context.Background(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin repeatable read: %v", err)
	}
	_, err = tx.Exec(context.Background(),
		`INSERT INTO jobwork_receipts (issue_id, qty, received_on, idempotency_key) VALUES ($1, 1, current_date, 'isolation-check')`,
		f.issueID)
	_ = tx.Rollback(context.Background())
	if err == nil || !strings.Contains(err.Error(), "READ COMMITTED") {
		t.Errorf("repeatable-read insert should be rejected by the guard, got %v", err)
	}
}

// TestIssueProgressAndVoucherRetry covers the read and retry endpoints: the
// issued/received/outstanding math, the status filter, and the failed →
// pending retry contract (including the 409 for non-failed vouchers).
func TestIssueProgressAndVoucherRetry(t *testing.T) {
	type voucherRow struct {
		ID        int64  `json:"id"`
		Status    string `json:"status"`
		Attempts  int    `json:"attempts"`
		LastError string `json:"last_error"`
	}

	f := newFixture(t, 20)
	srv := httptest.NewServer(httpapi.New(testPool))
	defer srv.Close()

	code, created := postReceipt(t, srv, f.issueID, 5, "progress-1")
	if code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201", code)
	}

	var summary struct {
		Issued      int `json:"issued"`
		Received    int `json:"received"`
		Outstanding int `json:"outstanding"`
	}
	resp, err := srv.Client().Get(fmt.Sprintf("%s/issues/%d", srv.URL, f.issueID))
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get issue: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	if summary.Issued != 20 || summary.Received != 5 || summary.Outstanding != 15 {
		t.Errorf("issue math: got issued=%d received=%d outstanding=%d, want 20/5/15",
			summary.Issued, summary.Received, summary.Outstanding)
	}

	listPending := func() []voucherRow {
		t.Helper()
		res, err := srv.Client().Get(srv.URL + "/vouchers?status=pending")
		if err != nil {
			t.Fatalf("list pending: %v", err)
		}
		defer res.Body.Close()
		var list struct {
			Vouchers []voucherRow `json:"vouchers"`
		}
		if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return list.Vouchers
	}

	found := false
	for _, v := range listPending() {
		if v.ID == created.Voucher.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("voucher %d missing from status=pending list", created.Voucher.ID)
	}

	retry := func() int {
		t.Helper()
		res, err := srv.Client().Post(fmt.Sprintf("%s/vouchers/%d/retry", srv.URL, created.Voucher.ID), "", nil)
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if code := retry(); code != http.StatusConflict {
		t.Errorf("retry on a pending voucher: status %d, want 409", code)
	}

	// Park it as failed (what the worker does after the attempt budget runs
	// out), then retry: pending again with a fresh budget, reason preserved.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE vouchers SET status = 'failed', attempts = 5, last_error = 'injected failure' WHERE id = $1`,
		created.Voucher.ID); err != nil {
		t.Fatalf("park voucher: %v", err)
	}
	if code := retry(); code != http.StatusOK {
		t.Fatalf("retry on a failed voucher: status %d, want 200", code)
	}

	seen := false
	for _, v := range listPending() {
		if v.ID != created.Voucher.ID {
			continue
		}
		seen = true
		if v.Status != "pending" || v.Attempts != 0 {
			t.Errorf("after retry: status=%q attempts=%d, want pending/0", v.Status, v.Attempts)
		}
		if v.LastError != "injected failure" {
			t.Errorf("after retry: last_error=%q, want the previous reason to stay visible", v.LastError)
		}
	}
	if !seen {
		t.Errorf("retried voucher %d not back in the pending list", created.Voucher.ID)
	}
}
