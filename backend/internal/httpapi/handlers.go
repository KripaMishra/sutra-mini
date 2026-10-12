package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/KripaMishra/sutra-mini/backend/internal/store"
)

type createReceiptRequest struct {
	IssueID    int64  `json:"issue_id"`
	Qty        int    `json:"qty"`
	ReceivedOn string `json:"received_on"` // YYYY-MM-DD (anything else is a 400)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateReceipt(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required")
		return
	}

	var req createReceiptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.IssueID <= 0 || req.Qty <= 0 {
		writeError(w, http.StatusBadRequest, "issue_id and qty must be positive")
		return
	}
	receivedOn, err := time.Parse("2006-01-02", req.ReceivedOn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "received_on must be YYYY-MM-DD")
		return
	}

	result, err := s.store.CreateOrReplayReceipt(r.Context(), req.IssueID, req.Qty, receivedOn, key)
	switch {
	case err == nil:
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	case errors.Is(err, store.ErrIssueNotFound):
		writeError(w, http.StatusNotFound, "issue not found")
	case errors.Is(err, store.ErrOvershoot):
		writeError(w, http.StatusConflict, "receipt would make total received exceed the issued quantity")
	case errors.Is(err, store.ErrKeyReuse):
		writeError(w, http.StatusConflict, "Idempotency-Key was already used with a different request body")
	default:
		logInternal("create receipt", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid issue id")
		return
	}

	summary, err := s.store.GetIssue(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, summary)
	case errors.Is(err, store.ErrIssueNotFound):
		writeError(w, http.StatusNotFound, "issue not found")
	default:
		logInternal("get issue", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) handleListVouchers(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "pushed", "failed":
	default:
		writeError(w, http.StatusBadRequest, "status must be one of pending, pushed, failed")
		return
	}

	vouchers, err := s.store.ListVouchers(r.Context(), status)
	if err != nil {
		logInternal("list vouchers", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if vouchers == nil {
		vouchers = []store.Voucher{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vouchers": vouchers})
}

func (s *Server) handleRetryVoucher(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid voucher id")
		return
	}

	voucher, err := s.store.RetryVoucher(r.Context(), id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"voucher": voucher})
	case errors.Is(err, store.ErrVoucherNotFound):
		writeError(w, http.StatusNotFound, "voucher not found")
	case errors.Is(err, store.ErrNotRetryable):
		writeError(w, http.StatusConflict, "only failed vouchers can be retried")
	default:
		logInternal("retry voucher", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}
