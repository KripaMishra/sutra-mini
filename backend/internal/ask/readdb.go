package ask

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// readDB is the GORM handle behind the read tools. Every query runs inside a
// READ ONLY transaction with a hard statement timeout, so even a bug in one of
// the finders below cannot write to the database: Postgres rejects the
// statement instead. There is deliberately no write path in this package.
type readDB struct {
	db *gorm.DB
}

const (
	statementTimeout = 5 * time.Second
	findLimit        = 20
	stockLimit       = 50
)

func openReadDB(databaseURL string) (*readDB, error) {
	gdb, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		// The tools are not an admin surface: keep GORM quiet unless it has an
		// actual error to report, and never log bound values.
		Logger: logger.Default.LogMode(logger.Error),
	})
	if err != nil {
		return nil, fmt.Errorf("open gorm: %w", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("gorm sql.DB: %w", err)
	}
	// The API's own pool is capped at 8; keep the read-only side small so a
	// burst of questions cannot starve the endpoints that write.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	return &readDB{db: gdb}, nil
}

func (r *readDB) Close() error {
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// readOnly runs fn inside a READ ONLY transaction. Postgres itself refuses any
// write the callback attempts; the timeout bounds a query that goes rogue.
func (r *readDB) readOnly(ctx context.Context, fn func(tx *gorm.DB) error) error {
	tx := r.db.WithContext(ctx).Begin(&sql.TxOptions{ReadOnly: true})
	if tx.Error != nil {
		return fmt.Errorf("begin read-only transaction: %w", tx.Error)
	}
	defer func() { _ = tx.Rollback().Error }()

	// SET LOCAL cannot take a bound parameter; the value is a constant here,
	// never anything the model wrote.
	if err := tx.Exec(fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", statementTimeout.Milliseconds())).Error; err != nil {
		return fmt.Errorf("set statement timeout: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit().Error
}

// Sku maps the skus table.
type Sku struct {
	ID        int64  `gorm:"column:id" json:"id"`
	Code      string `gorm:"column:code" json:"code"`
	Name      string `gorm:"column:name" json:"name"`
	RatePaise int64  `gorm:"column:rate_paise" json:"rate_paise"`
}

func (Sku) TableName() string { return "skus" }

// JobWorker maps the parties table — "job-worker" in the brief's language.
type JobWorker struct {
	ID    int64  `gorm:"column:id" json:"id"`
	Name  string `gorm:"column:name" json:"name"`
	Phone string `gorm:"column:phone" json:"phone"`
}

func (JobWorker) TableName() string { return "parties" }

// JobworkReceipt maps the append-only receipts table; it appears in the
// aggregate as a per-issue sum, never joined directly (that would multiply
// issued quantities for issues with several partial receipts).
type JobworkReceipt struct {
	ID      int64 `gorm:"column:id"`
	IssueID int64 `gorm:"column:issue_id"`
	Qty     int   `gorm:"column:qty"`
}

func (JobworkReceipt) TableName() string { return "jobwork_receipts" }

// The scan targets below carry explicit column tags: GORM's naming strategy
// would otherwise have to guess at things like SKUCode -> sku_code.

type skuMatch struct {
	ID        int64  `gorm:"column:id" json:"id"`
	Code      string `gorm:"column:code" json:"code"`
	Name      string `gorm:"column:name" json:"name"`
	RatePaise int64  `gorm:"column:rate_paise" json:"rate_paise"`
}

type skuMatches struct {
	Query   string     `json:"query"`
	Count   int        `json:"count"`
	Matches []skuMatch `json:"matches"`
	Note    string     `json:"note,omitempty"`
}

type workerMatches struct {
	Query   string        `json:"query"`
	Count   int           `json:"count"`
	Matches []workerMatch `json:"matches"`
	Note    string        `json:"note,omitempty"`
}

type workerMatch struct {
	ID    int64  `gorm:"column:id" json:"id"`
	Name  string `gorm:"column:name" json:"name"`
	Phone string `gorm:"column:phone" json:"phone"`
}

type stockRow struct {
	JobWorker string `gorm:"column:job_worker" json:"job_worker"`
	SKUCode   string `gorm:"column:sku_code" json:"sku_code"`
	SKUName   string `gorm:"column:sku_name" json:"sku_name"`
	Issued    int64  `gorm:"column:issued" json:"issued"`
	Received  int64  `gorm:"column:received" json:"received"`
	StillWith int64  `gorm:"column:still_with" json:"still_with"`
}

type stockResult struct {
	SKUQuery       string     `json:"sku_query"`
	WorkerQuery    string     `json:"worker_query"`
	Count          int        `json:"count"`
	TotalStillWith int64      `json:"total_still_with"`
	Rows           []stockRow `json:"rows"`
	Note           string     `json:"note,omitempty"`
}

// like builds a case-insensitive contains pattern. The wildcards a user could
// type are escaped so "50%" searches for a literal 50% instead of matching
// everything.
func like(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(s))
	return "%" + s + "%"
}

// findSkus returns every SKU whose code or name contains the query. All
// matches come back — a partial name must stay visible to the model so it can
// ask a better question instead of silently getting one arbitrary row.
func (r *readDB) findSkus(ctx context.Context, query string) (skuMatches, error) {
	out := skuMatches{Query: query, Matches: []skuMatch{}}
	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		pattern := like(query)
		return tx.Model(&Sku{}).
			Where("code ILIKE ? OR name ILIKE ?", pattern, pattern).
			Order("code").
			Limit(findLimit).
			Scan(&out.Matches).Error
	})
	if err != nil {
		return out, err
	}
	out.Count = len(out.Matches)
	out.Note = matchNote(out.Count, "SKU", "use a shorter fragment of the code or name")
	return out, nil
}

