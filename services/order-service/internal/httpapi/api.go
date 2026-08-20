// Package httpapi is the HTTP boundary. It does three things and nothing more:
// parse and validate input, enforce idempotency, and translate domain errors
// into the Problem Details envelope. All business logic lives in orchestrator.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souq/order-service/internal/domain"
	"github.com/souq/order-service/internal/orchestrator"
	"github.com/souq/order-service/internal/platform"
	"github.com/souq/order-service/internal/saga"
	"github.com/souq/order-service/internal/store"
)

// maxBodyBytes caps request bodies. An order with 100 lines is comfortably
// under 64 KiB; anything larger is either a bug or an attempt to exhaust
// memory, and reading it to find out is the mistake.
const maxBodyBytes = 64 << 10

type API struct {
	orch  *orchestrator.Orchestrator
	store *store.Store
}

func New(o *orchestrator.Orchestrator, s *store.Store) *API {
	return &API{orch: o, store: s}
}

func (a *API) Mount(r chi.Router, auth func(http.Handler) http.Handler) {
	r.Route("/v1/orders", func(r chi.Router) {
		r.Use(auth)

		r.With(platform.Observe("/v1/orders")).Post("/", a.placeOrder)
		r.With(platform.Observe("/v1/orders")).Get("/", a.listOrders)
		r.With(platform.Observe("/v1/orders/{orderId}")).Get("/{orderId}", a.getOrder)
		r.With(platform.Observe("/v1/orders/{orderId}/status")).Get("/{orderId}/status", a.getStatus)
		r.With(platform.Observe("/v1/orders/{orderId}/stream")).Get("/{orderId}/stream", a.streamStatus)
		r.With(platform.Observe("/v1/orders/{orderId}/cancel")).Post("/{orderId}/cancel", a.cancelOrder)

		// Admin: the saga inspector.
		r.With(platform.RequireRole("OPS"), platform.Observe("/v1/orders/{orderId}/saga")).
			Get("/{orderId}/saga", a.getSagaTrace)
	})
}

// ---------------------------------------------------------------------------
// POST /v1/orders

type placeOrderBody struct {
	CartID             string          `json:"cartId"`
	CartVersion        int             `json:"cartVersion"`
	Items              []itemBody      `json:"items"`
	Subtotal           domain.Money    `json:"subtotal"`
	DiscountTotal      domain.Money    `json:"discountTotal"`
	ShippingTotal      domain.Money    `json:"shippingTotal"`
	TaxTotal           domain.Money    `json:"taxTotal"`
	ExpectedTotal      domain.Money    `json:"expectedTotal"`
	ShippingAddress    domain.Address  `json:"shippingAddress"`
	BillingAddress     *domain.Address `json:"billingAddress"`
	PaymentMethodToken string          `json:"paymentMethodToken"`
	RulesVersion       string          `json:"rulesVersion"`
}

type itemBody struct {
	SKU       string       `json:"sku"`
	ProductID string       `json:"productId"`
	Title     string       `json:"title"`
	ImageURL  string       `json:"image"`
	Quantity  int          `json:"quantity"`
	UnitPrice domain.Money `json:"unitPrice"`
}

