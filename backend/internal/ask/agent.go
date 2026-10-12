package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
)

const (
	// maxSteps bounds the tool loop. The headline question needs two calls
	// (resolve, then aggregate); six leaves room to recover from a mistyped
	// lookup without letting a confused model run all afternoon.
	maxSteps = 6
	// turnTimeout bounds one model call; toolTimeout bounds one tool call
	// (the read-only statement timeout inside it is 5s).
	turnTimeout = 25 * time.Second
	toolTimeout = 8 * time.Second
	// maxToolCallsPerTurn bounds one model turn: maxSteps bounds the turns, and
	// without this a single response asking for fifty tools would run all fifty.
	maxToolCallsPerTurn = 8
	// traceArgLimit and traceResultLimit bound what the response body carries;
	// payloads beyond them fall back to a truncated string.
	traceArgLimit    = 400
	traceResultLimit = 8192
)

// ToolCallRecord is one logged, traced tool call. Args and Result are embedded
// as JSON (not as JSON-in-a-string), so a trace reads like the payloads the
// model actually saw. The model itself always receives the full, untruncated
// result.
type ToolCallRecord struct {
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	DurationMS int64           `json:"duration_ms"`
	Error      string          `json:"error,omitempty"`

	// fullResult is the untruncated payload the model sees; Result above is
	// the response-safe copy.
	fullResult string
}

// Answer is one completed question: the text, the model that produced it, and
// the tool calls it took to get there.
type Answer struct {
	Text      string
	Model     string
	ToolCalls []ToolCallRecord
	Duration  time.Duration
	Queued    time.Duration
}

const systemPrompt = `You answer questions about a textile job-work tracking database.
Job-workers are parties who take SKUs (garments) away to work on them; issues record what went
out; receipts record what came back. "Still with job-worker Y" means issued minus received.

Schema (Postgres), which is all you can read:
  skus(id, code UNIQUE, name, rate_paise)
  parties(id, name, phone)                          -- job-workers; names are NOT unique
  jobwork_issues(id, party_id -> parties.id, sku_id -> skus.id, qty, issued_on)
  jobwork_receipts(id, issue_id -> jobwork_issues.id, qty, received_on)  -- one row per returned batch; the app never updates or deletes them, and receipts never exceed what was issued

Rules:
- Answer "how much of SKU X is still with job-worker Y" as sum(issued) - sum(received) for that pair; the stock tools compute exactly that.
- Resolve codes and names with the tools before quoting numbers, and prefer stock_with_job_worker for the headline question.
- Tool results carry ids (job_worker_id, sku_id): use them when names repeat, and never merge two workers that happen to share a name.
- Respect the note field in a tool result, and total_matched / truncated: a capped list is not a complete answer.
- If a lookup matches several rows, say which row you used; if it matches none, say you could not find it instead of guessing.
- Quote the numbers you used (issued, received, still_with) and the unit (pieces).
- Your access is read-only: you cannot change data, and you must never claim to have done so.
- Answer in one to three sentences of plain prose, no markdown tables.`

// run drives the tool loop: model turn, execute any tool calls, feed the
// results back, repeat until the model answers without asking for a tool.
func (s *Service) run(ctx context.Context, question string) (Answer, error) {
	started := time.Now()
	answer := Answer{Model: s.cfg.Model, ToolCalls: []ToolCallRecord{}}

	messages := []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextContent{Text: systemPrompt}}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextContent{Text: question}}},
	}

	for step := 1; step <= maxSteps; step++ {
		var choice *llms.ContentChoice
		err := func() error {
			turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
			defer cancel()
			response, err := s.model.GenerateContent(turnCtx, messages, llms.WithTools(s.defs))
			if err != nil {
				return err
			}
			if len(response.Choices) == 0 {
				return fmt.Errorf("%w: empty response", ErrModel)
			}
			choice = response.Choices[0]
			return nil
		}()
		if err != nil {
			s.logf(ctx, "llm turn=%d failed after %s: %v; request: %s",
				step, time.Since(started).Round(time.Millisecond), err, messageShape(messages))
			// A cancelled or timed-out request is not a model failure: let the
			// HTTP layer answer 504 / drop the connection instead of 502.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return answer, ctxErr
			}
			return answer, fmt.Errorf("%w: %v", ErrModel, err)
		}

		if len(choice.ToolCalls) == 0 {
			answer.Text = strings.TrimSpace(choice.Content)
			if answer.Text == "" {
				return answer, fmt.Errorf("%w: empty answer", ErrModel)
			}
			answer.Duration = time.Since(started)
			s.logf(ctx, "answered step=%d dur=%s tools=%d", step, answer.Duration.Round(time.Millisecond), len(answer.ToolCalls))
			return answer, nil
		}

		// Plan the turn before writing anything back. The rule the providers
		// enforce: every tool call that appears in the assistant message must be
		// followed by exactly one tool response. So a call we will not run is
		// either answered with a refusal (and echoed), or not echoed at all —
		// never echoed and left unanswered.
		type plannedCall struct {
			call    llms.ToolCall
			record  ToolCallRecord
			respond bool
		}
		plan := make([]plannedCall, 0, len(choice.ToolCalls))
		responded := 0
		for _, call := range choice.ToolCalls {
			item := plannedCall{call: call}
			switch {
			case call.FunctionCall == nil || strings.TrimSpace(call.FunctionCall.Name) == "":
				// No name to answer: keep it out of the transcript entirely.
				item.record = s.rejectedToolCall(ctx, call, "refused: tool call without a function name")
			case responded >= maxToolCallsPerTurn:
				item.record = s.rejectedToolCall(ctx, call,
					fmt.Sprintf("refused: at most %d tool calls are run per model turn; make fewer, better-aimed calls", maxToolCallsPerTurn))
				// Still responded to below: the id is already in the echo.
				item.respond = true
			default:
				item.record = s.runTool(ctx, call)
				item.respond = true
			}
			if item.respond {
				responded++
			}
			plan = append(plan, item)
			answer.ToolCalls = append(answer.ToolCalls, item.record)
		}

		if responded > 0 {
			// The echo is rebuilt from the calls we answer rather than using
			// choice.Parts: langchaingo's OpenAI client never populates Parts,
			// and rebuilding guarantees the transcript stays pairable.
			parts := make([]llms.ContentPart, 0, responded)
			for _, item := range plan {
				if item.respond {
					parts = append(parts, item.call)
				}
			}
			messages = append(messages, llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: parts})
			for _, item := range plan {
				if !item.respond {
					continue
				}
				messages = append(messages, llms.MessageContent{
					Role: llms.ChatMessageTypeTool,
					Parts: []llms.ContentPart{llms.ToolCallResponse{
						ToolCallID: item.call.ID,
						Name:       item.record.Name,
						Content:    item.record.fullResult,
					}},
				})
			}
		}
		s.logf(ctx, "llm turn=%d dur=%s asked=%d answered=%d refused=%d", step,
			time.Since(started).Round(time.Millisecond), len(choice.ToolCalls), responded, len(choice.ToolCalls)-responded)
	}

	answer.Duration = time.Since(started)
	return answer, fmt.Errorf("%w: no final answer after %d steps", ErrModel, maxSteps)
}

