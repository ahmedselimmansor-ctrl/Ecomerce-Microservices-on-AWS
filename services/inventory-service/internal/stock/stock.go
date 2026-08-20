// Package stock is the reservation engine — the code the concurrency model in
// model_test.go was written for.
//
// This is the hottest row in the platform. On a flash sale, hundreds of
// checkout transactions hit the same SKU in the same millisecond, and an
// oversell is the most expensive bug this system can ship: it is only
// discovered at fulfilment, after the customer has been charged.
//
// The safe implementation and the broken one differ by about ten characters of
// SQL, and the broken one passes any test that only checks
// `reserved <= on_hand`. Read docs/DESIGN-INVARIANTS.md §3 before changing
// anything in this file.
package stock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souq/inventory-service/internal/events"
	"github.com/souq/inventory-service/internal/store"
)

// DefaultTTL is how long a reservation holds stock.
//
// Long enough for a customer to finish 3-D Secure on a slow provider; short
// enough that an abandoned checkout does not keep the last unit of a hot SKU
// out of sale for an hour.
const DefaultTTL = 15 * time.Minute

type ReserveItem struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type UnavailableSKU struct {
	SKU       string `json:"sku"`
	Requested int    `json:"requested"`
	Available int    `json:"available"`
}

type Outcome string

const (
	// OutcomeReserved: stock is held.
	OutcomeReserved Outcome = "RESERVED"
	// OutcomeFailed: nothing was held, and Unavailable says exactly which
	// SKUs fell short so the storefront can say something useful.
	OutcomeFailed Outcome = "FAILED"
	// OutcomeAlreadyProcessed: a redelivery. Not an error — Kafka is
	// at-least-once and a repeat must be a no-op, not a second hold.
	OutcomeAlreadyProcessed Outcome = "ALREADY_PROCESSED"
)

type ReserveResult struct {
	Outcome     Outcome
	State       store.ReservationState
	ExpiresAt   time.Time
	ReasonCode  string
	Unavailable []UnavailableSKU
}

var (
	// ErrReleaseAfterCommit means the saga tried to compensate past the point
	// of no return. It should be impossible — order-service refuses to emit it
	// and its sweeper double-checks — so it is a loud error, not a quiet 400.
	ErrReleaseAfterCommit = errors.New("reservation is already committed and cannot be released")

	ErrReservationNotFound = errors.New("reservation not found")
	ErrNotReserved         = errors.New("reservation is not in a committable state")
	ErrAdjustmentRejected  = errors.New("adjustment would take on_hand below reserved or below zero")
)

type Engine struct {
	store *store.Store
}

func New(s *store.Store) *Engine { return &Engine{store: s} }

// ---------------------------------------------------------------------------
// Reserve

