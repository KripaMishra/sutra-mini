package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ListVouchers returns vouchers newest-first, optionally filtered by status
// ("pending", "pushed", "failed"; empty means all).
func (s *Store) ListVouchers(ctx context.Context, status string) ([]Voucher, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+voucherColumns+`
		  FROM vouchers
		 WHERE ($1 = '' OR status = $1)
		 ORDER BY id DESC`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Voucher
	for rows.Next() {
		var v Voucher
		if err := rows.Scan(voucherScanTargets(&v)...); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RetryVoucher requeues a failed voucher: pending again with a fresh attempt
// budget, so the next worker pass picks it up. The previous failure reason
// stays visible on the page until the next attempt overwrites it.
func (s *Store) RetryVoucher(ctx context.Context, id int64) (Voucher, error) {
	var v Voucher
	err := s.pool.QueryRow(ctx, `
		UPDATE vouchers
		   SET status = 'pending',
		       attempts = 0,
		       next_attempt_at = now(),
		       lease_until = NULL,
		       lease_token = NULL
		 WHERE id = $1 AND status = 'failed'
		 RETURNING `+voucherColumns, id).
		Scan(voucherScanTargets(&v)...)

	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		switch err := s.pool.QueryRow(ctx, `SELECT status FROM vouchers WHERE id = $1`, id).Scan(&status); {
		case errors.Is(err, pgx.ErrNoRows):
			return v, ErrVoucherNotFound
		case err != nil:
			return v, err
		default:
			return v, ErrNotRetryable
		}
	}
	if err != nil {
		return v, err
	}
	return v, nil
}