func (a *API) placeOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := platform.UserIDFrom(ctx)

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		platform.WriteProblem(w, r, http.StatusBadRequest, platform.CodeValidationFailed,
			"This endpoint requires an Idempotency-Key header. Generate a UUIDv4 per checkout attempt and reuse it across retries.",
			nil)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		platform.WriteProblem(w, r, http.StatusRequestEntityTooLarge,
			platform.CodeValidationFailed, "Request body is too large.", err)
		return
	}

	var body placeOrderBody
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // a typo'd field must fail loudly, not be ignored
	if err := dec.Decode(&body); err != nil {
		platform.WriteProblem(w, r, http.StatusBadRequest, platform.CodeValidationFailed,
			"Request body is not valid JSON for this endpoint: "+err.Error(), err)
		return
	}

	hash, err := store.HashRequest(raw)
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		return
	}

	// Claim the idempotency key in its own short transaction, before doing any
	// work. Insert-first rather than select-then-insert, for the reason
	// payment-service internal/psp/paymob_test.go §4b makes concrete.
	var replay store.Replay
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		replay, err = store.ClaimKey(ctx, tx, key, userID, "POST /v1/orders", hash)
		return err
	})
	switch {
	case errors.Is(err, store.ErrKeyReused):
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeIdempotencyReuse,
			"This Idempotency-Key was already used with a different request body.", err)
		return
	case errors.Is(err, store.ErrInProgress):
		w.Header().Set("Retry-After", "1")
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeRequestInProgress,
			"An identical request is already being processed. Retry in a moment.", err)
		return
	case err != nil:
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		return
	}

	if replay.Outcome == store.ClaimedReplay {
		// Byte-identical replay of the original response. The client cannot
		// tell the difference, which is the entire contract.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(replay.ResponseCode)
		_, _ = w.Write(replay.ResponseBody)
		return
	}

	items := make([]domain.OrderItem, 0, len(body.Items))
	for i, it := range body.Items {
		items = append(items, domain.OrderItem{
			LineNo:    i,
			SKU:       it.SKU,
			ProductID: it.ProductID,
			Title:     it.Title,
			ImageURL:  it.ImageURL,
			Quantity:  it.Quantity,
			UnitPrice: it.UnitPrice,
			LineTotal: it.UnitPrice.Mul(it.Quantity),
		})
	}

	ord, err := a.orch.PlaceOrder(ctx, orchestrator.PlaceOrderInput{
		UserID:             userID,
		Items:              items,
		Subtotal:           body.Subtotal,
		DiscountTotal:      body.DiscountTotal,
		ShippingTotal:      body.ShippingTotal,
		TaxTotal:           body.TaxTotal,
		ExpectedTotal:      body.ExpectedTotal,
		ShippingAddress:    body.ShippingAddress,
		BillingAddress:     body.BillingAddress,
		PaymentMethodToken: body.PaymentMethodToken,
		RulesVersion:       body.RulesVersion,
		IdempotencyKey:     key,
	})
	if err != nil {
		// The order was never created and nothing external happened, so the
		// key is safe to release and the client can retry with the same one.
		// This is only sound because no side effect occurred — see the warning
		// on store.ReleaseKey.
		_ = a.store.InTx(ctx, func(tx pgx.Tx) error { return store.ReleaseKey(ctx, tx, key) })

		switch {
		case errors.Is(err, orchestrator.ErrInvalidOrder):
			platform.WriteProblem(w, r, http.StatusUnprocessableEntity,
				platform.CodeValidationFailed, err.Error(), err)
		case errors.Is(err, orchestrator.ErrTotalMismatch):
			platform.WriteProblem(w, r, http.StatusConflict, platform.CodeCartStale,
				"The cart total changed since it was displayed. Reload the cart and try again.", err)
		default:
			platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		}
		return
	}

	// 202, not 201: the saga has started, not finished.
	resp := map[string]any{
		"orderId":     ord.ID,
		"status":      string(ord.Status),
		"statusUrl":   "/v1/orders/" + ord.ID + "/status",
		"pollAfterMs": 500,
	}
	encoded, _ := json.Marshal(resp)

	if err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		return store.CompleteKey(ctx, tx, key, http.StatusAccepted, encoded)
	}); err != nil {
		// The order exists; failing to cache the response only costs us the
		// replay guarantee on this one key. Log and carry on.
		platform.WriteJSON(w, r, http.StatusAccepted, resp)
		return
	}

	w.Header().Set("Location", "/v1/orders/"+ord.ID)
	platform.WriteJSON(w, r, http.StatusAccepted, resp)
}

// ---------------------------------------------------------------------------
// GET /v1/orders/{orderId}

func (a *API) getOrder(w http.ResponseWriter, r *http.Request) {
	ord, ok := a.loadOwned(w, r)
	if !ok {
		return
	}
	platform.WriteJSON(w, r, http.StatusOK, ord)
}

// GET /v1/orders/{orderId}/status — the checkout page's polling endpoint.
// Deliberately small: it is hit every 500ms per in-flight checkout.
func (a *API) getStatus(w http.ResponseWriter, r *http.Request) {
	ord, ok := a.loadOwned(w, r)
	if !ok {
		return
	}
	platform.WriteJSON(w, r, http.StatusOK, statusPayload(ord))
}

