// Package httpapi exposes the JSON HTTP surface: receipts, issue progress,
// vouchers and retry.
package httpapi

import (
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
	// ClientIPFromRemoteAddr is the non-spoofable replacement for the
	// deprecated middleware.RealIP (GHSA-3fxj-6jh8-hvhx and friends): it
	// records the socket peer address and never trusts X-Forwarded-For /
	// X-Real-IP / True-Client-IP. This service runs without a trusted proxy;
	// if one is ever put in front, swap in middleware.ClientIPFromXFF with
	// the proxy's ranges.
	r.Use(middleware.ClientIPFromRemoteAddr)
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
