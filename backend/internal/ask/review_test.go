// The tests added by the review pass: they pin the behaviours the review found
// broken or unproven — same-named workers, silent truncation, shutdown — plus
// the payload formatter.
package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"

	"github.com/KripaMishra/sutra-mini/backend/internal/config"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil || cfg.DatabaseURL == "" {
		t.Fatalf("DATABASE_URL is required for this test: %v", err)
	}
	return cfg.DatabaseURL
}

func insertID(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := testPool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return id
}

// A name is not an identity: two workers can share one, and the headline answer
// must stay per-worker instead of silently summing them.
func TestStockKeepsSameNamedWorkersApart(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	name := fmt.Sprintf("Same Name Worker %d", suffix)
	code := fmt.Sprintf("SAME-%d", suffix)

	skuID := insertID(t, `INSERT INTO skus (code, name, image_url, rate_paise) VALUES ($1, 'Shared Name SKU', '', 1000) RETURNING id`, code)
	firstID := insertID(t, `INSERT INTO parties (name, phone) VALUES ($1, '') RETURNING id`, name)
	secondID := insertID(t, `INSERT INTO parties (name, phone) VALUES ($1, '') RETURNING id`, name)
	firstIssue := insertID(t, `INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on) VALUES ($1, $2, 10, current_date) RETURNING id`, firstID, skuID)
	secondIssue := insertID(t, `INSERT INTO jobwork_issues (party_id, sku_id, qty, issued_on) VALUES ($1, $2, 7, current_date) RETURNING id`, secondID, skuID)
	insertID(t, `INSERT INTO jobwork_receipts (issue_id, qty, received_on, idempotency_key) VALUES ($1, 4, current_date, $2) RETURNING id`, firstIssue, fmt.Sprintf("same-name-%d", suffix))

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM jobwork_receipts WHERE issue_id = $1`, firstIssue)
		_, _ = testPool.Exec(bg, `DELETE FROM jobwork_issues WHERE id = ANY($1)`, []int64{firstIssue, secondIssue})
		_, _ = testPool.Exec(bg, `DELETE FROM parties WHERE id = ANY($1)`, []int64{firstID, secondID})
		_, _ = testPool.Exec(bg, `DELETE FROM skus WHERE id = $1`, skuID)
	})

	result, err := testRead.stock(ctx, code, name)
	if err != nil {
		t.Fatalf("stock: %v", err)
	}
	if result.Count != 2 || result.Pairs != 2 {
		t.Fatalf("two workers share this name but got count=%d pairs=%d: %+v", result.Count, result.Pairs, result.Rows)
	}

	byWorker := map[int64]stockRow{}
	for _, row := range result.Rows {
		byWorker[row.JobWorkerID] = row
	}
	first, ok := byWorker[firstID]
	if !ok {
		t.Fatalf("row for worker %d missing: %+v", firstID, result.Rows)
	}
	second, ok := byWorker[secondID]
	if !ok {
		t.Fatalf("row for worker %d missing: %+v", secondID, result.Rows)
	}
	if first.Issued != 10 || first.Received != 4 || first.StillWith != 6 {
		t.Fatalf("first worker = %+v, want 10/4/6", first)
	}
	if second.Issued != 7 || second.Received != 0 || second.StillWith != 7 {
		t.Fatalf("second worker = %+v, want 7/0/7", second)
	}
	if result.TotalStillWith != 13 {
		t.Fatalf("total = %d, want 13", result.TotalStillWith)
	}
	if !strings.Contains(result.Note, "share a name") {
		t.Fatalf("note = %q, want it to flag the shared name", result.Note)
	}
}

// A capped list must never look complete.
func TestFindSkusReportsTruncation(t *testing.T) {
	ctx := context.Background()
	token := fmt.Sprintf("bulk-%d", time.Now().UnixNano())
	extra := 3
	total := findLimit + extra

	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	rows, err := testPool.Query(ctx, `
		INSERT INTO skus (code, name, image_url, rate_paise)
		SELECT 'BULK-' || $1 || '-' || g, 'Bulk ' || $2 || ' ' || g, '', 1000
		  FROM generate_series(1, $3) AS g
		RETURNING id`, stamp, token, total)
	if err != nil {
		t.Fatalf("insert bulk skus: %v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan bulk sku: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != total {
		t.Fatalf("inserted %d skus, want %d", len(ids), total)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM skus WHERE id = ANY($1)`, ids)
	})

	matches, err := testRead.findSkus(ctx, token)
	if err != nil {
		t.Fatalf("findSkus: %v", err)
	}
	if matches.Count != findLimit {
		t.Fatalf("count = %d, want the page limit %d", matches.Count, findLimit)
	}
	if matches.Total != total {
		t.Fatalf("total_matched = %d, want %d", matches.Total, total)
	}
	if !matches.Truncated {
		t.Fatal("a capped list must report itself as truncated")
	}
	if !strings.Contains(matches.Note, "showing the first") {
		t.Fatalf("note = %q, want the truncation explained", matches.Note)
	}
}