func statusPayload(ord *domain.Order) map[string]any {
	return map[string]any{
		"orderId":            ord.ID,
		"status":             string(ord.Status),
		"terminal":           saga.IsTerminal(ord.Status),
		"cancellationReason": nilIfEmpty(string(ord.CancellationReason)),
		"updatedAt":          ord.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// GET /v1/orders/{orderId}/stream — Server-Sent Events.
//
// Offered alongside polling because a saga usually settles in under two
// seconds, and four poll round-trips to discover that is wasteful at scale.
// The client falls back to polling if SSE is unavailable (corporate proxies
// still break it), so this is an optimisation and never the only path.
func (a *API) streamStatus(w http.ResponseWriter, r *http.Request) {
	ord, ok := a.loadOwned(w, r)
	if !ok {
		return
	}

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		platform.WriteJSON(w, r, http.StatusOK, statusPayload(ord))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // stop nginx buffering the stream
	w.WriteHeader(http.StatusOK)

	send := func(o *domain.Order) {
		payload, _ := json.Marshal(statusPayload(o))
		fmt.Fprintf(w, "event: status\ndata: %s\n\n", payload)
		flusher.Flush()
	}

	send(ord)
	if saga.IsTerminal(ord.Status) {
		return
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Hard cap. An abandoned tab must not hold a connection forever; the
	// client reconnects, and a saga that has not settled in two minutes is a
	// paging matter rather than a UI one.
	deadline := time.After(2 * time.Minute)
	last := ord.Status

	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			fmt.Fprint(w, "event: timeout\ndata: {}\n\n")
			flusher.Flush()
			return
		case <-ticker.C:
			cur, err := a.store.GetOrder(r.Context(), ord.ID)
			if err != nil {
				return
			}
			if cur.Status != last {
				last = cur.Status
				send(cur)
				if saga.IsTerminal(cur.Status) {
					return
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// GET /v1/orders

func (a *API) listOrders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := platform.UserIDFrom(ctx)

	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			platform.WriteProblem(w, r, http.StatusBadRequest, platform.CodeValidationFailed,
				"limit must be an integer between 1 and 100.", err)
			return
		}
		limit = n
	}

	var cursorTime time.Time
	var cursorID string
	if c := r.URL.Query().Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			platform.WriteProblem(w, r, http.StatusBadRequest, platform.CodeValidationFailed,
				"cursor is malformed; omit it to start from the beginning.", err)
			return
		}
		cursorTime, cursorID = t, id
	}

	// Fetch one extra to know whether another page exists without a count(*).
	orders, err := a.store.ListOrders(ctx, userID, limit+1, cursorTime, cursorID)
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		return
	}

	hasMore := len(orders) > limit
	if hasMore {
		orders = orders[:limit]
	}
	var next *string
	if hasMore && len(orders) > 0 {
		l := orders[len(orders)-1]
		c := encodeCursor(l.PlacedAt, l.ID)
		next = &c
	}

	platform.WriteJSON(w, r, http.StatusOK, map[string]any{
		"items": orders, "nextCursor": next, "hasMore": hasMore,
	})
}

// ---------------------------------------------------------------------------
// POST /v1/orders/{orderId}/cancel

func (a *API) cancelOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID := chi.URLParam(r, "orderId")
	if !domain.ValidID("ord", orderID) {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", nil)
		return
	}

	err := a.orch.CancelOrder(ctx, orderID, platform.UserIDFrom(ctx))
	switch {
	case errors.Is(err, store.ErrNotFound):
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", err)
	case errors.Is(err, orchestrator.ErrNotCancellable):
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeNotCancellable,
			"This order has passed the point where it can be cancelled automatically. Contact support for a refund.", err)
	case err != nil:
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
	default:
		platform.WriteJSON(w, r, http.StatusAccepted, map[string]any{
			"orderId": orderID, "status": string(saga.StateCompensating),
		})
	}
}

// ---------------------------------------------------------------------------
// GET /v1/orders/{orderId}/saga  (OPS only)

func (a *API) getSagaTrace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orderID := chi.URLParam(r, "orderId")

	ord, err := a.store.GetOrder(ctx, orderID)
	if err != nil {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", err)
		return
	}
	steps, err := a.store.StepsFor(ctx, orderID)
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		return
	}

	out := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		out = append(out, map[string]any{
			"step": string(s.Step), "state": s.State, "attempts": s.Attempts,
			"sentAt": s.SentAt, "ackedAt": s.AckedAt, "deadlineAt": s.DeadlineAt,
			"eventId": nilIfEmpty(s.LastEventID), "error": nilIfEmpty(s.Error),
		})
	}

	platform.WriteJSON(w, r, http.StatusOK, map[string]any{
		"orderId": ord.ID,
		"status":  string(ord.Status),
		"steps":   out,
		// Drives the admin UI: hide "force cancel" past the point of no return.
		"rollbackForbidden": saga.RollbackForbidden(ord.Status),
		"correlationId":     ord.CorrelationID,
		"startedAt":         ord.PlacedAt,
		"updatedAt":         ord.UpdatedAt,
	})
}

// ---------------------------------------------------------------------------

// loadOwned fetches an order and checks the caller owns it. A missing order
// and someone else's order produce the same 404 — distinguishing them would
// let anyone probe for valid order ids.
func (a *API) loadOwned(w http.ResponseWriter, r *http.Request) (*domain.Order, bool) {
	ctx := r.Context()
	orderID := chi.URLParam(r, "orderId")

	if !domain.ValidID("ord", orderID) {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", nil)
		return nil, false
	}

	ord, err := a.store.GetOrder(ctx, orderID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", err)
			return nil, false
		}
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err)
		return nil, false
	}

	if ord.UserID != platform.UserIDFrom(ctx) &&
		!platform.HasRole(ctx, "OPS") && !platform.HasRole(ctx, "SUPPORT") {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeOrderNotFound, "", nil)
		return nil, false
	}
	return ord, true
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", errors.New("cursor is not in the expected format")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	return t, parts[1], nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
