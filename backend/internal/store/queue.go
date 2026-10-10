package store

import (
	"context"
	"time"
)

// ClaimedVoucher is a voucher leased to this worker for one push attempt.
type ClaimedVoucher struct {
	ID          int64
	ClientRef   string
	AmountPaise int64
	Attempts    int // attempts already recorded before this claim
	LeaseToken  string
}

// ClaimVouchers leases up to limit claimable vouchers of the given type (””
// claims any type). Claimable means: still pending, due (next_attempt_at in
// the past) and not leased by another worker. FOR UPDATE SKIP LOCKED makes
// concurrent claimers — the same worker loop or a second process — never
// receive the same row, and a lease that expires (worker crashed mid-push)
// simply becomes claimable again.
func (s *Store) ClaimVouchers(ctx context.Context, voucherType string, limit, leaseSeconds int) ([]ClaimedVoucher, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE vouchers v
		   SET lease_until = now() + make_interval(secs => $2),
		       lease_token = gen_random_uuid()::text
		  FROM (
		        SELECT id
		          FROM vouchers
		         WHERE status = 'pending'
		           AND next_attempt_at <= now()
		           AND (lease_until IS NULL OR lease_until < now())
		           AND ($3 = '' OR type = $3)
		         ORDER BY next_attempt_at, id
		         LIMIT $1
		           FOR UPDATE SKIP LOCKED
		       ) AS claimable
		 WHERE v.id = claimable.id
		RETURNING v.id, v.client_ref, v.amount_paise, v.attempts, v.lease_token`,
		limit, leaseSeconds, voucherType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClaimedVoucher
	for rows.Next() {
		var v ClaimedVoucher
		if err := rows.Scan(&v.ID, &v.ClientRef, &v.AmountPaise, &v.Attempts, &v.LeaseToken); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// MarkPushed finalizes a voucher after accounting accepted it (or told us it
// already has it). The lease token guard means a worker that lost its lease
// cannot clobber a voucher another worker has since re-claimed.
func (s *Store) MarkPushed(ctx context.Context, id int64, leaseToken, externalRef string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE vouchers
		   SET status = 'pushed',
		       external_ref = $3,
		       attempts = attempts + 1,
		       last_error = '',
		       lease_until = NULL,
		       lease_token = NULL
		 WHERE id = $1 AND lease_token = $2`, id, leaseToken, externalRef)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkRetry records a failed attempt and schedules the next one after backoff.
func (s *Store) MarkRetry(ctx context.Context, id int64, leaseToken string, attempts int, backoff time.Duration, lastErr string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE vouchers
		   SET status = 'pending',
		       attempts = $3,
		       next_attempt_at = now() + make_interval(secs => $4),
		       last_error = $5,
		       lease_until = NULL,
		       lease_token = NULL
		 WHERE id = $1 AND lease_token = $2`,
		id, leaseToken, attempts, int(backoff/time.Second), lastErr)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkFailed parks a voucher that exhausted its attempts.
func (s *Store) MarkFailed(ctx context.Context, id int64, leaseToken string, attempts int, lastErr string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE vouchers
		   SET status = 'failed',
		       attempts = $3,
		       next_attempt_at = now(),
		       last_error = $4,
		       lease_until = NULL,
		       lease_token = NULL
		 WHERE id = $1 AND lease_token = $2`, id, leaseToken, attempts, lastErr)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