// The finders match case-insensitively; the original test only searched digits,
// so it could not tell LIKE from ILIKE.
func TestFindersIgnoreCase(t *testing.T) {
	ctx := context.Background()
	f := newAskFixture(t, 5, "")

	byCode, err := testRead.stock(ctx, strings.ToLower(f.skuCode), "")
	if err != nil {
		t.Fatalf("stock by lower-case code: %v", err)
	}
	if byCode.Count != 1 || byCode.Rows[0].SKUCode != f.skuCode {
		t.Fatalf("lower-case code lookup = %+v, want the fixture", byCode.Rows)
	}

	upperWorker := strings.ToUpper(f.partyName)
	byWorker, err := testRead.stock(ctx, "", upperWorker)
	if err != nil {
		t.Fatalf("stock by upper-case worker: %v", err)
	}
	if byWorker.Count != 1 || byWorker.Rows[0].JobWorkerID != f.partyID {
		t.Fatalf("upper-case worker lookup = %+v, want the fixture", byWorker.Rows)
	}
}

// Shutdown must answer queued questions rather than leaving them to time out.
func TestCloseAnswersQueuedQuestions(t *testing.T) {
	model := &blockingModel{gate: make(chan struct{})}
	// No workers at all: a queued job can only be answered by Close.
	svc := &Service{
		cfg:   Config{Model: "test", Concurrency: 0, QueueDepth: 2, QueueWait: 5 * time.Second},
		defs:  readToolDefs(),
		model: model,
	}
	svc.start()

	replies := make(chan error, 1)
	go func() {
		_, err := svc.Ask(context.Background(), "queued then closed")
		replies <- err
	}()
	waitFor(t, 3*time.Second, func() bool { return len(svc.jobs) == 1 }, "the question to be queued")

	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-replies:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("queued question = %v, want ErrUnavailable once the service is closed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a queued question was left waiting after Close")
	}

	if _, err := svc.Ask(context.Background(), "after close"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Ask after Close = %v, want ErrUnavailable", err)
	}
}

// Close must abort work in flight instead of outwaiting a model loop.
func TestCloseCancelsAQuestionInFlight(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	model := &blockingModel{gate: gate}
	svc := &Service{
		cfg:   Config{Model: "test", Concurrency: 1, QueueDepth: 1, QueueWait: time.Second},
		defs:  readToolDefs(),
		model: model,
	}
	svc.start()

	replies := make(chan error, 1)
	go func() {
		_, err := svc.Ask(context.Background(), "in flight when shutdown starts")
		replies <- err
	}()
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&model.inFlight) == 1 }, "the model call to start")

	if err := svc.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-replies:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight question = %v, want the cancellation surfaced", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel the question in flight")
	}
}

