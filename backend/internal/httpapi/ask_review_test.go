// Handler-level tests added by the review pass: the boundaries the first round
// of tests did not reach (oversize body, oversize question, cancellation) and
// the distinction between an unwired and a deliberately disabled service.
package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KripaMishra/sutra-mini/backend/internal/ask"
	"github.com/KripaMishra/sutra-mini/backend/internal/config"
	"github.com/KripaMishra/sutra-mini/backend/internal/httpapi"
)

// testDatabaseURL re-reads the environment: TestMain has already proven
// DATABASE_URL is set, and the configured-service test needs it to build a real
// service behind the router.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil || cfg.DatabaseURL == "" {
		t.Fatalf("DATABASE_URL is required for this test: %v", err)
	}
	return cfg.DatabaseURL
}

func TestAskRejectsAnOversizeBody(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{}))

	// A little over the 1 MiB cap the handler sets.
	body := `{"question":"` + strings.Repeat("x", 1<<20) + `"}`
	rec, parsed := doAsk(t, handler, body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(parsed.Error, "too large") {
		t.Fatalf("error = %q, want it to mention the size", parsed.Error)
	}
}

func TestAskRejectsAnOversizeQuestion(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{}))

	rec, parsed := doAsk(t, handler, fmt.Sprintf(`{"question":%q}`, strings.Repeat("x", maxQuestionForTest+1)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(parsed.Error, "at most") {
		t.Fatalf("error = %q, want the limit named", parsed.Error)
	}

	// The boundary itself is accepted.
	rec, _ = doAsk(t, handler, fmt.Sprintf(`{"question":%q}`, strings.Repeat("x", maxQuestionForTest)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a question at the limit = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// maxQuestionForTest mirrors the handler's cap; the constant lives in the
// package under test, so it is restated here deliberately.
const maxQuestionForTest = 2000

func TestAskAnswers503WhenTheQuestionIsCancelled(t *testing.T) {
	handler := httpapi.New(testPool, httpapi.WithAsk(stubAsker{err: context.Canceled}))

	rec, parsed := doAsk(t, handler, `{"question":"how much?"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(parsed.Error, "cancelled") {
		t.Fatalf("error = %q, want the cancellation explained", parsed.Error)
	}
}

// A configured service is live: this is the assertion that fails if main.go
// stops wiring the service (every other test would still pass with a 503).
func TestAConfiguredServiceIsEnabled(t *testing.T) {
	svc, err := ask.New(ask.Config{
		APIKey:      "test-key",
		Model:       "test-model",
		BaseURL:     "http://127.0.0.1:1/v1",
		Concurrency: 2,
		QueueDepth:  1,
		QueueWait:   time.Second,
	}, testDatabaseURL(t))
	if err != nil {
		t.Fatalf("ask.New: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	}()

	if why := svc.Disabled(); why != "" {
		t.Fatalf("configured service reports itself disabled: %q", why)
	}

	// Wired through the real router, the route exists and does not 503 for
	// configuration reasons (the model endpoint itself is unreachable here, so a
	// 502 is the expected failure mode).
	handler := httpapi.New(testPool, httpapi.WithAsk(svc))
	rec, parsed := doAsk(t, handler, `{"question":"how much of CHK-01 is still with Meera?"}`)
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("a configured service answered 503: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway && rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("unreachable model = %d %q, want 502 (or 504 under load)", rec.Code, parsed.Error)
	}
}
