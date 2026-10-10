// Command mockaccounting is the deliberately unreliable accounting API:
// 20% of calls return HTTP 500, 10% hang for five seconds and then succeed,
// and a repeated client_ref returns 409 with the reference it already holds.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"
)

type mockStore struct {
	mu   sync.Mutex
	refs map[string]string // client_ref -> external_ref
	next int
}

// record stores a new voucher, or reports a duplicate with the original ref.
func (s *mockStore) record(clientRef string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref, ok := s.refs[clientRef]; ok {
		return ref, true
	}
	s.next++
	ref := fmt.Sprintf("MOCK-%06d", s.next)
	s.refs[clientRef] = ref
	return ref, false
}

func (s *mockStore) lookup(clientRef string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.refs[clientRef]
	return ref, ok
}

type pushRequest struct {
	ClientRef   string `json:"client_ref"`
	AmountPaise int64  `json:"amount_paise"`
}

func main() {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	mock := &mockStore{refs: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vouchers", func(w http.ResponseWriter, r *http.Request) {
		var req pushRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.ClientRef == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_ref is required"})
			return
		}

		// Known duplicate: hand back the original reference so the caller can
		// reconcile instead of double-posting.
		if ref, dup := mock.lookup(req.ClientRef); dup {
			log.Printf("POST /api/vouchers %s -> 409 duplicate (%s)", req.ClientRef, ref)
			writeJSON(w, http.StatusConflict, map[string]string{"external_ref": ref})
			return
		}

		switch roll := rand.Float64(); {
		case roll < 0.20:
			log.Printf("POST /api/vouchers %s -> 500 (injected failure)", req.ClientRef)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "injected failure"})

		case roll < 0.30:
			log.Printf("POST /api/vouchers %s -> hanging 5s before succeeding", req.ClientRef)
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
				log.Printf("POST /api/vouchers %s -> caller went away during the hang", req.ClientRef)
				return
			}
			ref, dup := mock.record(req.ClientRef)
			if dup {
				writeJSON(w, http.StatusConflict, map[string]string{"external_ref": ref})
				return
			}
			log.Printf("POST /api/vouchers %s -> 200 %s (after hang)", req.ClientRef, ref)
			writeJSON(w, http.StatusOK, map[string]string{"external_ref": ref})

		default:
			ref, dup := mock.record(req.ClientRef)
			if dup {
				writeJSON(w, http.StatusConflict, map[string]string{"external_ref": ref})
				return
			}
			log.Printf("POST /api/vouchers %s -> 200 %s", req.ClientRef, ref)
			writeJSON(w, http.StatusOK, map[string]string{"external_ref": ref})
		}
	})

	// Debugging aid: everything accounting has accepted so far.
	mux.HandleFunc("GET /mock/vouchers", func(w http.ResponseWriter, r *http.Request) {
		mock.mu.Lock()
		defer mock.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"count": len(mock.refs), "vouchers": mock.refs})
	})

	log.Printf("mock accounting listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
