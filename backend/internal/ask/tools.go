package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/llms"
)

// The tool surface is deliberately small and typed: the model can look up SKUs
// and job-workers, and read the issued/received/still-with ledger. It has no
// way to express a write — there is no raw SQL tool, and every finder runs in a
// read-only transaction.
const (
	toolFindSkus           = "find_skus"
	toolFindJobWorkers     = "find_job_workers"
	toolStockWithJobWorker = "stock_with_job_worker"
	toolStockBySKU         = "stock_by_sku"
	toolStockByJobWorker   = "stock_by_job_worker"
)

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// readToolDefs describes the tools to the model as OpenAI function schemas.
func readToolDefs() []llms.Tool {
	return []llms.Tool{
		{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        toolFindSkus,
				Description: "Find SKUs by a partial code or name (case-insensitive). Returns up to 20 matches with id, code, name and rate in paise; total_matched says how many matched in all.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": stringProp(`substring of the SKU code or name, e.g. "CHK-01" or "chikan"`)},
					"required":   []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        toolFindJobWorkers,
				Description: "Find job-workers (parties) by a partial name or phone (case-insensitive). Returns up to 20 matches with id and name, plus total_matched for the full count; contact details are never returned. Names are not unique, so use the id.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"query": stringProp(`substring of the job-worker name or phone, e.g. "meera"`)},
					"required":   []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        toolStockWithJobWorker,
				Description: "How much of one SKU is still with one job-worker: issued, received and still_with (pieces) for the matching pairs, keyed by job_worker_id and sku_id. This is the tool for questions like \"how much of SKU X is still with job-worker Y\". Two workers sharing a name appear as separate rows; check the note.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sku":        stringProp("SKU code or partial name"),
						"job_worker": stringProp("job-worker name or partial name"),
					},
					"required": []string{"sku", "job_worker"},
				},
			},
		},
		{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        toolStockBySKU,
				Description: "Rollup of one SKU across the job-workers holding it: issued, received and still_with per job-worker, up to 50 rows (check truncated and matched_pairs).",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"sku": stringProp("SKU code or partial name")},
					"required":   []string{"sku"},
				},
			},
		},
		{
			Type: "function",
			Function: &llms.FunctionDefinition{
				Name:        toolStockByJobWorker,
				Description: "Rollup of one job-worker across the SKUs they hold: issued, received and still_with per SKU, up to 50 rows (check truncated and matched_pairs).",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{"job_worker": stringProp("job-worker name or partial name")},
					"required":   []string{"job_worker"},
				},
			},
		},
	}
}

// callTool executes one tool call and returns the compact JSON the model sees.
// Errors come back as a structured payload rather than a bare string so a
// confused model can correct itself in the next turn.
func (s *Service) callTool(ctx context.Context, name, argsJSON string) (string, error) {
	switch name {
	case toolFindSkus:
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", name, err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("%s: query must not be empty", name)
		}
		result, err := s.db.findSkus(ctx, args.Query)
		return encodeResult(result, err)

	case toolFindJobWorkers:
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", name, err)
		}
		if strings.TrimSpace(args.Query) == "" {
			return "", fmt.Errorf("%s: query must not be empty", name)
		}
		result, err := s.db.findJobWorkers(ctx, args.Query)
		return encodeResult(result, err)

	case toolStockWithJobWorker:
		var args struct {
			SKU       string `json:"sku"`
			JobWorker string `json:"job_worker"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", name, err)
		}
		if strings.TrimSpace(args.SKU) == "" || strings.TrimSpace(args.JobWorker) == "" {
			return "", fmt.Errorf("%s: sku and job_worker must both be given", name)
		}
		result, err := s.db.stock(ctx, args.SKU, args.JobWorker)
		return encodeResult(result, err)

	case toolStockBySKU:
		var args struct {
			SKU string `json:"sku"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", name, err)
		}
		if strings.TrimSpace(args.SKU) == "" {
			return "", fmt.Errorf("%s: sku must not be empty", name)
		}
		result, err := s.db.stock(ctx, args.SKU, "")
		return encodeResult(result, err)

	case toolStockByJobWorker:
		var args struct {
			JobWorker string `json:"job_worker"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("%s: invalid arguments: %w", name, err)
		}
		if strings.TrimSpace(args.JobWorker) == "" {
			return "", fmt.Errorf("%s: job_worker must not be empty", name)
		}
		result, err := s.db.stock(ctx, "", args.JobWorker)
		return encodeResult(result, err)

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func encodeResult(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode tool result: %w", err)
	}
	return string(encoded), nil
}
