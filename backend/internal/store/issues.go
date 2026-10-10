package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IssueSummary is an issue's progress: what went out, what came back, what is
// still outstanding.
type IssueSummary struct {
	ID          int64     `json:"id"`
	PartyID     int64     `json:"party_id"`
	SKUID       int64     `json:"sku_id"`
	Issued      int       `json:"issued"`
	Received    int64     `json:"received"`
	Outstanding int64     `json:"outstanding"`
	IssuedOn    time.Time `json:"issued_on"`
	PartyName   string    `json:"party_name"`
	SKUCode     string    `json:"sku_code"`
	SKUName     string    `json:"sku_name"`
}

func (s *Store) GetIssue(ctx context.Context, id int64) (IssueSummary, error) {
	var out IssueSummary
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, i.party_id, i.sku_id, i.qty, i.issued_on,
		       p.name, s.code, s.name,
		       COALESCE((SELECT sum(r.qty) FROM jobwork_receipts r WHERE r.issue_id = i.id), 0)
		  FROM jobwork_issues i
		  JOIN parties p ON p.id = i.party_id
		  JOIN skus    s ON s.id = i.sku_id
		 WHERE i.id = $1`, id).
		Scan(&out.ID, &out.PartyID, &out.SKUID, &out.Issued, &out.IssuedOn,
			&out.PartyName, &out.SKUCode, &out.SKUName, &out.Received)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrIssueNotFound
	}
	if err != nil {
		return out, err
	}
	out.Outstanding = int64(out.Issued) - out.Received
	return out, nil
}