// Reserve holds stock for an order, all-or-nothing.
//
// # Why the SQL looks the way it does
//
// The single most important statement in this service is the conditional
// UPDATE in takeOne. The obvious alternative —
//
//	SELECT reserved FROM stock_levels WHERE sku = $1;      -- check
//	UPDATE stock_levels SET reserved = reserved + $2 ...;  -- then act
//
// oversells, and the model finds it in four steps: two buyers read
// reserved = 0 against on_hand = 2, both pass the check, both increment, and
// the row lands at 4.
//
// # Why the SKUs are sorted
//
// Two concurrent multi-SKU orders touching {A,B} and {B,A} deadlock if they
// take row locks in arrival order. Sorting gives every transaction a global
// ordering, so one waits for the other instead of each holding what the other
// needs.
func (e *Engine) Reserve(
	ctx context.Context,
	reservationID, orderID string,
	items []ReserveItem,
	ttl time.Duration,
	correlationID string,
) (ReserveResult, error) {
	if len(items) == 0 {
		return ReserveResult{}, fmt.Errorf("a reservation must contain at least one item")
	}

	// Deduplicate and sort. A payload with the same SKU twice is a caller bug,
	// but summing is the only interpretation that cannot silently under-reserve.
	wanted := map[string]int{}
	for _, it := range items {
		if it.Quantity <= 0 {
			return ReserveResult{}, fmt.Errorf("quantity for %s must be positive, got %d", it.SKU, it.Quantity)
		}
		wanted[it.SKU] += it.Quantity
	}

	skus := make([]string, 0, len(wanted))
	for sku := range wanted {
		skus = append(skus, sku)
	}
	sort.Strings(skus) // deadlock avoidance — see the doc comment

	var result ReserveResult
	var unavailable []UnavailableSKU

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		// Idempotency and tombstone check in one query. An existing row means
		// either a redelivery (RESERVED/COMMITTED) or, crucially, a tombstone
		// written by a Release that overtook us (RELEASED).
		existing, err := store.LoadReservationForUpdate(ctx, tx, reservationID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			result = ReserveResult{Outcome: OutcomeAlreadyProcessed, State: existing.State}
			return nil
		}

		// A second reservation for the same order means the saga restarted and
		// minted a new id. Honouring it would double-hold the stock.
		if other, err := store.ReservationForOrder(ctx, tx, orderID); err == nil && other != reservationID {
			result = ReserveResult{Outcome: OutcomeAlreadyProcessed, State: store.StateReserved}
			return nil
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}

		expiresAt := time.Now().UTC().Add(ttl)
		if err := store.InsertReservation(ctx, tx, reservationID, orderID, store.StateReserved, false, "", &expiresAt); err != nil {
			return err
		}

		for _, sku := range skus {
			qty := wanted[sku]

			taken, onHand, reserved, err := store.TryTake(ctx, tx, sku, qty)
			if err != nil {
				return err
			}
			if !taken {
				available, known, err := store.AvailableFor(ctx, tx, sku)
				if err != nil {
					return err
				}
				if !known {
					available = 0
				}
				unavailable = append(unavailable, UnavailableSKU{SKU: sku, Requested: qty, Available: available})
				continue
			}

			if err := store.InsertReservationItem(ctx, tx, reservationID, sku, qty); err != nil {
				return err
			}
			if err := store.WriteLedger(ctx, tx, store.LedgerEntry{
				SKU: sku, ReservationID: reservationID, OrderID: orderID,
				Movement: "RESERVATION", Quantity: qty,
				OnHandAfter: onHand, ReservedAfter: reserved,
			}); err != nil {
				return err
			}
		}

		if len(unavailable) > 0 {
			// ALL OR NOTHING. Returning an error rolls the transaction back,
			// undoing every successful take above. A partial reservation would
			// be worse than a failure: the customer gets an error AND the
			// stock stays held.
			return errPartial
		}

		reserved := make([]events.Item, 0, len(skus))
		for _, sku := range skus {
			reserved = append(reserved, events.Item{SKU: sku, Quantity: wanted[sku]})
		}

		// The event goes in the SAME transaction as the stock change.
		// Publishing after COMMIT loses events on crash — the outbox model
		// finds it in two steps.
		if err := events.Enqueue(ctx, tx, events.Reserved(orderID, reservationID, reserved, expiresAt, correlationID)); err != nil {
			return err
		}

		result = ReserveResult{Outcome: OutcomeReserved, State: store.StateReserved, ExpiresAt: expiresAt}
		return nil
	})

	if errors.Is(err, errPartial) {
		// The failure event still has to be published, in its own
		// transaction. Without it the saga waits for a reply that never comes
		// and only recovers on its 30-second timeout.
		if pubErr := e.publishFailure(ctx, reservationID, orderID, unavailable, correlationID); pubErr != nil {
			return ReserveResult{}, pubErr
		}
		return ReserveResult{
			Outcome:     OutcomeFailed,
			State:       store.StateFailed,
			ReasonCode:  "INSUFFICIENT_STOCK",
			Unavailable: unavailable,
		}, nil
	}
	if err != nil {
		return ReserveResult{}, err
	}
	return result, nil
}

var errPartial = errors.New("insufficient stock on one or more lines")

func (e *Engine) publishFailure(ctx context.Context, reservationID, orderID string, unavailable []UnavailableSKU, correlationID string) error {
	return e.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := store.InsertReservationIfAbsent(ctx, tx, reservationID, orderID,
			store.StateFailed, "INSUFFICIENT_STOCK"); err != nil {
			return err
		}
		return events.Enqueue(ctx, tx,
			events.ReservationFailed(orderID, reservationID, "INSUFFICIENT_STOCK", toEventSKUs(unavailable), correlationID))
	})
}

func toEventSKUs(in []UnavailableSKU) []events.UnavailableSKU {
	out := make([]events.UnavailableSKU, 0, len(in))
	for _, u := range in {
		out = append(out, events.UnavailableSKU{SKU: u.SKU, Requested: u.Requested, Available: u.Available})
	}
	return out
}

// ---------------------------------------------------------------------------
// Release

