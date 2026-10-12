// These tests run against the real Postgres from .env, like the other suites in
// this repo: the read tools are only as trustworthy as the SQL under them, so a
// fake would test nothing. Each test builds its own SKU/party/issue rows and
// removes them again afterwards, so the suite stays safe to run beside a live
// api/worker stack.
package ask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gorm.io/gorm"

	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/db"
)

var (
	testPool *pgxpool.Pool
	testRead *readDB
)

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

	readDB, err := openReadDB(cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read database:", err)
		os.Exit(1)
	}
	testRead = readDB

	code := m.Run()
	_ = readDB.Close()
	pool.Close()
	os.Exit(code)
}

type askFixture struct {
	skuID     int64
	partyID   int64
	issueID   int64
	skuCode   string
	partyName string
	// Fragments are unique slices of the generated names, safe to use as
	// search terms even while other packages' fixtures are in the same DB.
	skuFragment   string
	partyFragment string
}

// newAskFixture inserts an isolated SKU + party + open issue. The optional
// token lands in the SKU name so a test can create several SKUs that share a
// search fragment.
func newAskFixture(t *testing.T, issuedQty int, token string) askFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	name := fmt.Sprintf("Ask Test SKU %d", suffix)
	if token != "" {
		name = fmt.Sprintf("Ask Test SKU %s %d", token, suffix)
	}

	// The last ten digits of the nanosecond suffix are unique enough to search
	// by while other suites run in parallel.
	digits := fmt.Sprintf("%d", suffix)
	fragment := digits[len(digits)-10:]

	f := askFixture{
		skuCode:       fmt.Sprintf("ASK-%d", suffix),
		partyName:     fmt.Sprintf("Ask Test Worker %d", suffix),
		skuFragment:   fragment,
		partyFragment: fragment,
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO skus (code, name, image_url, rate_paise) VALUES ($1, $2, '', 1000) RETURNING id`,
		f.skuCode, name).Scan(&f.skuID); err != nil {
		t.Fatalf("insert sku: %v", err)
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO parties (name, phone) VALUES ($1, '') RETURNING id`,
		f.partyName).Scan(&f.partyID); err != nil {
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
		exec(`DELETE FROM jobwork_receipts WHERE issue_id = $1`, f.issueID)
		exec(`DELETE FROM jobwork_issues WHERE id = $1`, f.issueID)
		exec(`DELETE FROM parties WHERE id = $1`, f.partyID)
		exec(`DELETE FROM skus WHERE id = $1`, f.skuID)
	})
	return f
}

