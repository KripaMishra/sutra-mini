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
		amountPaise, priceErr := receiptAmount(ctx, tx, issueID, qty)
		if priceErr != nil {
			return out, priceErr
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO vouchers (type, source_id, amount_paise, client_ref)
			VALUES ('jobwork_receipt', $1, $2, $3)
			RETURNING `+voucherColumns,
			out.Receipt.ID, amountPaise, fmt.Sprintf("receipt-%d", out.Receipt.ID)).
			Scan(voucherScanTargets(&out.Voucher)...); err != nil {
			return out, fmt.Errorf("insert voucher: %w", err)
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

// receiptAmount prices a receipt: qty at the issue's SKU rate, integer paise.
func receiptAmount(ctx context.Context, tx pgx.Tx, issueID int64, qty int) (int64, error) {
	var ratePaise int64
	if err := tx.QueryRow(ctx, `
		SELECT s.rate_paise
		  FROM jobwork_issues i
		  JOIN skus s ON s.id = i.sku_id
		 WHERE i.id = $1`, issueID).Scan(&ratePaise); err != nil {
		return 0, mapReceiptError(err)
	}
	return ratePaise * int64(qty), nil
}