// A fully configured service must report itself live (not merely "not 503").
func TestNewWithFullConfigurationIsEnabled(t *testing.T) {
	svc, err := New(Config{
		APIKey:      "test-key",
		Model:       "test-model",
		BaseURL:     "http://127.0.0.1:1/v1",
		Concurrency: 2,
		QueueDepth:  1,
		QueueWait:   time.Second,
	}, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	if why := svc.Disabled(); why != "" {
		t.Fatalf("a configured service reports itself disabled: %q", why)
	}
}

func TestTracePayloadEmbedsJSONAndFallsBackToStrings(t *testing.T) {
	embedded := tracePayload(`{"count":1}`, 64)
	if string(embedded) != `{"count":1}` {
		t.Fatalf("embedded = %s, want the object itself", embedded)
	}
	var decoded map[string]int
	if err := json.Unmarshal(embedded, &decoded); err != nil || decoded["count"] != 1 {
		t.Fatalf("embedded payload is not an object: %s (%v)", embedded, err)
	}

	asString := tracePayload(`not json`, 64)
	if string(asString) != `"not json"` {
		t.Fatalf("fallback = %s, want a JSON string", asString)
	}

	truncated := tracePayload(`{"a":"`+strings.Repeat("x", 100)+`"}`, 20)
	if !json.Valid(truncated) {
		t.Fatalf("truncated payload is not valid JSON: %s", truncated)
	}
	var back string
	if err := json.Unmarshal(truncated, &back); err != nil {
		t.Fatalf("truncated payload should decode as a string: %s (%v)", truncated, err)
	}
	if !strings.HasSuffix(back, "…") {
		t.Fatalf("truncated payload = %q, want it marked as truncated", back)
	}

	if empty := tracePayload("   ", 64); string(empty) != `""` {
		t.Fatalf("empty payload = %s, want an empty string", empty)
	}
}

// The per-turn cap keeps one model response from running dozens of queries.
func TestRunCapsToolCallsPerTurn(t *testing.T) {
	f := newAskFixture(t, 5, "")

	calls := make([]llms.ToolCall, 0, maxToolCallsPerTurn+2)
	for i := 0; i < maxToolCallsPerTurn+2; i++ {
		calls = append(calls, toolCall(fmt.Sprintf("call-%d", i), toolFindSkus, fmt.Sprintf(`{"query":%q}`, f.skuFragment)))
	}
	model := &scriptedModel{turns: []scriptedTurn{
		{toolCalls: calls},
		{content: "done"},
	}}

	answer, err := testService(model).run(context.Background(), "ask for everything at once")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(answer.ToolCalls) != maxToolCallsPerTurn+2 {
		t.Fatalf("traced calls = %d, want %d executed plus 2 refused", len(answer.ToolCalls), maxToolCallsPerTurn+2)
	}
	executed, refused := 0, 0
	for _, record := range answer.ToolCalls {
		switch {
		case record.Error == "":
			executed++
		case strings.Contains(record.Error, "refused"):
			refused++
		default:
			t.Fatalf("unexpected tool error: %+v", record)
		}
	}
	if executed != maxToolCallsPerTurn || refused != 2 {
		t.Fatalf("executed %d, refused %d; want %d and 2", executed, refused, maxToolCallsPerTurn)
	}

	// Every tool call the transcript declares must be answered, or the provider
	// rejects the whole request (this is the bug the review pass missed).
	declared := map[string]bool{}
	answered := map[string]bool{}
	for _, message := range model.lastMessages() {
		switch message.Role {
		case llms.ChatMessageTypeAI:
			for _, part := range message.Parts {
				if call, ok := part.(llms.ToolCall); ok {
					declared[call.ID] = true
				}
			}
		case llms.ChatMessageTypeTool:
			for _, part := range message.Parts {
				if response, ok := part.(llms.ToolCallResponse); ok {
					answered[response.ToolCallID] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("the assistant turn was not echoed at all")
	}
	for id := range declared {
		if !answered[id] {
			t.Fatalf("tool call %s is in the transcript with no response; providers reject that", id)
		}
	}
	for id := range answered {
		if !declared[id] {
			t.Fatalf("tool response %s answers a call the transcript never declared", id)
		}
	}
	// Refusals are answered too — the model gets told why — so every call it
	// asked for is declared and paired.
	if want := maxToolCallsPerTurn + 2; len(declared) != want {
		t.Fatalf("declared calls = %d, want %d (executed plus refused)", len(declared), want)
	}
}

// A blank query lists the catalogue instead of erroring: refusing it is what
// made a live model search letter by letter.
func TestFindersListWhenTheQueryIsBlank(t *testing.T) {
	ctx := context.Background()

	skus, err := testRead.findSkus(ctx, "")
	if err != nil {
		t.Fatalf("findSkus(blank): %v", err)
	}
	if skus.Count == 0 || skus.Count > findLimit || skus.Total < skus.Count {
		t.Fatalf("blank SKU query = count %d, total %d; want a capped page of the catalogue", skus.Count, skus.Total)
	}
	if !strings.Contains(skus.Note, "no query given") {
		t.Fatalf("note = %q, want it to say the listing is a first page", skus.Note)
	}

	workers, err := testRead.findJobWorkers(ctx, "   ")
	if err != nil {
		t.Fatalf("findJobWorkers(blank): %v", err)
	}
	if workers.Count == 0 || workers.Count > findLimit {
		t.Fatalf("blank worker query = count %d; want a capped page of the catalogue", workers.Count)
	}
}
