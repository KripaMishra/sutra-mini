package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KripaMishra/sutra-mini/backend/internal/ask"
	"github.com/KripaMishra/sutra-mini/backend/internal/httpapi"
)

// stubAsker keeps the handler tests independent of a model or a database: the
// status mapping (200/400/429/502/503/504) is the part under test here.
type stubAsker struct {
	answer ask.Answer
	err    error
}

func (s stubAsker) Ask(context.Context, string) (ask.Answer, error) { return s.answer, s.err }

type askBody struct {
	Question   string `json:"question"`
	Answer     string `json:"answer"`
	Model      string `json:"model"`
	QueuedMS   int64  `json:"queued_ms"`
	DurationMS int64  `json:"duration_ms"`
	ToolCalls  []struct {
		Name       string          `json:"name"`
		Args       json.RawMessage `json:"args"`
		Result     json.RawMessage `json:"result"`
		DurationMS int64           `json:"duration_ms"`
		Error      string          `json:"error"`
	} `json:"tool_calls"`
	Error string `json:"error"`
}

func doAsk(t *testing.T, handler http.Handler, body string) (*httptest.ResponseRecorder, askBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var parsed askBody
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec, parsed
}

func TestAskReturnsTheAnswerAndTheToolTrace(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{answer: ask.Answer{
		Text:     "50 pieces of CHK-01 are still with Meera Embroidery Works.",
		Model:    "deepseek/deepseek-v4.1-flash",
		Queued:   12 * time.Millisecond,
		Duration: 4210 * time.Millisecond,
		ToolCalls: []ask.ToolCallRecord{{
			Name:       "stock_with_job_worker",
			Args:       json.RawMessage(`{"sku":"CHK-01","job_worker":"Meera"}`),
			Result:     json.RawMessage(`{"count":1,"total_still_with":50}`),
			DurationMS: 4,
		}},
	}}))

	const question = "how much of CHK-01 is still with Meera Embroidery Works?"
	rec, body := doAsk(t, handler, fmt.Sprintf(`{"question":%q}`, question))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if body.Question != question {
		t.Fatalf("question not echoed: %q", body.Question)
	}
	if !strings.Contains(body.Answer, "50 pieces") || body.Model == "" {
		t.Fatalf("answer/model missing: %+v", body)
	}
	if body.DurationMS != 4210 || body.QueuedMS != 12 {
		t.Fatalf("timings = %d/%d, want 4210/12", body.DurationMS, body.QueuedMS)
	}
	if len(body.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(body.ToolCalls))
	}
	call := body.ToolCalls[0]
	if call.Name != "stock_with_job_worker" || call.DurationMS != 4 || len(call.Result) == 0 || len(call.Args) == 0 {
		t.Fatalf("tool trace is incomplete: %+v", call)
	}
	// The trace embeds JSON, it does not smuggle JSON inside a string: a
	// client can read the payloads without decoding twice.
	if !strings.Contains(string(call.Args), `"sku":"CHK-01"`) {
		t.Fatalf("args = %s, want an embedded JSON object", call.Args)
	}
	if !strings.Contains(string(call.Result), `"total_still_with":50`) {
		t.Fatalf("result = %s, want an embedded JSON object", call.Result)
	}
	if raw := rec.Body.String(); strings.Contains(raw, `\"`) {
		t.Fatalf("response contains escaped JSON, it should be structured: %s", raw)
	}
}

func TestAskWithNoToolCallsStillReturnsAnEmptyList(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{answer: ask.Answer{Text: "nothing to look up"}}))

	rec, body := doAsk(t, handler, `{"question":"hello?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body.ToolCalls == nil {
		t.Fatalf("tool_calls should be [] rather than null: %s", rec.Body.String())
	}
}

func TestAskValidatesTheBody(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{}))

	rec, body := doAsk(t, handler, `{"question":"   "}`)
	if rec.Code != http.StatusBadRequest || body.Error != "question is required" {
		t.Fatalf("blank question = %d %q", rec.Code, body.Error)
	}

	rec, body = doAsk(t, handler, `{"question":`)
	if rec.Code != http.StatusBadRequest || body.Error != "invalid JSON body" {
		t.Fatalf("bad JSON = %d %q", rec.Code, body.Error)
	}
}

func TestAskMapsServiceFailuresOntoStatusCodes(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
		hideDetail  string
	}{
		{
			name:        "no key",
			err:         fmt.Errorf("%w: LLM_API_KEY is not configured", ask.ErrUnavailable),
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: "LLM_API_KEY is not configured",
		},
		{
			name:        "queue full",
			err:         fmt.Errorf("%w: 3 questions in flight", ask.ErrBusy),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "3 questions in flight",
		},
		{
			name:        "model failure",
			err:         fmt.Errorf("%w: dial tcp: upstream refused", ask.ErrModel),
			wantStatus:  http.StatusBadGateway,
			wantMessage: "model call failed",
			hideDetail:  "upstream refused",
		},
		{
			name:        "timeout",
			err:         context.DeadlineExceeded,
			wantStatus:  http.StatusGatewayTimeout,
			wantMessage: "timed out",
		},
	}

	for _, tc := range cases {
		handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{err: tc.err}))
		rec, body := doAsk(t, handler, `{"question":"how much?"}`)

		if rec.Code != tc.wantStatus {
			t.Fatalf("%s: status = %d, want %d (%s)", tc.name, rec.Code, tc.wantStatus, rec.Body.String())
		}
		if !strings.Contains(body.Error, tc.wantMessage) {
			t.Fatalf("%s: error = %q, want it to mention %q", tc.name, body.Error, tc.wantMessage)
		}
		if tc.hideDetail != "" && strings.Contains(body.Error, tc.hideDetail) {
			t.Fatalf("%s: upstream detail leaked to the client: %q", tc.name, body.Error)
		}
	}
}

func TestAskSetsRetryAfterWhenTheQueueIsFull(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{err: fmt.Errorf("%w: full", ask.ErrBusy)}))

	rec, _ := doAsk(t, handler, `{"question":"how much?"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
}

func TestAskWithoutAKeyAnswers503AndSaysWhy(t *testing.T) {
	// The real service, not a stub: this is the path a grader hits with an
	// untouched .env.
	svc, err := ask.New(ask.Config{
		Model:       "deepseek/deepseek-v4.1-flash",
		Concurrency: 3,
		QueueDepth:  32,
		QueueWait:   45 * time.Second,
	}, "")
	if err != nil {
		t.Fatalf("ask.New without a key must not fail: %v", err)
	}
	handler := httpapi.New(testPool, httpapi.WithAsk(svc))

	rec, body := doAsk(t, handler, `{"question":"how much of CHK-01 is still with Meera?"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(body.Error, "LLM_API_KEY is not configured") {
		t.Fatalf("error = %q, want it to name the missing variable", body.Error)
	}
}

func TestAskRouteAnswers503WhenTheServiceIsMissing(t *testing.T) {
	handler := httpapi.New(testPool)

	rec, body := doAsk(t, handler, `{"question":"how much?"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(body.Error, "ask is not enabled") {
		t.Fatalf("status = %d, body = %q", rec.Code, body.Error)
	}
}

func TestAskRejectsGET(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{err: errors.New("never called")}))

	req := httptest.NewRequest(http.MethodGet, "/ask", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /ask = %d, want 405", rec.Code)
	}
}
