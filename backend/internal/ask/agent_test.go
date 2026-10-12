package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

// scriptedModel stands in for the LLM. It is an llms.Model like the real
// client, so the test drives the same loop, prompts and tool plumbing the
// endpoint uses against a live model — only the token generation is fake.
type scriptedModel struct {
	mu    sync.Mutex
	turns []scriptedTurn
	calls int
	seen  [][]llms.MessageContent
	err   error
}

type scriptedTurn struct {
	content   string
	toolCalls []llms.ToolCall
}

func (m *scriptedModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.seen = append(m.seen, messages)
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	if len(m.turns) == 0 {
		return nil, errors.New("scripted model has no turns")
	}
	// Walk the script; once it runs out, keep repeating the last turn.
	index := m.calls - 1
	if index >= len(m.turns) {
		index = len(m.turns) - 1
	}
	turn := m.turns[index]
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		Content:   turn.content,
		ToolCalls: turn.toolCalls,
	}}}, nil
}

func (m *scriptedModel) Call(_ context.Context, _ string, _ ...llms.CallOption) (string, error) {
	return "", errors.New("scripted model does not implement Call")
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *scriptedModel) lastMessages() []llms.MessageContent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) == 0 {
		return nil
	}
	return m.seen[len(m.seen)-1]
}

func toolCall(id, name, args string) llms.ToolCall {
	return llms.ToolCall{ID: id, Type: "function", FunctionCall: &llms.FunctionCall{Name: name, Arguments: args}}
}

func testService(m llms.Model) *Service {
	return &Service{cfg: Config{Model: "test-model"}, db: testRead, defs: readToolDefs(), model: m}
}

func TestRunExecutesToolsAndLogsEveryCall(t *testing.T) {
	f := newAskFixture(t, 120, "")
	f.addReceipt(t, 70, "only")

	model := &scriptedModel{turns: []scriptedTurn{
		{toolCalls: []llms.ToolCall{toolCall("call-1", toolStockWithJobWorker,
			fmt.Sprintf(`{"sku":%q,"job_worker":%q}`, f.skuCode, f.partyName))}},
		{toolCalls: []llms.ToolCall{toolCall("call-2", toolFindSkus, fmt.Sprintf(`{"query":%q}`, f.skuFragment))}},
		{content: fmt.Sprintf("50 pieces of %s are still with %s.", f.skuCode, f.partyName)},
	}}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)

	ctx := WithRequestID(context.Background(), "req-test-1")
	answer, err := testService(model).run(ctx, "how much of the SKU is still with the worker?")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if want := "50 pieces"; !strings.Contains(answer.Text, want) {
		t.Fatalf("answer = %q, want it to contain %q", answer.Text, want)
	}
	if answer.Model != "test-model" {
		t.Fatalf("model = %q", answer.Model)
	}
	if answer.Duration <= 0 {
		t.Fatal("duration was not recorded")
	}

	if len(answer.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2: %+v", len(answer.ToolCalls), answer.ToolCalls)
	}
	if answer.ToolCalls[0].Name != toolStockWithJobWorker || answer.ToolCalls[1].Name != toolFindSkus {
		t.Fatalf("tool calls in the wrong order: %+v", answer.ToolCalls)
	}
	if !strings.Contains(string(answer.ToolCalls[0].Result), `"still_with":50`) {
		t.Fatalf("trace result = %q, want the aggregate it computed", answer.ToolCalls[0].Result)
	}
	// The trace must be embedded JSON, not a JSON string: a decoder sees the
	// tool arguments as an object.
	var args struct {
		SKU       string `json:"sku"`
		JobWorker string `json:"job_worker"`
	}
	if err := json.Unmarshal(answer.ToolCalls[0].Args, &args); err != nil {
		t.Fatalf("tool args are not structured JSON (%s): %v", answer.ToolCalls[0].Args, err)
	}
	if args.SKU != f.skuCode || args.JobWorker != f.partyName {
		t.Fatalf("args = %+v, want the SKU and worker the model asked for", args)
	}
	var result struct {
		TotalStillWith int64 `json:"total_still_with"`
	}
	if err := json.Unmarshal(answer.ToolCalls[0].Result, &result); err != nil || result.TotalStillWith != 50 {
		t.Fatalf("tool result is not structured JSON (%s): %v", answer.ToolCalls[0].Result, err)
	}
	for _, call := range answer.ToolCalls {
		if call.Error != "" {
			t.Fatalf("unexpected tool error in %+v", call)
		}
		if len(call.Args) == 0 {
			t.Fatalf("tool call not fully traced: %+v", call)
		}
	}

	// The model must have been fed the tool result, not just a status.
	toolMessages := 0
	for _, message := range model.lastMessages() {
		if message.Role != llms.ChatMessageTypeTool {
			continue
		}
		toolMessages++
		if response, ok := message.Parts[0].(llms.ToolCallResponse); ok && strings.Contains(response.Content, `"still_with":50`) {
			return // found the aggregate on its way back to the model
		}
	}
	t.Fatalf("the aggregate never reached the model (tool messages seen: %d)", toolMessages)
}

