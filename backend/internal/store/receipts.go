package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReceiptResult is the response of a receipt write: the stored receipt, its
// voucher, and whether the request was a replay of an existing key.
type ReceiptResult struct {
	Receipt  Receipt `json:"receipt"`
	Voucher  Voucher `json:"voucher"`
	Replayed bool    `json:"replayed"`
}

// CreateOrReplayReceipt writes the receipt and its voucher in one
// transaction. Replaying an existing Idempotency-Key returns the stored pair
// and creates nothing; the unique index, ON CONFLICT DO NOTHING and the guard
// trigger together make that safe even when duplicates arrive concurrently.
func (s *Store) CreateOrReplayReceipt(ctx context.Context, issueID int64, qty int, receivedOn time.Time, idempotencyKey string) (ReceiptResult, error) {
	var out ReceiptResult

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO jobwork_receipts (issue_id, qty, received_on, idempotency_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, issue_id, qty, received_on, idempotency_key, created_at`,
		issueID, qty, receivedOn, idempotencyKey).
		Scan(&out.Receipt.ID, &out.Receipt.IssueID, &out.Receipt.Qty,
			&out.Receipt.ReceivedOn, &out.Receipt.IdempotencyKey, &out.Receipt.CreatedAt)

	switch {
	case err == nil:
		// Price and write the voucher in one statement: amount = qty × the
		// issue's SKU rate (integer paise), keeping the locked window short.
		err := tx.QueryRow(ctx, `
			INSERT INTO vouchers (type, source_id, amount_paise, client_ref)
			SELECT 'jobwork_receipt', $1, s.rate_paise * $2, $3
			  FROM jobwork_issues i
			  JOIN skus s ON s.id = i.sku_id
			 WHERE i.id = $4
			RETURNING `+voucherColumns,
			out.Receipt.ID, qty, fmt.Sprintf("receipt-%d", out.Receipt.ID), issueID).
			Scan(voucherScanTargets(&out.Voucher)...)
		if err != nil {
			return out, mapReceiptError(err)
		}

	case errors.Is(err, pgx.ErrNoRows):
		// The key already exists (ON CONFLICT DO NOTHING returned nothing):
		// replay the stored receipt and its voucher, creating nothing.
		out.Replayed = true
		if err := tx.QueryRow(ctx, `
			SELECT id, issue_id, qty, received_on, idempotency_key, created_at
			  FROM jobwork_receipts
			 WHERE idempotency_key = $1`, idempotencyKey).
			Scan(&out.Receipt.ID, &out.Receipt.IssueID, &out.Receipt.Qty,
				&out.Receipt.ReceivedOn, &out.Receipt.IdempotencyKey, &out.Receipt.CreatedAt); err != nil {
			return out, fmt.Errorf("load replayed receipt: %w", err)
		}
		// The trigger catches same-body replays, but a concurrent reuse of the
		// key with a different body can slip past its snapshot; verify here so
		// the caller always gets 409 instead of someone else's receipt.
		if out.Receipt.IssueID != issueID || out.Receipt.Qty != qty || !out.Receipt.ReceivedOn.Equal(receivedOn) {
			return out, ErrKeyReuse
		}
		if err := tx.QueryRow(ctx, `
			SELECT `+voucherColumns+`
			  FROM vouchers
			 WHERE type = 'jobwork_receipt' AND source_id = $1`, out.Receipt.ID).
			Scan(voucherScanTargets(&out.Voucher)...); err != nil {
			return out, fmt.Errorf("load replayed voucher: %w", err)
		}

	default:
		return out, mapReceiptError(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}
