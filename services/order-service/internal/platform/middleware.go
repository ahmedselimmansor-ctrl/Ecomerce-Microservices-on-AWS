package platform

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RequestID assigns or propagates X-Request-Id. Everything downstream — logs,
// error envelopes, Kafka headers — carries it, so one id from a customer's
// screenshot finds every line of every service involved in their order.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newID()
		}
		ctx := WithRequestID(r.Context(), id)

		if corr := r.Header.Get("X-Correlation-Id"); corr != "" {
			ctx = WithCorrelationID(ctx, corr)
		} else {
			ctx = WithCorrelationID(ctx, id)
		}

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the status code so the metrics and access-log
// middleware can see what was actually written.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Observe records RED metrics and one structured access-log line per request.
// `route` must be the templated path (/v1/orders/{id}), never the concrete one,
// or the metric cardinality grows without bound and Prometheus falls over.
func Observe(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 0}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			elapsed := time.Since(start)

			HTTPRequests.WithLabelValues(route, r.Method, statusClass(rec.status)).Inc()
			HTTPDuration.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())

			slog.InfoContext(r.Context(), "http request",
				slog.String("route", route),
				slog.String("method", r.Method),
				slog.Int("status", rec.status),
				slog.Int64("durationMs", elapsed.Milliseconds()),
				slog.Int("bytes", rec.bytes),
				slog.String("requestId", RequestIDFrom(r.Context())),
				slog.String("correlationId", CorrelationIDFrom(r.Context())),
				slog.String("userId", UserIDFrom(r.Context())),
			)
		})
	}
}

func statusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// Recover turns a panic into a 500 with a request id instead of a dropped
// connection, and keeps the goroutine alive.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", v),
					slog.String("requestId", RequestIDFrom(r.Context())),
					slog.String("path", r.URL.Path),
				)
				WriteProblem(w, r, http.StatusInternalServerError, CodeInternal, "", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds every handler. Without it a slow dependency holds a
// connection, then a worker, then the whole pool, and one struggling service
// takes down its callers too.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"code":"UPSTREAM_TIMEOUT","status":504}`)
	}
}

// ---------------------------------------------------------------------------
// Authentication
//
// Tokens are verified LOCALLY against a cached JWKS (docs/CONTRACTS.md §7).
// Calling identity-service on every request would make it a synchronous
// dependency of literally everything, and its bad day would become everyone's.

type Claims struct {
	Sub   string   `json:"sub"`
	Iss   string   `json:"iss"`
	Aud   []string `json:"aud"`
	Exp   int64    `json:"exp"`
	Iat   int64    `json:"iat"`
	Jti   string   `json:"jti"`
	Roles []string `json:"roles"`
	Scope string   `json:"scope"`
	Sid   string   `json:"sid"`
	Amr   []string `json:"amr"`
}

// TokenVerifier is satisfied by the real JWKS-backed verifier in production
// and by a stub in tests. Keeping it an interface here means the HTTP layer
// has no opinion about how signatures are checked.
type TokenVerifier interface {
	Verify(token string) (*Claims, error)
}

// Authenticate rejects anything without a valid bearer token.
func Authenticate(v TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearer(r)
			if raw == "" {
				WriteProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated,
					"A bearer token is required.", nil)
				return
			}

			claims, err := v.Verify(raw)
			if err != nil {
				WriteProblem(w, r, http.StatusUnauthorized, CodeUnauthenticated,
					"The token is invalid or has expired.", err)
				return
			}

			ctx := WithUser(r.Context(), claims.Sub, claims.Roles)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole gates a route. Checked after Authenticate.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasRole(r.Context(), role) {
				WriteProblem(w, r, http.StatusForbidden, CodeForbidden,
					"This operation requires the "+role+" role.", nil)
				return
			}
			next.ServeHTTP(w, r.WithContext(r.Context()))
		})
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return h[7:]
	}
	return ""
}

// DecodeUnverifiedClaims reads a JWT payload WITHOUT checking the signature.
// It exists for one purpose: structured logging of the subject on requests
// that are about to be rejected. Never make an authorisation decision with it.
func DecodeUnverifiedClaims(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errMalformedToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

type constError string

func (e constError) Error() string { return string(e) }

const errMalformedToken = constError("malformed jwt")

// ConstantTimeEqual compares secrets without leaking their length or content
// through timing. Used for internal service tokens and webhook signatures.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