func TestRunLogsEveryToolCallWithTheRequestID(t *testing.T) {
	f := newAskFixture(t, 10, "")

	model := &scriptedModel{turns: []scriptedTurn{
		{toolCalls: []llms.ToolCall{toolCall("call-1", toolStockBySKU, fmt.Sprintf(`{"sku":%q}`, f.skuCode))}},
		{content: "10 pieces are outstanding."},
	}}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)

	if _, err := testService(model).run(WithRequestID(context.Background(), "req-log-7"), "q"); err != nil {
		t.Fatalf("run: %v", err)
	}

	line := fmt.Sprintf("ask: req=req-log-7 tool=%s", toolStockBySKU)
	if !strings.Contains(logged.String(), line) {
		t.Fatalf("log output %q does not contain %q", logged.String(), line)
	}
}

func TestRunFeedsToolErrorsBackAndKeepsGoing(t *testing.T) {
	f := newAskFixture(t, 5, "")

	model := &scriptedModel{turns: []scriptedTurn{
		{toolCalls: []llms.ToolCall{toolCall("call-1", "drop_everything", `{"table":"skus"}`)}},
		{toolCalls: []llms.ToolCall{toolCall("call-2", toolFindSkus, `{"query":`)}},
		{toolCalls: []llms.ToolCall{toolCall("call-3", toolFindSkus, fmt.Sprintf(`{"query":%q}`, f.skuFragment))}},
		{content: "Found it on the third try."},
	}}

	answer, err := testService(model).run(context.Background(), "q")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if answer.Text != "Found it on the third try." {
		t.Fatalf("answer = %q", answer.Text)
	}
	if len(answer.ToolCalls) != 3 {
		t.Fatalf("tool calls = %d, want 3", len(answer.ToolCalls))
	}
	if answer.ToolCalls[0].Error == "" || !strings.Contains(answer.ToolCalls[0].Error, "unknown tool") {
		t.Fatalf("unknown tool should be recorded as an error: %+v", answer.ToolCalls[0])
	}
	if answer.ToolCalls[1].Error == "" {
		t.Fatalf("malformed arguments should be recorded as an error: %+v", answer.ToolCalls[1])
	}
	if !strings.Contains(string(answer.ToolCalls[0].Result), `"error"`) {
		t.Fatalf("a failed tool call should still trace a structured error payload: %s", answer.ToolCalls[0].Result)
	}
	if answer.ToolCalls[2].Error != "" {
		t.Fatalf("the recovery call should have succeeded: %+v", answer.ToolCalls[2])
	}

	// Errors go back to the model as structured payloads, so it can retry.
	sawError := false
	for _, message := range model.lastMessages() {
		if message.Role != llms.ChatMessageTypeTool {
			continue
		}
		if response, ok := message.Parts[0].(llms.ToolCallResponse); ok && strings.Contains(response.Content, `"error"`) {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("tool errors were not fed back to the model")
	}
}

func TestRunStopsAtTheStepBudget(t *testing.T) {
	f := newAskFixture(t, 5, "")

	// This model never stops calling a tool.
	model := &scriptedModel{turns: []scriptedTurn{
		{toolCalls: []llms.ToolCall{toolCall("call-1", toolFindSkus, fmt.Sprintf(`{"query":%q}`, f.skuFragment))}},
	}}

	answer, err := testService(model).run(context.Background(), "q")
	if !errors.Is(err, ErrModel) {
		t.Fatalf("want ErrModel when the model never finishes, got %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("no final answer after %d steps", maxSteps)) {
		t.Fatalf("error = %v, want the step budget named", err)
	}
	if len(answer.ToolCalls) != maxSteps {
		t.Fatalf("tool calls = %d, want the budget of %d", len(answer.ToolCalls), maxSteps)
	}
	if model.callCount() != maxSteps {
		t.Fatalf("model calls = %d, want %d (one per step)", model.callCount(), maxSteps)
	}
}

func TestRunSurfacesModelFailures(t *testing.T) {
	model := &scriptedModel{err: errors.New("upstream said no")}

	_, err := testService(model).run(context.Background(), "q")
	if !errors.Is(err, ErrModel) {
		t.Fatalf("want ErrModel, got %v", err)
	}
	if !strings.Contains(err.Error(), "upstream said no") {
		t.Fatalf("error = %v, want the upstream detail for the log", err)
	}
}

func TestRunPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	model := &scriptedModel{turns: []scriptedTurn{{content: "never reached"}}}
	_, err := testService(model).run(ctx, "q")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context error to pass through (so /ask can answer 504), got %v", err)
	}
	if errors.Is(err, ErrModel) {
		t.Fatalf("a cancelled request must not be reported as a model failure: %v", err)
	}
}
