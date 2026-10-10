// The accounting client's classification is pure HTTP behaviour, so these
// tests use an in-process stub and no database.
package accounting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPushOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantRef string
		wantDup bool
		wantErr bool
	}{
		{"ok 200", http.StatusOK, `{"external_ref":"M-1"}`, "M-1", false, false},
		{"created 201", http.StatusCreated, `{"external_ref":"M-2"}`, "M-2", false, false},
		{"duplicate 409", http.StatusConflict, `{"external_ref":"M-3"}`, "M-3", true, false},
		{"2xx without ref", http.StatusOK, `{}`, "", false, true},
		{"unreadable body", http.StatusOK, `not json`, "", false, true},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`, "", false, true},
		{"conflict without ref", http.StatusConflict, `{}`, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer stub.Close()

			client := NewClient(stub.URL, 2*time.Second)
			res, err := client.Push(context.Background(), "receipt-1", 1234)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if res.ExternalRef != tc.wantRef || res.Duplicate != tc.wantDup {
				t.Errorf("result = %+v, want ref %q duplicate %v", res, tc.wantRef, tc.wantDup)
			}
		})
	}
}

func TestPushTimesOut(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"external_ref":"late"}`))
	}))
	defer stub.Close()

	client := NewClient(stub.URL, 20*time.Millisecond)
	if _, err := client.Push(context.Background(), "receipt-1", 1234); err == nil {
		t.Fatal("expected a timeout error, got success")
	}
}
