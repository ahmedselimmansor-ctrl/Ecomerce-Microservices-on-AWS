// Package httpapi is the HTTP boundary. Thin by design: parse, delegate to the
// stock engine, serialise.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souq/inventory-service/internal/platform"
	"github.com/souq/inventory-service/internal/stock"
	"github.com/souq/inventory-service/internal/store"
)

const maxBody = 64 << 10

type API struct {
	engine *stock.Engine
	store  *store.Store
}

func New(e *stock.Engine, s *store.Store) *API { return &API{engine: e, store: s} }

func (a *API) Mount(r chi.Router) {
	r.Route("/v1/stock", func(r chi.Router) {
		r.With(platform.Observe("/v1/stock")).Get("/", a.getBatch)
		r.With(platform.Observe("/v1/stock/{sku}")).Get("/{sku}", a.getOne)
		r.With(platform.Observe("/v1/stock/{sku}/adjust")).Post("/{sku}/adjust", a.adjust)
	})

	// The synchronous reservation path. The saga normally reserves over Kafka;
	// these exist for the "reserve before showing the payment step" flow some
	// clients prefer, and for operational tooling.
	r.Route("/v1/reservations", func(r chi.Router) {
		r.With(platform.Observe("/v1/reservations")).Post("/", a.reserve)
		r.With(platform.Observe("/v1/reservations/{id}")).Get("/{id}", a.getReservation)
		r.With(platform.Observe("/v1/reservations/{id}/release")).Post("/{id}/release", a.release)
		r.With(platform.Observe("/v1/reservations/{id}/commit")).Post("/{id}/commit", a.commit)
	})
}

// ---------------------------------------------------------------------- reads

func (a *API) getOne(w http.ResponseWriter, r *http.Request) {
	sku := chi.URLParam(r, "sku")
	levels, err := a.engine.Levels(r.Context(), []string{sku})
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
		return
	}
	if len(levels) == 0 {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeValidationFailed,
			"No such SKU.", nil, nil)
		return
	}
	platform.WriteJSON(w, r, http.StatusOK, levels[0])
}

func (a *API) getBatch(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("skus")
	if raw == "" {
		platform.WriteJSON(w, r, http.StatusOK, map[string]any{"items": []stock.Level{}})
		return
	}
	skus := strings.Split(raw, ",")
	// Capped: a listing page asking for more than this is paginating wrong,
	// and letting it through turns one slow query into a table scan.
	if len(skus) > 100 {
		platform.WriteProblem(w, r, http.StatusUnprocessableEntity, platform.CodeValidationFailed,
			"At most 100 SKUs per request.", nil, nil)
		return
	}
	levels, err := a.engine.Levels(r.Context(), skus)
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
		return
	}
	platform.WriteJSON(w, r, http.StatusOK, map[string]any{"items": levels})
}

// --------------------------------------------------------------- reservations

type reserveBody struct {
	OrderID       string              `json:"orderId"`
	ReservationID string              `json:"reservationId"`
	Items         []stock.ReserveItem `json:"items"`
	TTLSeconds    int                 `json:"ttlSeconds"`
}

func (a *API) reserve(w http.ResponseWriter, r *http.Request) {
	var body reserveBody
	if !decode(w, r, &body) {
		return
	}
	if body.OrderID == "" || len(body.Items) == 0 {
		platform.WriteProblem(w, r, http.StatusUnprocessableEntity, platform.CodeValidationFailed,
			"orderId and at least one item are required.", nil, nil)
		return
	}

	reservationID := body.ReservationID
	if reservationID == "" {
		reservationID = "rsv_" + strings.ToUpper(platform.NewID()[:26])
	}
	ttl := stock.DefaultTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}

	res, err := a.engine.Reserve(r.Context(), reservationID, body.OrderID, body.Items, ttl,
		platform.CorrelationIDFrom(r.Context()))
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
		return
	}

	platform.Reservations.WithLabelValues(string(res.Outcome)).Inc()

	switch res.Outcome {
	case stock.OutcomeReserved:
		platform.WriteJSON(w, r, http.StatusCreated, map[string]any{
			"reservationId": reservationID, "orderId": body.OrderID, "state": "RESERVED",
			"expiresAt": res.ExpiresAt.Format("2006-01-02T15:04:05.000Z"),
		})

	case stock.OutcomeFailed:
		for _, u := range res.Unavailable {
			platform.Stockouts.WithLabelValues(u.SKU).Inc()
		}
		// 409, not 422: the request was well formed, the world just does not
		// currently permit it. The distinction matters because a client should
		// retry a 409 after changing quantities and never retry a 422.
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeInsufficientStock,
			"Some items are unavailable.", nil,
			map[string]any{"reasonCode": res.ReasonCode, "unavailable": res.Unavailable})

	case stock.OutcomeAlreadyProcessed:
		// 200, not 409. A redelivery is normal on an at-least-once bus and the
		// caller's correct reaction is to carry on.
		platform.WriteJSON(w, r, http.StatusOK, map[string]any{
			"reservationId": reservationID, "orderId": body.OrderID,
			"state": string(res.State), "duplicate": true,
		})
	}
}

