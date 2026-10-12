package mockacct

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixed(v float64) func() float64 {
	return func() float64 { return v }
}

func postVoucher(t *testing.T, srv *httptest.Server, ref string) (int, map[string]string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/vouchers", "application/json",
		strings.NewReader(`{"client_ref":"`+ref+`","amount_paise":123}`))
	if err != nil {
		t.Fatalf("post voucher: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, body
}

// TestClassifyMixIsTwentyTenSeven pins the advertised odds against a seeded
// uniform roll: ~20% failures, ~10% hangs, the rest successes.
func TestClassifyMixIsTwentyTenSeven(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	const n = 10000
	var failures, hangs int
	for i := 0; i < n; i++ {
		switch classify(rng.Float64(), 0.20, 0.10) {
		case outcomeFailure:
			failures++
		case outcomeHang:
			hangs++
		}
	}
	if failures < 1800 || failures > 2200 {
		t.Errorf("%d/%d calls failed, want ~2000 (20%%)", failures, n)
	}
	if hangs < 850 || hangs > 1150 {
		t.Errorf("%d/%d calls hung, want ~1000 (10%%)", hangs, n)
	}
}

// TestDuplicateIsRefusedWithTheOriginalReference: the second post of a
// client_ref is a 409 carrying the reference accounting already holds, and
// nothing new is recorded.
func TestDuplicateIsRefusedWithTheOriginalReference(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{Roll: fixed(0.99)}))
	defer srv.Close()

	status, body := postVoucher(t, srv, "dup-1")
	if status != http.StatusOK || body["external_ref"] == "" {
		t.Fatalf("first post: status %d ref %q, want 200 with a reference", status, body["external_ref"])
	}

	status, body2 := postVoucher(t, srv, "dup-1")
	if status != http.StatusConflict {
		t.Fatalf("duplicate post: status %d, want 409", status)
	}
	if body2["external_ref"] != body["external_ref"] {
		t.Errorf("duplicate post returned ref %q, want the original %q", body2["external_ref"], body["external_ref"])
	}

	resp, err := http.Get(srv.URL + "/mock/vouchers")
	if err != nil {
		t.Fatalf("get mock vouchers: %v", err)
	}
	defer resp.Body.Close()
	var all struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		t.Fatalf("decode mock vouchers: %v", err)
	}
	if all.Count != 1 {
		t.Errorf("accounting holds %d vouchers, want 1 (a duplicate must not add one)", all.Count)
	}
}

// TestDuplicateCheckPrecedesTheDice: the second call's roll would inject a
// failure, but the duplicate check answers first — accounting never changes
// its mind about a reference it already issued.
func TestDuplicateCheckPrecedesTheDice(t *testing.T) {
	rolls := []float64{0.99, 0.0}
	i := 0
	srv := httptest.NewServer(NewHandler(Options{Roll: func() float64 {
		v := rolls[i]
		i++
		return v
	}}))
	defer srv.Close()

	if status, _ := postVoucher(t, srv, "dup-2"); status != http.StatusOK {
		t.Fatalf("first post: status %d, want 200", status)
	}
	if status, _ := postVoucher(t, srv, "dup-2"); status != http.StatusConflict {
		t.Errorf("duplicate post: status %d, want 409 even though its roll lands in the failure band", status)
	}
}

// TestFailureBandReturns500: a roll inside the failure band is an injected
// HTTP 500 and records nothing.
func TestFailureBandReturns500(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{Roll: fixed(0.05)}))
	defer srv.Close()

	if status, _ := postVoucher(t, srv, "fail-1"); status != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 for a roll in the failure band", status)
	}
}

// TestHangBandHangsThenSucceeds: a roll inside the hang band delays the
// response (shortened here) and then records the voucher normally.
func TestHangBandHangsThenSucceeds(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{Roll: fixed(0.25), Hang: 40 * time.Millisecond}))
	defer srv.Close()

	start := time.Now()
	status, body := postVoucher(t, srv, "hang-1")
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("response after %s, want >= the configured 40ms hang", elapsed)
	}
	if status != http.StatusOK || body["external_ref"] == "" {
		t.Errorf("status %d ref %q, want 200 with a reference after the hang", status, body["external_ref"])
	}
}

// TestMissingClientRefIsRejected: a body without client_ref is a 400 before
// any dice are rolled.
func TestMissingClientRefIsRejected(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Options{Roll: fixed(0.99)}))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/vouchers", "application/json", strings.NewReader(`{"amount_paise":5}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}
