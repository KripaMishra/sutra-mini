// Package accounting is the HTTP client for the accounting API. It has a
// per-request timeout and treats a 409 (duplicate client_ref) as success,
// reconciling to the reference the accounting side already holds.
package accounting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

type PushResult struct {
	ExternalRef string
	Duplicate   bool // accounting already had this client_ref (409)
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

type pushRequest struct {
	ClientRef   string `json:"client_ref"`
	AmountPaise int64  `json:"amount_paise"`
}

type pushResponse struct {
	ExternalRef string `json:"external_ref"`
}

// Push posts one voucher. A nil error means the voucher is recorded
// accounting-side, either by this call or an earlier one (duplicate).
// The error is retryable: HTTP failures, timeouts, malformed responses.
func (c *Client) Push(ctx context.Context, clientRef string, amountPaise int64) (PushResult, error) {
	body, err := json.Marshal(pushRequest{ClientRef: clientRef, AmountPaise: amountPaise})
	if err != nil {
		return PushResult{}, fmt.Errorf("encode voucher: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/vouchers", bytes.NewReader(body))
	if err != nil {
		return PushResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("push %s: %w", clientRef, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return PushResult{}, fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var pr pushResponse
		if err := json.Unmarshal(payload, &pr); err != nil || pr.ExternalRef == "" {
			return PushResult{}, fmt.Errorf("accounting returned 200 without external_ref: %s", truncate(payload))
		}
		return PushResult{ExternalRef: pr.ExternalRef}, nil

	case http.StatusConflict:
		// Duplicate client_ref: accounting already has this voucher and tells
		// us its reference. Reconcile instead of double-posting.
		var pr pushResponse
		if err := json.Unmarshal(payload, &pr); err != nil || pr.ExternalRef == "" {
			return PushResult{}, fmt.Errorf("accounting returned 409 without external_ref: %s", truncate(payload))
		}
		return PushResult{ExternalRef: pr.ExternalRef, Duplicate: true}, nil

	default:
		return PushResult{}, fmt.Errorf("accounting returned %d: %s", resp.StatusCode, truncate(payload))
	}
}

func truncate(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
