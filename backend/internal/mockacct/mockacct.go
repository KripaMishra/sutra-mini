// Package mockacct is the stand-in accounting API: a deterministic contract
// wrapped in deliberate unreliability. A share of calls fail with HTTP 500, a
// share hang and then succeed, and a repeated client_ref is refused with 409
// carrying the reference accounting already holds — the behaviour the worker's
// retry, timeout and reconcile paths exist for.
package mockacct

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Options tune the unreliability. Zero values mean the production defaults:
// 20% failures, 10% hangs of 5s.
type Options struct {
	FailureRate float64
	HangRate    float64
	Hang        time.Duration
	// Roll returns [0,1); inject a seeded generator to make the mix testable.
	Roll func() float64
}

type outcome uint8

const (
	outcomeFailure outcome = iota
	outcomeHang
	outcomeSuccess
)

// classify maps one roll onto the outcome it selects.
func classify(roll, failureRate, hangRate float64) outcome {
	switch {
	case roll < failureRate:
		return outcomeFailure
	case roll < failureRate+hangRate:
		return outcomeHang
	default:
		return outcomeSuccess
	}
}

type store struct {
	mu   sync.Mutex
	refs map[string]string // client_ref -> external_ref
	next int
}

// record stores a new voucher, or reports a duplicate with the original ref.
func (s *store) record(clientRef string) (string, bool) {
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

func (s *store) lookup(clientRef string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.refs[clientRef]
	return ref, ok
}

type pushRequest struct {
	ClientRef   string `json:"client_ref"`
	AmountPaise int64  `json:"amount_paise"`
}

// NewHandler returns the accounting API: POST /api/vouchers plus the
// GET /mock/vouchers debugging aid.
func NewHandler(opts Options) http.Handler {
	if opts.FailureRate <= 0 {
		opts.FailureRate = 0.20
	}
	if opts.HangRate <= 0 {
		opts.HangRate = 0.10
	}
	if opts.Hang <= 0 {
		opts.Hang = 5 * time.Second
	}
	if opts.Roll == nil {
		opts.Roll = rand.Float64
	}

	mock := &store{refs: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/vouchers", func(w http.ResponseWriter, r *http.Request) {
		var req pushRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.ClientRef == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client_ref is required"})
			return
		}

		// Known duplicates are answered before the dice are rolled: what
		// accounting already holds must never change.
		if ref, dup := mock.lookup(req.ClientRef); dup {
			log.Printf("POST /api/vouchers %s -> 409 duplicate (%s)", req.ClientRef, ref)
			writeJSON(w, http.StatusConflict, map[string]string{"external_ref": ref})
			return
		}

		switch classify(opts.Roll(), opts.FailureRate, opts.HangRate) {
		case outcomeFailure:
			log.Printf("POST /api/vouchers %s -> 500 (injected failure)", req.ClientRef)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "injected failure"})

		case outcomeHang:
			log.Printf("POST /api/vouchers %s -> hanging %s before succeeding", req.ClientRef, opts.Hang)
			select {
			case <-time.After(opts.Hang):
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

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
