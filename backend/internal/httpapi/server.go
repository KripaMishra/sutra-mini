// Package httpapi exposes the JSON HTTP surface: receipts, issue progress,
// vouchers and retry.
package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kripa5661/srs/backend/internal/store"
)

type Server struct {
	store *store.Store
}

// New wires routes and middleware around an existing pool.
func New(pool *pgxpool.Pool) http.Handler {
	s := &Server{store: store.New(pool)}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(func(next http.Handler) http.Handler {
		// Echo the request id so a client-side 500 report maps to a log line.
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Request-Id", middleware.GetReqID(req.Context()))
			next.ServeHTTP(w, req)
		})
	})
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", s.handleHealthz)
	r.Post("/jobwork/receipts", s.handleCreateReceipt)
	r.Get("/issues/{id}", s.handleGetIssue)
	r.Get("/vouchers", s.handleListVouchers)
	r.Post("/vouchers/{id}/retry", s.handleRetryVoucher)

	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func logInternal(op string, err error) {
	log.Printf("error: %s: %v", op, err)
}
