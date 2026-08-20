// Package httpapi exposes payment-service over HTTP.
//
// The webhook below is the most security-sensitive endpoint in the platform.
// It is reachable from the public internet by design — the provider has to be
// able to reach it — and a bug in it lets anyone mark any order as paid.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/souq/payment-service/internal/psp"
	"github.com/souq/payment-service/internal/service"
)

// maxWebhookBody caps what we will read. A provider callback is a few
// kilobytes; anything larger is either a bug or an attempt to exhaust memory,
// and reading it to find out is the mistake.
const maxWebhookBody = 256 << 10

type WebhookHandler struct {
	provider psp.Provider
	svc      *service.Service
}

func NewWebhookHandler(p psp.Provider, s *service.Service) *WebhookHandler {
	return &WebhookHandler{provider: p, svc: s}
}

// ServeHTTP handles POST /v1/webhooks/{provider}.
//
// Five rules, each of which exists because breaking it has bitten somebody:
//
//  1. VERIFY BEFORE ANYTHING ELSE. No parsing into domain types, no database
//     lookups, no logging of the body — nothing that touches unverified input
//     beyond the signature check itself.
//
//  2. Return 200 for anything we have durably accepted, INCLUDING a duplicate.
//     Paymob retries a non-200 for hours; a 500 on a message we already
//     applied turns one callback into thousands.
//
//  3. Return 4xx for a bad signature and never retry-able 5xx. A forged
//     callback should be told no once, not invited back.
//
//  4. Do the work synchronously. Queueing it and returning 200 early means
//     acknowledging a payment we have not recorded — if the queue drops it,
//     the provider believes we know and will never tell us again.
//
//  5. Never log the raw body. It carries the PAN's last four, the cardholder
//     name, and a full billing address.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// The provider is waiting on this connection; a slow database must not
	// hold it open until the provider's own timeout fires and it retries.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var body []byte
	if r.Method == http.MethodPost {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
	}

	query := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			query[k] = vs[0]
		}
	}
	headers := map[string]string{}
	for _, k := range []string{"hmac", "x-paymob-signature"} {
		if v := r.Header.Get(k); v != "" {
			headers[k] = v
		}
	}

	// RULE 1.
	cb, err := h.provider.ParseCallback(ctx, body, headers, query)
	if err != nil {
		if errors.Is(err, psp.ErrInvalidSignature) {
			// Deliberately terse. A detailed error tells whoever is probing
			// exactly how close they got.
			slog.WarnContext(ctx, "rejected a webhook with an invalid signature",
				slog.String("remoteAddr", clientIP(r)),
				slog.String("provider", h.provider.Name()))
			// RULE 3. 401, not 500: this is not our fault and must not be retried.
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		slog.WarnContext(ctx, "could not parse a webhook",
			slog.String("provider", h.provider.Name()),
			slog.String("error", err.Error()))
		http.Error(w, "malformed callback", http.StatusBadRequest)
		return
	}

	slog.InfoContext(ctx, "webhook verified",
		slog.String("provider", h.provider.Name()),
		slog.String("kind", string(cb.Kind)),
		slog.String("providerRef", cb.ProviderRef),
		slog.String("merchantRef", cb.OrderID),
		slog.Bool("success", cb.Success),
		slog.Int64("amount", cb.Amount.Amount))

	// RULE 4.
	if err := h.svc.HandleCallback(ctx, cb); err != nil {
		// Unknown merchant reference is terminal: retrying will not make the
		// payment exist. Everything else might be transient, so let the
		// provider retry.
		if errors.Is(err, service.ErrNotFound) {
			slog.ErrorContext(ctx, "verified webhook references a payment we do not have",
				slog.String("merchantRef", cb.OrderID),
				slog.String("providerRef", cb.ProviderRef))
			// 200 anyway: there is nothing a retry can fix, and leaving the
			// provider retrying forever buries the real signal in noise. The
			// log line above is the alert.
			writeAck(w, "unknown-reference")
			return
		}

		slog.ErrorContext(ctx, "failed to apply a verified webhook; asking the provider to retry",
			slog.String("providerRef", cb.ProviderRef),
			slog.String("error", err.Error()))
		http.Error(w, "temporary failure, please retry", http.StatusServiceUnavailable)
		return
	}

	// RULE 2.
	writeAck(w, "ok")
}

func writeAck(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// clientIP prefers the ALB's X-Forwarded-For over RemoteAddr, which behind a
// load balancer is always the load balancer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Leftmost is the original client. It is client-controlled and so not
		// trustworthy for authorisation — this is for a log line only.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return r.RemoteAddr
}
