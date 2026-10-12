package ask

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// blockingModel holds every call until the gate opens and records how many
// calls overlap — the evidence that the queue really caps concurrency.
type blockingModel struct {
	gate        chan struct{}
	inFlight    int32
	maxInFlight int32
}

func (m *blockingModel) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	current := atomic.AddInt32(&m.inFlight, 1)
	for {
		maximum := atomic.LoadInt32(&m.maxInFlight)
		if current <= maximum || atomic.CompareAndSwapInt32(&m.maxInFlight, maximum, current) {
			break
		}
	}
	defer atomic.AddInt32(&m.inFlight, -1)

	select {
	case <-m.gate:
		return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "done"}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *blockingModel) Call(_ context.Context, _ string, _ ...llms.CallOption) (string, error) {
	return "", errors.New("blocking model does not implement Call")
}

func TestQueueKeepsExactlyThreeInFlight(t *testing.T) {
	gate := make(chan struct{})
	model := &blockingModel{gate: gate}
	// QueueDepth 0 makes the queue unbuffered, so a saturated service must
	// refuse the next question instead of silently stacking it.
	svc := &Service{
		cfg:   Config{Model: "test", Concurrency: 3, QueueDepth: 0, QueueWait: 300 * time.Millisecond},
		defs:  readToolDefs(),
		model: model,
	}
	svc.start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	replies := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := svc.Ask(context.Background(), "q")
			replies <- err
		}()
	}
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&model.inFlight) == 3 }, "three questions in flight")

	started := time.Now()
	_, err := svc.Ask(context.Background(), "the fourth question")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy once the queue is full, got %v", err)
	}
	// Bounded on both sides: too fast means the budget was not honoured, and
	// much too slow means the wait is not the configured one.
	if waited := time.Since(started); waited < 250*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("ErrBusy came back after %s, want roughly the 300ms wait budget", waited)
	}

	close(gate)
	for i := 0; i < 3; i++ {
		select {
		case err := <-replies:
			if err != nil {
				t.Fatalf("a queued question failed: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a queued question never got its answer")
		}
	}
	if got := atomic.LoadInt32(&model.maxInFlight); got != 3 {
		t.Fatalf("max in flight = %d, want exactly 3", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestNewReportsMissingVariablesInsteadOfFailing(t *testing.T) {
	// No key: boot succeeds, the endpoint reports itself unavailable.
	svc, err := New(Config{Model: "m", Concurrency: 3, QueueDepth: 1, QueueWait: time.Second}, "")
	if err != nil {
		t.Fatalf("a missing key must not fail boot: %v", err)
	}
	if why := svc.Disabled(); !strings.Contains(why, "LLM_API_KEY") {
		t.Fatalf("disabled reason = %q, want it to name LLM_API_KEY", why)
	}
	if _, err := svc.Ask(context.Background(), "q"); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Fatalf("Ask without a key = %v, want ErrUnavailable naming LLM_API_KEY", err)
	}

	// Key but no base URL: still a named problem, not a mystery.
	svc, err = New(Config{APIKey: "k", Model: "m", Concurrency: 3, QueueDepth: 1, QueueWait: time.Second}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if why := svc.Disabled(); !strings.Contains(why, "LLM_BASE_URL") {
		t.Fatalf("disabled reason = %q, want it to name LLM_BASE_URL", why)
	}

	// Configured model, no database: a real boot failure.
	if _, err := New(Config{APIKey: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1", Concurrency: 3, QueueDepth: 1, QueueWait: time.Second}, ""); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("New without DATABASE_URL = %v, want an error naming it", err)
	}
}

func TestNewValidatesQueueSettings(t *testing.T) {
	base := Config{APIKey: "k", Model: "m", BaseURL: "http://127.0.0.1:1/v1", Concurrency: 3, QueueDepth: 1, QueueWait: time.Second}
	// The messages matter: without asserting them, this test also passes when the
	// validation is gone and the failure comes from the unreachable database URL
	// below (which is exactly what a mutated build showed).
	mutations := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"no concurrency": {func(c *Config) { c.Concurrency = 0 }, "concurrency"},
		"negative depth": {func(c *Config) { c.QueueDepth = -1 }, "queue depth"},
		"no wait":        {func(c *Config) { c.QueueWait = 0 }, "queue wait"},
	}
	for name, tc := range mutations {
		cfg := base
		tc.mutate(&cfg)
		_, err := New(cfg, "postgres://example/db")
		if err == nil {
			t.Fatalf("%s: want a configuration error", name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to name %q", name, err, tc.want)
		}
	}
}

type panickingModel struct{ calls int32 }

func (m *panickingModel) GenerateContent(_ context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if atomic.AddInt32(&m.calls, 1) == 1 {
		panic("model exploded")
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "ok"}}}, nil
}

func (m *panickingModel) Call(_ context.Context, _ string, _ ...llms.CallOption) (string, error) {
	return "", errors.New("panicking model does not implement Call")
}

func TestAJobPanicDoesNotTakeTheWorkerDown(t *testing.T) {
	model := &panickingModel{}
	svc := &Service{
		cfg:   Config{Model: "test", Concurrency: 1, QueueDepth: 1, QueueWait: time.Second},
		defs:  readToolDefs(),
		model: model,
	}
	svc.start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := svc.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := svc.Ask(ctx, "boom"); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Ask against a panicking model = %v, want an error mentioning the panic", err)
	}

	answer, err := svc.Ask(ctx, "again")
	if err != nil {
		t.Fatalf("the worker did not survive the panic: %v", err)
	}
	if answer.Text != "ok" {
		t.Fatalf("answer = %q, want the next question to be served normally", answer.Text)
	}
}