func (f askFixture) addReceipt(t *testing.T, qty int, tag string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO jobwork_receipts (issue_id, qty, received_on, idempotency_key) VALUES ($1, $2, current_date, $3)`,
		f.issueID, qty, fmt.Sprintf("ask-%d-%s-%d", f.issueID, tag, time.Now().UnixNano())); err != nil {
		t.Fatalf("insert receipt: %v", err)
	}
}

func TestStockIsIssuedMinusReceived(t *testing.T) {
	f := newAskFixture(t, 120, "")
	f.addReceipt(t, 45, "a")
	f.addReceipt(t, 25, "b")

	ctx := context.Background()
	result, err := testRead.stock(ctx, f.skuCode, f.partyName)
	if err != nil {
		t.Fatalf("stock: %v", err)
	}
	if result.Count != 1 {
		t.Fatalf("count = %d, want 1 (rows: %+v)", result.Count, result.Rows)
	}
	row := result.Rows[0]
	if row.Issued != 120 || row.Received != 70 || row.StillWith != 50 {
		t.Fatalf("row = %+v, want issued 120, received 70, still_with 50", row)
	}
	if result.TotalStillWith != 50 {
		t.Fatalf("total = %d, want 50", result.TotalStillWith)
	}

	// Fragments and case must resolve the same pair.
	lower, err := testRead.stock(ctx, strings.ToLower(f.skuFragment), strings.ToLower(f.partyFragment))
	if err != nil {
		t.Fatalf("stock by fragments: %v", err)
	}
	if lower.Count != 1 || lower.Rows[0].StillWith != 50 {
		t.Fatalf("fragment lookup = %+v, want the same single row", lower.Rows)
	}
}

func TestStockRollups(t *testing.T) {
	f := newAskFixture(t, 30, "")
	f.addReceipt(t, 10, "a")

	ctx := context.Background()

	// SKU only: every worker holding it (just the fixture here).
	bySKU, err := testRead.stock(ctx, f.skuFragment, "")
	if err != nil {
		t.Fatalf("stock by sku: %v", err)
	}
	if bySKU.Count != 1 || bySKU.Rows[0].JobWorker != f.partyName || bySKU.Rows[0].StillWith != 20 {
		t.Fatalf("sku rollup = %+v", bySKU.Rows)
	}

	// Worker only: every SKU they hold.
	byWorker, err := testRead.stock(ctx, "", f.partyFragment)
	if err != nil {
		t.Fatalf("stock by worker: %v", err)
	}
	if byWorker.Count != 1 || byWorker.Rows[0].SKUCode != f.skuCode || byWorker.Rows[0].StillWith != 20 {
		t.Fatalf("worker rollup = %+v", byWorker.Rows)
	}

	// Neither: refuse rather than dump the whole ledger.
	if _, err := testRead.stock(ctx, "", ""); err == nil {
		t.Fatal("stock with no filters should be refused")
	}
}

func TestMissingEntitiesComeBackEmptyWithAHint(t *testing.T) {
	ctx := context.Background()
	missing := fmt.Sprintf("nope-%d", time.Now().UnixNano())

	stock, err := testRead.stock(ctx, missing, "")
	if err != nil {
		t.Fatalf("stock: %v", err)
	}
	if stock.Count != 0 || stock.Note == "" || len(stock.Rows) != 0 {
		t.Fatalf("missing sku lookup = %+v, want empty rows and a note", stock)
	}

	skus, err := testRead.findSkus(ctx, missing)
	if err != nil {
		t.Fatalf("findSkus: %v", err)
	}
	if skus.Count != 0 || skus.Note == "" || skus.Matches == nil {
		t.Fatalf("findSkus = %+v, want empty matches, a note and a non-nil list", skus)
	}

	workers, err := testRead.findJobWorkers(ctx, missing)
	if err != nil {
		t.Fatalf("findJobWorkers: %v", err)
	}
	if workers.Count != 0 || workers.Note == "" || workers.Matches == nil {
		t.Fatalf("findJobWorkers = %+v, want empty matches, a note and a non-nil list", workers)
	}
}

func TestFindSkusReturnsEveryMatch(t *testing.T) {
	token := fmt.Sprintf("group-%d", time.Now().UnixNano())
	first := newAskFixture(t, 10, token)
	second := newAskFixture(t, 20, token)

	matches, err := testRead.findSkus(context.Background(), token)
	if err != nil {
		t.Fatalf("findSkus: %v", err)
	}
	if matches.Count != 2 {
		t.Fatalf("count = %d, want both SKUs sharing the token: %+v", matches.Count, matches.Matches)
	}
	if matches.Note == "" {
		t.Fatal("an ambiguous lookup should carry a note telling the model to disambiguate")
	}
	codes := map[string]bool{matches.Matches[0].Code: true, matches.Matches[1].Code: true}
	if !codes[first.skuCode] || !codes[second.skuCode] {
		t.Fatalf("matches = %+v, want %s and %s", matches.Matches, first.skuCode, second.skuCode)
	}
}

func TestFindersEscapeLikeWildcards(t *testing.T) {
	ctx := context.Background()
	// Unescaped, "%" would match every row and "_" every one-character name.
	for _, wildcard := range []string{"%", "_"} {
		matches, err := testRead.findSkus(ctx, wildcard)
		if err != nil {
			t.Fatalf("findSkus(%q): %v", wildcard, err)
		}
		if matches.Count != 0 {
			t.Fatalf("findSkus(%q) matched %d rows; wildcards are not escaped", wildcard, matches.Count)
		}
	}
}

func TestReadOnlyTransactionRefusesWrites(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("ask-readonly-%d", time.Now().UnixNano())

	err := testRead.readOnly(ctx, func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO parties (name) VALUES (?)`, name).Error
	})
	if err == nil {
		t.Fatal("a write inside the read-only tool transaction succeeded")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected Postgres to refuse the write as read-only, got: %v", err)
	}

	var count int64
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM parties WHERE name = $1`, name).Scan(&count); err != nil {
		t.Fatalf("count parties: %v", err)
	}
	if count != 0 {
		t.Fatalf("the refused write left %d rows behind", count)
	}

	// The same write through the raw handle succeeds: the guard is the read-only
	// transaction per tool call, not (yet) a database role. That is exactly why
	// every finder above goes through readOnly.
	if err := testRead.db.WithContext(ctx).Exec(`INSERT INTO parties (name) VALUES (?)`, name).Error; err != nil {
		t.Fatalf("control insert: %v", err)
	}
	defer func() { _, _ = testPool.Exec(ctx, `DELETE FROM parties WHERE name = $1`, name) }()

	sentinel := errors.New("sentinel")
	if err := testRead.readOnly(ctx, func(tx *gorm.DB) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback errors must propagate unchanged, got: %v", err)
	}
}