func (a *API) getReservation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	res, items, err := a.store.Reservation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeReservationMissing, "", nil, nil)
		return
	}
	if err != nil {
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
		return
	}

	lines := make([]map[string]any, 0, len(items))
	for _, l := range items {
		lines = append(lines, map[string]any{"sku": l.SKU, "quantity": l.Quantity})
	}
	out := map[string]any{
		"reservationId": res.ID, "orderId": res.OrderID, "state": string(res.State),
		"wasTombstone": res.WasTombstone, "items": lines,
	}
	if res.ExpiresAt != nil {
		out["expiresAt"] = res.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	platform.WriteJSON(w, r, http.StatusOK, out)
}

func (a *API) release(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		OrderID    string `json:"orderId"`
		ReasonCode string `json:"reasonCode"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ReasonCode == "" {
		body.ReasonCode = "MANUAL_RELEASE"
	}

	tombstone, err := a.engine.Release(r.Context(), id, body.OrderID, body.ReasonCode,
		platform.CorrelationIDFrom(r.Context()))
	switch {
	case errors.Is(err, stock.ErrReleaseAfterCommit):
		platform.CompensationAfterCommit.Inc()
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeAlreadyCommitted,
			"This reservation is committed. The stock may already be picked; issue a refund rather than a release.",
			err, nil)
	case err != nil:
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
	default:
		if tombstone {
			platform.Tombstones.Inc()
		}
		platform.WriteJSON(w, r, http.StatusOK, map[string]any{
			"reservationId": id, "state": "RELEASED", "wasTombstone": tombstone,
		})
	}
}

func (a *API) commit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		OrderID string `json:"orderId"`
	}
	if !decode(w, r, &body) {
		return
	}

	err := a.engine.Commit(r.Context(), id, body.OrderID, platform.CorrelationIDFrom(r.Context()))
	switch {
	case errors.Is(err, stock.ErrReservationNotFound):
		platform.WriteProblem(w, r, http.StatusNotFound, platform.CodeReservationMissing, "", err, nil)
	case errors.Is(err, stock.ErrNotReserved):
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeNotCommittable, err.Error(), err, nil)
	case err != nil:
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
	default:
		platform.WriteJSON(w, r, http.StatusOK, map[string]any{"reservationId": id, "state": "COMMITTED"})
	}
}

// ----------------------------------------------------------------- adjustment

func (a *API) adjust(w http.ResponseWriter, r *http.Request) {
	sku := chi.URLParam(r, "sku")
	var body struct {
		Delta    int    `json:"delta"`
		Movement string `json:"movement"`
		Note     string `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}

	allowed := map[string]bool{"RESTOCK": true, "ADJUSTMENT": true, "RETURN": true, "SHRINKAGE": true}
	if !allowed[body.Movement] {
		platform.WriteProblem(w, r, http.StatusUnprocessableEntity, platform.CodeValidationFailed,
			"movement must be RESTOCK, ADJUSTMENT, RETURN or SHRINKAGE.", nil, nil)
		return
	}

	// Every manual adjustment is attributed. A stock discrepancy investigation
	// that cannot name who changed the number is not an investigation.
	actor := r.Header.Get("X-Actor")
	if actor == "" {
		platform.WriteProblem(w, r, http.StatusUnprocessableEntity, platform.CodeValidationFailed,
			"Adjustments require an X-Actor header identifying who made the change.", nil, nil)
		return
	}

	level, err := a.engine.Adjust(r.Context(), sku, body.Delta, body.Movement, actor, body.Note,
		platform.CorrelationIDFrom(r.Context()))
	switch {
	case errors.Is(err, stock.ErrAdjustmentRejected):
		platform.WriteProblem(w, r, http.StatusConflict, platform.CodeAdjustmentRejected,
			"That adjustment would take on_hand below what is already reserved for customers mid-checkout.",
			err, nil)
	case err != nil:
		platform.WriteProblem(w, r, http.StatusInternalServerError, platform.CodeInternal, "", err, nil)
	default:
		platform.WriteJSON(w, r, http.StatusOK, level)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		platform.WriteProblem(w, r, http.StatusRequestEntityTooLarge,
			platform.CodeValidationFailed, "Request body is too large.", err, nil)
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// A typo'd field must fail loudly rather than being silently ignored.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		platform.WriteProblem(w, r, http.StatusBadRequest, platform.CodeValidationFailed,
			"Request body is not valid JSON for this endpoint: "+err.Error(), err, nil)
		return false
	}
	return true
}