// Release returns held stock to sale.
//
// # The tombstone
//
// A Release may arrive for a reservation this service has never seen. That is
// not corruption: the saga timed out and compensated while the Reserve command
// was still in the consumer's buffer.
//
// The wrong response is to ignore it. The Reserve then arrives, creates a
// reservation nobody will ever release, and the saga waits forever for a
// Released event. So we write a RELEASED row anyway — a tombstone — and the
// late Reserve finds it and declines. docs/DESIGN-INVARIANTS.md §2, and
// TestWithoutTombstonesTheSagaCanWedge in order-service proves the saga wedges
// without it.
func (e *Engine) Release(ctx context.Context, reservationID, orderID, reasonCode, correlationID string) (bool, error) {
	var wasTombstone bool

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		existing, err := store.LoadReservationForUpdate(ctx, tx, reservationID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			// THE TOMBSTONE.
			wasTombstone = true
			expiry := (*time.Time)(nil)
			if err := store.InsertReservation(ctx, tx, reservationID, orderID,
				store.StateReleased, true, reasonCode, expiry); err != nil {
				return err
			}
			slog.WarnContext(ctx, "release arrived before its reserve; wrote a tombstone",
				slog.String("reservationId", reservationID),
				slog.String("orderId", orderID),
				slog.String("reference", "docs/DESIGN-INVARIANTS.md §2"))

		case err != nil:
			return err

		case existing.State == store.StateCommitted:
			// Stock is already deducted and may be picked. A release here
			// means the saga compensated past the point of no return, which
			// docs/DESIGN-INVARIANTS.md §1 forbids. Refuse loudly rather than
			// silently corrupting the count.
			return ErrReleaseAfterCommit

		case existing.State == store.StateReserved:
			if err := e.returnStock(ctx, tx, reservationID, orderID, "RELEASE"); err != nil {
				return err
			}
			if err := store.MarkReservation(ctx, tx, reservationID, store.StateReleased, reasonCode); err != nil {
				return err
			}

		default:
			// Already RELEASED or FAILED. Re-emit anyway: the previous event
			// may have been lost, and the saga is idempotent on receipt.
		}

		return events.Enqueue(ctx, tx, events.Released(orderID, reservationID, wasTombstone, correlationID))
	})

	return wasTombstone, err
}

// ---------------------------------------------------------------------------
// Commit

// Commit deducts the stock for real.
//
// This is the participant side of the saga's point of no return. Once it
// returns, no compensation is possible — Release above refuses it.
func (e *Engine) Commit(ctx context.Context, reservationID, orderID, correlationID string) error {
	return e.store.InTx(ctx, func(tx pgx.Tx) error {
		existing, err := store.LoadReservationForUpdate(ctx, tx, reservationID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrReservationNotFound
		}
		if err != nil {
			return err
		}

		if existing.State == store.StateCommitted {
			// Redelivery. Re-emit and return; committing twice would deduct
			// the stock a second time.
			return events.Enqueue(ctx, tx, events.Committed(orderID, reservationID, correlationID))
		}
		if existing.State != store.StateReserved {
			return fmt.Errorf("%w: %s is %s", ErrNotReserved, reservationID, existing.State)
		}

		lines, err := store.ReservationItems(ctx, tx, reservationID)
		if err != nil {
			return err
		}

		for _, line := range lines {
			// Deduct from on_hand AND reserved together. Decrementing only
			// on_hand leaves the units counted as reserved forever and slowly
			// starves the SKU; decrementing only reserved puts sold stock back
			// on sale. The no_oversell CHECK catches the first, not the second.
			onHand, reserved, err := store.CommitLine(ctx, tx, line.SKU, line.Quantity)
			if err != nil {
				return err
			}
			if err := store.WriteLedger(ctx, tx, store.LedgerEntry{
				SKU: line.SKU, ReservationID: reservationID, OrderID: orderID,
				Movement: "COMMIT", Quantity: -line.Quantity,
				OnHandAfter: onHand, ReservedAfter: reserved,
			}); err != nil {
				return err
			}
			if err := events.Enqueue(ctx, tx,
				events.StockChanged(line.SKU, onHand, reserved, "COMMIT", correlationID)); err != nil {
				return err
			}
		}

		if err := store.MarkReservation(ctx, tx, reservationID, store.StateCommitted, ""); err != nil {
			return err
		}
		return events.Enqueue(ctx, tx, events.Committed(orderID, reservationID, correlationID))
	})
}

// ---------------------------------------------------------------------------
// TTL sweeper

