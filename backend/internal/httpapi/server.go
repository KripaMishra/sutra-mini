// Package httpapi exposes the JSON HTTP surface: receipts, issue progress,
// vouchers, retry, and the LLM-backed /ask endpoint.
package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KripaMishra/sutra-mini/backend/internal/store"
)

type Server struct {
	store *store.Store
	ask   asker
}

// Option configures optional server dependencies.
type Option func(*Server)

// WithAsk attaches the /ask service. Without it the route still exists and
// answers 503, so a half-configured deployment fails loudly instead of 404ing.
func WithAsk(svc asker) Option {
	return func(s *Server) { s.ask = svc }
}

// New wires routes and middleware around an existing pool.
func New(pool *pgxpool.Pool, opts ...Option) http.Handler {
	s := &Server{store: store.New(pool)}
	for _, opt := range opts {
		opt(s)
	}

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

	// The database endpoints answer in milliseconds; 30s is already generous.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

		r.Get("/healthz", s.handleHealthz)
		r.Post("/jobwork/receipts", s.handleCreateReceipt)
		r.Get("/issues/{id}", s.handleGetIssue)
		r.Get("/vouchers", s.handleListVouchers)
		r.Post("/vouchers/{id}/retry", s.handleRetryVoucher)
	})

	// /ask runs a queue wait plus several model turns, so it gets its own,
	// longer budget; the server's WriteTimeout is set above this one.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(askTimeout))
		r.Post("/ask", s.handleAsk)
	})

	return r
}