// findJobWorkers returns every party whose name or phone contains the query.
// parties.name has no unique constraint, so several rows are a normal answer,
// not an error.
func (r *readDB) findJobWorkers(ctx context.Context, query string) (workerMatches, error) {
	out := workerMatches{Query: query, Matches: []workerMatch{}}
	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		pattern := like(query)
		return tx.Model(&JobWorker{}).
			Where("name ILIKE ? OR phone ILIKE ?", pattern, pattern).
			Order("name").
			Limit(findLimit).
			Scan(&out.Matches).Error
	})
	if err != nil {
		return out, err
	}
	out.Count = len(out.Matches)
	out.Note = matchNote(out.Count, "job-worker", "use a shorter fragment of the name")
	return out, nil
}

// stock answers the brief's question. With no SKU query it rolls up per SKU
// (everything a worker holds); with no worker query it rolls up across every
// worker holding that SKU; with both it is exactly the headline question.
//
// still_with is the repo's own semantics: sum(issues.qty) − sum(receipts.qty)
// per (job-worker, SKU). The receipts side is summed per issue first so an
// issue with three partial receipts still counts its issued quantity once.
func (r *readDB) stock(ctx context.Context, skuQuery, workerQuery string) (stockResult, error) {
	out := stockResult{SKUQuery: skuQuery, WorkerQuery: workerQuery, Rows: []stockRow{}}
	if strings.TrimSpace(skuQuery) == "" && strings.TrimSpace(workerQuery) == "" {
		return out, errors.New("at least one of sku and job_worker must be given")
	}

	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		receivedPerIssue := tx.Model(&JobworkReceipt{}).
			Select("issue_id, SUM(qty) AS received").
			Group("issue_id")

		q := tx.Table("jobwork_issues AS i").
			Select(`p.name AS job_worker, s.code AS sku_code, s.name AS sku_name,
				CAST(SUM(i.qty) AS bigint) AS issued,
				CAST(COALESCE(SUM(rec.received), 0) AS bigint) AS received`).
			Joins("JOIN parties AS p ON p.id = i.party_id").
			Joins("JOIN skus AS s ON s.id = i.sku_id").
			Joins("LEFT JOIN (?) AS rec ON rec.issue_id = i.id", receivedPerIssue).
			Group("p.name, s.code, s.name").
			Order("p.name, s.code").
			Limit(stockLimit)

		if skuQuery != "" {
			pattern := like(skuQuery)
			q = q.Where("s.code ILIKE ? OR s.name ILIKE ?", pattern, pattern)
		}
		if workerQuery != "" {
			pattern := like(workerQuery)
			q = q.Where("p.name ILIKE ? OR p.phone ILIKE ?", pattern, pattern)
		}
		return q.Scan(&out.Rows).Error
	})
	if err != nil {
		return out, err
	}

	for i := range out.Rows {
		out.Rows[i].StillWith = out.Rows[i].Issued - out.Rows[i].Received
		out.TotalStillWith += out.Rows[i].StillWith
	}
	out.Count = len(out.Rows)
	switch out.Count {
	case 0:
		out.Note = "no rows matched; check the code or name with find_skus / find_job_workers"
	case 1:
		out.Note = ""
	default:
		out.Note = "more than one (job-worker, SKU) pair matched; the answer is per row and the total is the sum"
	}
	return out, nil
}

func matchNote(count int, kind, hint string) string {
	switch count {
	case 0:
		return "no " + kind + " matched; " + hint
	case 1:
		return ""
	default:
		return "several " + kind + "s matched; pick the right one (or report them all) rather than assuming"
	}
}
