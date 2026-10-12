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

	// GORM opens its pool lazily, so without this a bad DSN would only show up
	// on the first question. Fail where it is visible instead.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping ask database: %w", err)
	}
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
//
// Each one also carries a window-count column (matched_total / matched_pairs)
// so a capped page can say how much it is not showing instead of passing a
// short list off as complete. Those columns never reach the model (json:"-").

type skuMatch struct {
	ID        int64  `gorm:"column:id" json:"id"`
	Code      string `gorm:"column:code" json:"code"`
	Name      string `gorm:"column:name" json:"name"`
	RatePaise int64  `gorm:"column:rate_paise" json:"rate_paise"`

	MatchedTotal int64 `gorm:"column:matched_total" json:"-"`
}

type skuMatches struct {
	Query     string     `json:"query"`
	Count     int        `json:"count"`
	Total     int        `json:"total_matched"`
	Truncated bool       `json:"truncated,omitempty"`
	Matches   []skuMatch `json:"matches"`
	Note      string     `json:"note,omitempty"`
}

// workerMatch deliberately omits parties.phone: a question may mention a phone
// number as a search term, but contact details are never echoed back into a
// response the caller — or the log — can read.
type workerMatch struct {
	ID   int64  `gorm:"column:id" json:"id"`
	Name string `gorm:"column:name" json:"name"`

	MatchedTotal int64 `gorm:"column:matched_total" json:"-"`
}

type workerMatches struct {
	Query     string        `json:"query"`
	Count     int           `json:"count"`
	Total     int           `json:"total_matched"`
	Truncated bool          `json:"truncated,omitempty"`
	Matches   []workerMatch `json:"matches"`
	Note      string        `json:"note,omitempty"`
}

type stockRow struct {
	JobWorkerID int64  `gorm:"column:job_worker_id" json:"job_worker_id"`
	JobWorker   string `gorm:"column:job_worker" json:"job_worker"`
	SKUID       int64  `gorm:"column:sku_id" json:"sku_id"`
	SKUCode     string `gorm:"column:sku_code" json:"sku_code"`
	SKUName     string `gorm:"column:sku_name" json:"sku_name"`
	Issued      int64  `gorm:"column:issued" json:"issued"`
	Received    int64  `gorm:"column:received" json:"received"`
	StillWith   int64  `gorm:"column:still_with" json:"still_with"`

	MatchedPairs int64 `gorm:"column:matched_pairs" json:"-"`
	MatchedSum   int64 `gorm:"column:matched_sum" json:"-"`
}

type stockResult struct {
	SKUQuery    string     `json:"sku_query"`
	WorkerQuery string     `json:"worker_query"`
	Count       int        `json:"count"`
	Pairs       int        `json:"matched_pairs"`
	Truncated   bool       `json:"truncated,omitempty"`
	Rows        []stockRow `json:"rows"`
	// TotalStillWith covers every matched pair, not just the rows returned.
	TotalStillWith int64  `json:"total_still_with"`
	Note           string `json:"note,omitempty"`
}

// like builds a case-insensitive contains pattern. The wildcards a user could
// type are escaped so "50%" searches for a literal 50% instead of matching
// everything.
func like(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(s))
	return "%" + s + "%"
}

// findSkus returns the SKUs whose code or name contains the query, up to
// findLimit of them. A partial name stays visible to the model so it can ask a
// better question, and total_matched says when the list is capped.
func (r *readDB) findSkus(ctx context.Context, query string) (skuMatches, error) {
	out := skuMatches{Query: query, Matches: []skuMatch{}}
	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		pattern := like(query)
		return tx.Model(&Sku{}).
			Select("id, code, name, rate_paise, CAST(COUNT(*) OVER () AS bigint) AS matched_total").
			Where("code ILIKE ? OR name ILIKE ?", pattern, pattern).
			Order("code").
			Limit(findLimit).
			Scan(&out.Matches).Error
	})
	if err != nil {
		return out, err
	}
	out.Count = len(out.Matches)
	if out.Count > 0 {
		out.Total = int(out.Matches[0].MatchedTotal)
	}
	out.Truncated = out.Total > out.Count
	out.Note = matchNote(out.Count, out.Total, "SKU", "use a shorter fragment of the code or name")
	return out, nil
}