// SweepExpired releases reservations past their TTL.
//
// Defence in depth rather than the primary release mechanism — the saga
// releases explicitly, and the state-space model proves termination without
// relying on this. It exists because "the saga released it" assumes the saga is
// running, and that assumption fails during an incident, which is exactly when
// stock is most contended.
func (e *Engine) SweepExpired(ctx context.Context, batch int) (int, error) {
	var swept int

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		expired, err := store.ClaimExpired(ctx, tx, batch)
		if err != nil {
			return err
		}

		for _, r := range expired {
			if err := e.returnStock(ctx, tx, r.ID, r.OrderID, "RELEASE"); err != nil {
				return err
			}
			if err := store.MarkReservation(ctx, tx, r.ID, store.StateReleased, "TTL_EXPIRED"); err != nil {
				return err
			}
			if err := events.Enqueue(ctx, tx,
				events.Released(r.OrderID, r.ID, false, "ttl-sweeper")); err != nil {
				return err
			}

			slog.WarnContext(ctx, "reservation expired before the saga released it",
				slog.String("reservationId", r.ID),
				slog.String("orderId", r.OrderID),
				slog.String("hint", "check for a stalled order-service"))
		}

		swept = len(expired)
		return nil
	})

	return swept, err
}

// returnStock gives held units back. Shared by Release and the sweeper.
func (e *Engine) returnStock(ctx context.Context, tx pgx.Tx, reservationID, orderID, movement string) error {
	lines, err := store.ReservationItems(ctx, tx, reservationID)
	if err != nil {
		return err
	}

	for _, line := range lines {
		// store.ReturnLine clamps at zero. If a reconciliation job or a manual
		// correction has already adjusted `reserved` down, subtracting again
		// would underflow past zero and trip the CHECK — turning a bookkeeping
		// discrepancy into a failed release, which would then hold the stock
		// hostage. Clamping degrades gracefully and the ledger still records
		// exactly what happened.
		onHand, reserved, err := store.ReturnLine(ctx, tx, line.SKU, line.Quantity)
		if err != nil {
			return err
		}
		if err := store.WriteLedger(ctx, tx, store.LedgerEntry{
			SKU: line.SKU, ReservationID: reservationID, OrderID: orderID,
			Movement: movement, Quantity: -line.Quantity,
			OnHandAfter: onHand, ReservedAfter: reserved,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reads and adjustments

type Level struct {
	SKU          string `json:"sku"`
	ProductID    string `json:"productId"`
	OnHand       int    `json:"onHand"`
	Reserved     int    `json:"reserved"`
	Available    int    `json:"available"`
	ReorderPoint int    `json:"reorderPoint"`
	Status       string `json:"status"`
}

// Levels is a batch read for listing pages. One round trip for up to 100 SKUs,
// because a product grid issuing 100 sequential lookups is how a listing page
// ends up slower than checkout.
func (e *Engine) Levels(ctx context.Context, skus []string) ([]Level, error) {
	rows, err := e.store.Levels(ctx, skus)
	if err != nil {
		return nil, err
	}
	out := make([]Level, 0, len(rows))
	for _, r := range rows {
		out = append(out, Level{
			SKU: r.SKU, ProductID: r.ProductID, OnHand: r.OnHand, Reserved: r.Reserved,
			Available: max(r.OnHand-r.Reserved, 0), ReorderPoint: r.ReorderPoint, Status: r.Status,
		})
	}
	return out, nil
}

// Adjust changes physical stock: a delivery arrived, a stocktake found a
// discrepancy, a return came back. Never touches `reserved`.
func (e *Engine) Adjust(ctx context.Context, sku string, delta int, movement, actor, note, correlationID string) (Level, error) {
	var level Level

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		// A negative adjustment must not take on_hand below what is already
		// reserved: those units are promised to customers mid-checkout. The
		// no_oversell CHECK would reject it anyway; catching it here produces
		// a usable error instead of a constraint violation.
		onHand, reserved, ok, err := store.AdjustOnHand(ctx, tx, sku, delta)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s by %d", ErrAdjustmentRejected, sku, delta)
		}

		if err := store.WriteLedger(ctx, tx, store.LedgerEntry{
			SKU: sku, Movement: movement, Quantity: delta,
			OnHandAfter: onHand, ReservedAfter: reserved,
			Actor: actor, Note: note,
		}); err != nil {
			return err
		}

		level = Level{SKU: sku, OnHand: onHand, Reserved: reserved, Available: max(onHand-reserved, 0), Status: "ACTIVE"}
		return events.Enqueue(ctx, tx, events.StockChanged(sku, onHand, reserved, movement, correlationID))
	})

	return level, err
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
