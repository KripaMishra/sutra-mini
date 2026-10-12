package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/KripaMishra/sutra-mini/backend/internal/ask"
)

// asker is the /ask dependency: the real *ask.Service in production, a stub in
// handler tests. Keeping it this small lets the status mapping below
// (503/502/429/504) be tested without a model or a database.
type asker interface {
	Ask(ctx context.Context, question string) (ask.Answer, error)
}

type askRequest struct {
	Question string `json:"question"`
}

type askResponse struct {
	Question   string               `json:"question"`
	Answer     string               `json:"answer"`
	Model      string               `json:"model"`
	QueuedMS   int64                `json:"queued_ms"`
	DurationMS int64                `json:"duration_ms"`
	ToolCalls  []ask.ToolCallRecord `json:"tool_calls"`
}

// handleAsk answers a free-text question with the LLM over read-only database
// tools. Every tool call is logged server-side and returned in tool_calls.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if s.ask == nil {
		writeError(w, http.StatusServiceUnavailable, "ask is not enabled")
		return
	}

	var req askRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}

	// The request id rides along so each tool-call log line names the request.
	ctx := ask.WithRequestID(r.Context(), middleware.GetReqID(r.Context()))
	answer, err := s.ask.Ask(ctx, req.Question)

	switch {
	case err == nil:
		trace := answer.ToolCalls
		if trace == nil {
			trace = []ask.ToolCallRecord{}
		}
		writeJSON(w, http.StatusOK, askResponse{
			Question:   req.Question,
			Answer:     answer.Text,
			Model:      answer.Model,
			QueuedMS:   answer.Queued.Milliseconds(),
			DurationMS: answer.Duration.Milliseconds(),
			ToolCalls:  trace,
		})
	case errors.Is(err, ask.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ask.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, ask.ErrModel):
		logInternal("ask", err)
		writeError(w, http.StatusBadGateway, "model call failed")
	case errors.Is(err, context.DeadlineExceeded):
		// chi's timeout middleware answers 504 as well; whichever write wins,
		// the client sees a timeout rather than an empty 200.
		writeError(w, http.StatusGatewayTimeout, "ask timed out before the model finished")
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody to answer.
	default:
		logInternal("ask", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// askTimeout is the /ask budget: queue wait plus a few model turns.
const askTimeout = 60 * time.Second
