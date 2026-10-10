// Package store holds the SQL layer. Every invariant that must survive
// concurrency lives in Postgres (constraints and triggers); this package only
// surfaces those outcomes as typed errors.
package store

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrIssueNotFound   = errors.New("issue not found")
	ErrOvershoot       = errors.New("receipt would exceed the issued quantity")
	ErrKeyReuse        = errors.New("idempotency key was already used with a different request")
	ErrVoucherNotFound = errors.New("voucher not found")
	ErrNotRetryable    = errors.New("only failed vouchers can be retried")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool for health checks and tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

type Receipt struct {
	ID             int64     `json:"id"`
	IssueID        int64     `json:"issue_id"`
	Qty            int       `json:"qty"`
	ReceivedOn     time.Time `json:"received_on"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

type Voucher struct {
	ID            int64     `json:"id"`
	Type          string    `json:"type"`
	SourceID      int64     `json:"source_id"`
	AmountPaise   int64     `json:"amount_paise"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error"`
	ClientRef     string    `json:"client_ref"`
	ExternalRef   string    `json:"external_ref"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const voucherColumns = `id, type, source_id, amount_paise, status, attempts,
	last_error, client_ref, external_ref, next_attempt_at, created_at, updated_at`

func voucherScanTargets(v *Voucher) []any {
	return []any{
		&v.ID, &v.Type, &v.SourceID, &v.AmountPaise, &v.Status, &v.Attempts,
		&v.LastError, &v.ClientRef, &v.ExternalRef, &v.NextAttemptAt,
		&v.CreatedAt, &v.UpdatedAt,
	}
}

// mapReceiptError translates the guard trigger's signals (and the issue
// foreign key) into sentinel errors the HTTP layer can map onto status codes.
func mapReceiptError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrIssueNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "P0001" && pgErr.Detail == "overshoot":
			return ErrOvershoot
		case pgErr.Code == "P0001" && pgErr.Detail == "key_reuse_mismatch":
			return ErrKeyReuse
		case pgErr.Code == "P0001" && pgErr.Detail == "issue_not_found":
			return ErrIssueNotFound
		case pgErr.Code == "23503": // foreign key violation (issue_id)
			return ErrIssueNotFound
		}
	}
	return err
}