// findJobWorkers returns the parties whose name or phone contains the query.
// parties.name has no unique constraint, so several rows are a normal answer,
// not an error — and each row carries its id so the model can tell two
// same-named workers apart.
func (r *readDB) findJobWorkers(ctx context.Context, query string) (workerMatches, error) {
	out := workerMatches{Query: query, Matches: []workerMatch{}}
	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		pattern := like(query)
		return tx.Model(&JobWorker{}).
			Select("id, name, CAST(COUNT(*) OVER () AS bigint) AS matched_total").
			Where("name ILIKE ? OR phone ILIKE ?", pattern, pattern).
			Order("name, id").
			Limit(findLimit).
			Scan(&out.Matches).Error
	})
	if err != nil {
		return out, err
	}
	out.Count = len(out.Matches)
	if out.Count > 0 {
		out.Total = int(out.Matches[0].MatchedTotal)
	}
	out.Truncated = out.Total > out.Count
	out.Note = matchNote(out.Count, out.Total, "job-worker", "use a shorter fragment of the name")
	return out, nil
}

// stock answers the brief's question. With no SKU query it rolls up per SKU
// (everything a worker holds); with no worker query it rolls up across every
// worker holding that SKU; with both it is exactly the headline question.
//
// still_with is the repo's own semantics: sum(issues.qty) − sum(receipts.qty)
// per (job-worker, SKU). The receipts side is summed per issue first so an
// issue with three partial receipts still counts its issued quantity once.
// Rows are grouped by party and SKU *id*, because parties.name is not unique:
// two workers who share a name must never be merged into one number.
func (r *readDB) stock(ctx context.Context, skuQuery, workerQuery string) (stockResult, error) {
	skuQuery = strings.TrimSpace(skuQuery)
	workerQuery = strings.TrimSpace(workerQuery)
	out := stockResult{SKUQuery: skuQuery, WorkerQuery: workerQuery, Rows: []stockRow{}}
	if skuQuery == "" && workerQuery == "" {
		return out, errors.New("at least one of sku and job_worker must be given")
	}

	err := r.readOnly(ctx, func(tx *gorm.DB) error {
		receivedPerIssue := tx.Model(&JobworkReceipt{}).
			Select("issue_id, SUM(qty) AS received").
			Group("issue_id")

		// The two window columns describe the whole result set, before LIMIT:
		// how many pairs matched, and the sum of still_with across all of them.
		q := tx.Table("jobwork_issues AS i").
			Select(`p.id AS job_worker_id, p.name AS job_worker,
				s.id AS sku_id, s.code AS sku_code, s.name AS sku_name,
				CAST(SUM(i.qty) AS bigint) AS issued,
				CAST(COALESCE(SUM(rec.received), 0) AS bigint) AS received,
				CAST(COUNT(*) OVER () AS bigint) AS matched_pairs,
				CAST(SUM(SUM(i.qty) - COALESCE(SUM(rec.received), 0)) OVER () AS bigint) AS matched_sum`).
			Joins("JOIN parties AS p ON p.id = i.party_id").
			Joins("JOIN skus AS s ON s.id = i.sku_id").
			Joins("LEFT JOIN (?) AS rec ON rec.issue_id = i.id", receivedPerIssue).
			Group("p.id, p.name, s.id, s.code, s.name").
			Order("p.name, p.id, s.code").
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
	}
	out.Count = len(out.Rows)
	if out.Count > 0 {
		out.Pairs = int(out.Rows[0].MatchedPairs)
		out.TotalStillWith = out.Rows[0].MatchedSum
	}
	out.Truncated = out.Pairs > out.Count
	out.Note = stockNote(out)
	return out, nil
}

func stockNote(res stockResult) string {
	switch {
	case res.Count == 0:
		return "no rows matched; check the code or name with find_skus / find_job_workers"
	case res.Truncated:
		return fmt.Sprintf("showing %d of %d matching (job-worker, SKU) pairs; total_still_with covers all %d, but the rows list is incomplete",
			res.Count, res.Pairs, res.Pairs)
	}

	// Different workers can share a name; say so rather than letting a reader
	// assume these rows are one worker's.
	ids := make(map[int64]bool, res.Count)
	names := make(map[string]bool, res.Count)
	for _, row := range res.Rows {
		ids[row.JobWorkerID] = true
		names[row.JobWorker] = true
	}
	if len(ids) > len(names) {
		return "these rows belong to different job-workers that share a name; compare them by job_worker_id"
	}
	if res.Count > 1 {
		return "more than one (job-worker, SKU) pair matched; the answer is per row and total_still_with is the sum"
	}
	return ""
}

func matchNote(count, total int, kind, hint string) string {
	switch {
	case count == 0:
		return "no " + kind + " matched; " + hint
	case total > count:
		return fmt.Sprintf("matched %d in total, showing the first %d; narrow the search rather than treating this as the full list", total, count)
	case count > 1:
		return "several " + kind + "s matched; pick the right one (or report them all) rather than assuming"
	}
	return ""
}
