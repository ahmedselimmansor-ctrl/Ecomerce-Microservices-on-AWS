// Package platform holds the cross-cutting machinery every SOUQ service needs:
// the error envelope, config loading, structured logging, and metrics. The Go
// services share this shape; the Java, Python and Node services
// reimplement the same contract in their own idiom (docs/CONTRACTS.md §2).
package platform

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Problem is the RFC 9457 Problem Details envelope, extended with the fields
// docs/CONTRACTS.md §2.2 requires. Every 4xx and 5xx from every service in
// every language serialises to exactly this shape, so the frontend has one
// error path rather than eleven.
type Problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Instance  string       `json:"instance,omitempty"`
	Code      string       `json:"code"`
	RequestID string       `json:"requestId"`
	Timestamp string       `json:"timestamp"`
	Errors    []FieldError `json:"errors,omitempty"`
}

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error codes. Adding one here without adding it to ERROR_CODES in
// libs/ts-contracts/src/primitives.ts means the storefront renders a generic
// failure instead of a useful message — degraded, not broken, but catch it in
// review.
const (
	CodeValidationFailed  = "VALIDATION_FAILED"
	CodeWeakPassword      = "WEAK_PASSWORD"
	CodeUnauthenticated   = "UNAUTHENTICATED"
	CodeForbidden         = "FORBIDDEN"
	CodeOrderNotFound     = "ORDER_NOT_FOUND"
	CodeCartNotFound      = "CART_NOT_FOUND"
	CodeCartStale         = "CART_STALE"
	CodeInsufficientStock = "INVENTORY_INSUFFICIENT_STOCK"
	CodeIdempotencyReuse  = "IDEMPOTENCY_KEY_REUSE"
	CodeRequestInProgress = "REQUEST_IN_PROGRESS"
	CodeNotCancellable    = "ORDER_NOT_CANCELLABLE"
	CodeAccountLocked     = "ACCOUNT_LOCKED"
	CodeRateLimited       = "RATE_LIMITED"
	CodeUpstreamDown      = "UPSTREAM_UNAVAILABLE"
	CodeUpstreamTimeout   = "UPSTREAM_TIMEOUT"
	CodeInternal          = "INTERNAL_ERROR"
)

const errorBaseURI = "https://errors.souq.dev/order/"

// WriteProblem renders a Problem and logs it. 5xx are logged at error with the
// underlying cause; 4xx at info without it, because a client sending bad input
// is not an operational event and paging on it trains people to ignore alerts.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, cause error, fields ...FieldError) {
	reqID := RequestIDFrom(r.Context())

	p := Problem{
		Type:      errorBaseURI + slugify(code),
		Title:     titleFor(code),
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		Code:      code,
		RequestID: reqID,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Errors:    fields,
	}

	attrs := []any{
		slog.String("code", code),
		slog.Int("status", status),
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("requestId", reqID),
	}
	if status >= 500 {
		if cause != nil {
			attrs = append(attrs, slog.String("error", cause.Error()))
		}
		slog.ErrorContext(r.Context(), "request failed", attrs...)
	} else {
		slog.InfoContext(r.Context(), "request rejected", attrs...)
	}

	// A 5xx must never leak the cause to the client — it can carry SQL,
	// hostnames, or a customer's data from an adjacent row.
	if status >= 500 {
		p.Detail = "The request could not be completed. Quote the requestId to support."
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Request-Id", reqID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// WriteJSON renders a success response.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", RequestIDFrom(r.Context()))
	w.WriteHeader(status)
	if body != nil {
		if err := json.NewEncoder(w).Encode(body); err != nil {
			// Headers are already out; all we can do is record it.
			slog.ErrorContext(r.Context(), "failed to encode response body",
				slog.String("error", err.Error()))
		}
	}
}

func titleFor(code string) string {
	switch code {
	case CodeValidationFailed:
		return "Validation failed"
	case CodeUnauthenticated:
		return "Authentication required"
	case CodeForbidden:
		return "Forbidden"
	case CodeOrderNotFound:
		return "Order not found"
	case CodeCartNotFound:
		return "Cart not found"
	case CodeCartStale:
		return "Cart changed since it was loaded"
	case CodeInsufficientStock:
		return "Insufficient stock"
	case CodeIdempotencyReuse:
		return "Idempotency key reused with a different body"
	case CodeRequestInProgress:
		return "An identical request is already in progress"
	case CodeNotCancellable:
		return "Order can no longer be cancelled"
	case CodeRateLimited:
		return "Too many requests"
	case CodeUpstreamDown:
		return "A dependency is unavailable"
	case CodeUpstreamTimeout:
		return "A dependency timed out"
	default:
		return "Internal error"
	}
}

func slugify(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c == '_':
			out = append(out, '-')
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