// runTool executes one tool call, records the trace, and logs the call.
func (s *Service) runTool(ctx context.Context, call llms.ToolCall) ToolCallRecord {
	record := ToolCallRecord{}
	args := ""
	if call.FunctionCall != nil {
		record.Name = call.FunctionCall.Name
		args = call.FunctionCall.Arguments
	}
	record.Args = tracePayload(args, traceArgLimit)

	toolCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.callTool(toolCtx, record.Name, args)
	record.DurationMS = time.Since(started).Milliseconds()

	if err != nil {
		record.Error = err.Error()
		// The model gets a structured error so it can retry intelligently.
		encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
		record.fullResult = string(encoded)
	} else {
		record.fullResult = result
	}
	record.Result = tracePayload(record.fullResult, traceResultLimit)

	s.logf(ctx, "tool=%s dur=%dms args=%s err=%v result=%s",
		record.Name, record.DurationMS, truncate(args, traceArgLimit), record.Error, truncate(record.fullResult, 300))
	return record
}

// rejectedToolCall records, without executing, a tool call the loop will not
// run: the reason is logged and traced so the cap is visible instead of silent.
// When the call is still answered (the id is already in the transcript) the
// model receives the reason as its tool result.
func (s *Service) rejectedToolCall(ctx context.Context, call llms.ToolCall, reason string) ToolCallRecord {
	record := ToolCallRecord{Error: reason}
	if call.FunctionCall != nil {
		record.Name = call.FunctionCall.Name
		record.Args = tracePayload(call.FunctionCall.Arguments, traceArgLimit)
	}
	encoded, _ := json.Marshal(map[string]string{"error": reason})
	record.Result = encoded
	record.fullResult = string(encoded)
	s.logf(ctx, "tool=%s %s", record.Name, reason)
	return record
}

// messageShape summarizes the outbound transcript: how many tool calls it
// declares and how many it answers. A mismatch there is what providers reject
// with a bare "invalid request", so log it whenever a model call fails.
func messageShape(messages []llms.MessageContent) string {
	declared, answered := 0, 0
	shape := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case llms.ChatMessageTypeSystem:
			shape = append(shape, "sys")
		case llms.ChatMessageTypeHuman:
			shape = append(shape, "user")
		case llms.ChatMessageTypeAI:
			calls := 0
			for _, part := range message.Parts {
				if _, ok := part.(llms.ToolCall); ok {
					calls++
				}
			}
			declared += calls
			shape = append(shape, fmt.Sprintf("ai(%d calls)", calls))
		case llms.ChatMessageTypeTool:
			answered++
			shape = append(shape, "tool")
		default:
			shape = append(shape, string(message.Role))
		}
	}
	return fmt.Sprintf("%d messages, %d tool calls declared, %d answered [%s]",
		len(messages), declared, answered, strings.Join(shape, " "))
}

// tracePayload embeds a payload in the trace as JSON when it is valid and
// within the limit, and as a plain string otherwise. Either way the response
// stays valid JSON, and well-formed tool output never arrives double-escaped.
func tracePayload(payload string, limit int) json.RawMessage {
	trimmed := strings.TrimSpace(payload)
	if trimmed != "" && len(trimmed) <= limit && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	encoded, _ := json.Marshal(truncate(trimmed, limit))
	return encoded
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…"
}

func (s *Service) logf(ctx context.Context, format string, args ...any) {
	log.Printf("ask: req=%s "+format, append([]any{requestID(ctx)}, args...)...)
}
