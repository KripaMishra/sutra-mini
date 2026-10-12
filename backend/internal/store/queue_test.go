// These tests run against the real Postgres from .env — leases are database
// behaviour, so a fake would test nothing. Fixtures use their own voucher
// type ('qtest') and claims are type-scoped, so parallel test binaries can
// never claim each other's rows.
package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/db"
	"github.com/KripaMishra/sutra-mini/backend/internal/store"
)

const testVoucherType = "qtest"

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

func insertVoucher(t *testing.T, status string) int64 {
	t.Helper()
	ref := fmt.Sprintf("qtest-%s-%d", t.Name(), time.Now().UnixNano())
	var id int64
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO vouchers (type, source_id, amount_paise, status, client_ref)
		VALUES ($1, $2, 100, $3, $4)
		RETURNING id`, testVoucherType, time.Now().UnixNano(), status, ref).Scan(&id); err != nil {
		t.Fatalf("insert voucher: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM vouchers WHERE client_ref = $1`, ref)
	})
	return id
}

func TestClaimLeasesAreDisjointAndTokenGuarded(t *testing.T) {
	ctx := context.Background()
	s := store.New(testPool)

	ids := []int64{insertVoucher(t, "pending"), insertVoucher(t, "pending"), insertVoucher(t, "pending")}

	// Two concurrent claimers must never receive the same voucher.
	var wg sync.WaitGroup
	results := make([][]store.ClaimedVoucher, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, err := s.ClaimVouchers(ctx, testVoucherType, 10, 60)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			results[i] = claimed
		}(i)
	}
	wg.Wait()

	seen := map[int64]string{} // voucher id -> lease token
	var all []store.ClaimedVoucher
	for _, list := range results {
		for _, v := range list {
			if prev, dup := seen[v.ID]; dup {
				t.Errorf("voucher %d was claimed by both workers (tokens %q and %q)", v.ID, prev, v.LeaseToken)
			}
			seen[v.ID] = v.LeaseToken
			all = append(all, v)
		}
	}
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			t.Errorf("voucher %d was not claimed by either worker", id)
		}
	}
	if len(all) == 0 {
		t.Fatal("no vouchers were claimed at all")
	}
	victim := all[0]

	// A stale token cannot complete anything.
	if ok, err := s.MarkPushed(ctx, victim.ID, "not-the-token", "MOCK-STALE"); err != nil {
		t.Fatalf("mark pushed (stale): %v", err)
	} else if ok {
		t.Errorf("MarkPushed accepted a stale lease token")
	}
	if ok, err := s.MarkRetry(ctx, victim.ID, "not-the-token", 1, time.Second, "stale"); err != nil {
		t.Fatalf("mark retry (stale): %v", err)
	} else if ok {
		t.Errorf("MarkRetry accepted a stale lease token")
	}
	assertVoucherStatus(t, victim.ID, "pending")

	// An expired lease becomes claimable again, with a fresh token.
	if _, err := testPool.Exec(ctx,
		`UPDATE vouchers SET lease_until = now() - interval '1 second' WHERE id = $1`, victim.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, err := s.ClaimVouchers(ctx, testVoucherType, 1, 60)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != victim.ID {
		t.Fatalf("expected the expired voucher to be reclaimable, got %+v", reclaimed)
	}
	if reclaimed[0].LeaseToken == victim.LeaseToken {
		t.Errorf("reclaim reused the old lease token")
	}

	// The fresh token can complete it.
	if ok, err := s.MarkPushed(ctx, reclaimed[0].ID, reclaimed[0].LeaseToken, "MOCK-OK"); err != nil {
		t.Fatalf("mark pushed: %v", err)
	} else if !ok {
		t.Errorf("MarkPushed with the current token was refused")
	}
	var status, ref string
	if err := testPool.QueryRow(ctx, `SELECT status, external_ref FROM vouchers WHERE id = $1`, victim.ID).
		Scan(&status, &ref); err != nil {
		t.Fatalf("load voucher: %v", err)
	}
	if status != "pushed" || ref != "MOCK-OK" {
		t.Errorf("voucher after push: status=%q ref=%q, want pushed/MOCK-OK", status, ref)
	}
}

func assertVoucherStatus(t *testing.T, id int64, want string) {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM vouchers WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("load voucher status: %v", err)
	}
	if status != want {
		t.Errorf("voucher %d status %q, want %q", id, status, want)
	}
}
