// Package service is payment-service's business layer: it turns saga commands
// and provider callbacks into state transitions and outbound events.
//
// The shape follows payment-service internal/psp/paymob_test.go. In particular:
//
//   - The idempotency key is claimed with ONE atomic statement, never
//     SELECT-then-INSERT (§4b).
//   - The provider key is derived once and stored BEFORE the provider is
//     called, so a crash-and-retry presents the same key (§4).
//   - An UNKNOWN outcome is never collapsed into success or failure. It is
//     persisted as such and handed to the reconciler.
package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/souq/payment-service/internal/payment"
	"github.com/souq/payment-service/internal/psp"
)

// State mirrors the CHECK constraint on payments.state.
type State string

const (
	StatePending           State = "PENDING"
	StateAuthorizing       State = "AUTHORIZING"
	StateAuthorized        State = "AUTHORIZED"
	StateCapturing         State = "CAPTURING"
	StateCaptured          State = "CAPTURED"
	StateVoided            State = "VOIDED"
	StateFailed            State = "FAILED"
	StateRefunded          State = "REFUNDED"
	StatePartiallyRefunded State = "PARTIALLY_REFUNDED"
)

// Store is the persistence surface. An interface so the service can be tested
// without Postgres, and so the transaction boundary is explicit at every call.
type Store interface {
	InTx(ctx context.Context, fn func(Tx) error) error
	GetPaymentByOrder(ctx context.Context, orderID string) (*Payment, error)
	GetPaymentByMerchantRef(ctx context.Context, merchantRef string) (*Payment, error)
}

// Tx is the subset available inside a transaction.
type Tx interface {
	InsertPayment(ctx context.Context, p *Payment) error
	GetPaymentForUpdate(ctx context.Context, paymentID string) (*Payment, error)
	UpdatePaymentState(ctx context.Context, paymentID string, expectVersion int, next State, f StateFields) error
	RecordAttempt(ctx context.Context, a Attempt) error
	WriteLedger(ctx context.Context, entries []LedgerEntry) error
	Enqueue(ctx context.Context, e OutboxEvent) error
	ClaimEvent(ctx context.Context, eventID, consumer string) (bool, error)
}

type Payment struct {
	ID             string
	OrderID        string
	UserID         string
	State          State
	Amount         int64
	Currency       string
	CapturedAmount int64
	RefundedAmount int64
	Provider       string
	MethodToken    string
	Method         psp.PaymentMethod
	// PSPIdempotencyKey is derived once and stored before the first call.
	// It is what makes a retry deduplicate at the provider (FINDINGS §4).
	PSPIdempotencyKey string
	ProviderAuthID    string
	ProviderCaptureID string
	AuthExpiresAt     sql.NullTime
	CorrelationID     string
	Version           int
}

type StateFields struct {
	ProviderAuthID    string
	ProviderCaptureID string
	AuthCode          string
	DeclineCode       string
	ReasonCode        string
	Retriable         *bool
	CapturedAmount    *int64
	AuthExpiresAt     *time.Time
}

type Attempt struct {
	PaymentID        string
	Operation        string
	AttemptNo        int
	PSPKey           string
	Outcome          string
	ProviderRef      string
	LatencyMs        int
	RedactedResponse map[string]any
	ErrorMessage     string
}

type LedgerEntry struct {
	PaymentID   string
	OrderID     string
	Account     string
	Direction   string
	Amount      int64
	Currency    string
	EntryGroup  string
	Description string
}

type OutboxEvent struct {
	AggregateID  string
	EventID      string
	EventType    string
	Topic        string
	PartitionKey string
	Payload      []byte
	Headers      map[string]string
}

type Service struct {
	store    Store
	provider psp.Provider
	deriver  *payment.PSPKeyDeriver
	newID    func(prefix string) string
	now      func() time.Time
}

func New(store Store, provider psp.Provider, deriver *payment.PSPKeyDeriver, newID func(string) string) *Service {
	return &Service{
		store: store, provider: provider, deriver: deriver,
		newID: newID, now: func() time.Time { return time.Now().UTC() },
	}
}

const (
	consumerGroup = "payment-service.saga-commands"
	topicEvents   = "souq.payment.events.v1"
	source        = "souq/payment-service"
)

// ---------------------------------------------------------------------------
// Authorize

type AuthorizeCommand struct {
	EventID       string
	OrderID       string
	PaymentID     string
	UserID        string
	Amount        int64
	Currency      string
	Method        psp.PaymentMethod
	MethodToken   string
	WalletPhone   string
	Customer      psp.Customer
	ReturnURL     string
	CorrelationID string
	// IdempotencyKey is the client's key, carried through the saga from the
	// original checkout request. The PROVIDER key is derived from it.
	IdempotencyKey string
}

// Authorize handles souq.payment.authorize.v1.
//
// Structured as two transactions with the provider call between them, which is
// the only honest shape:
//
//	tx1  claim the key, derive and store the provider key, mark AUTHORIZING
//	     ---- the provider call, which may crash, hang, or lie ----
//	tx2  record what happened
//
// A crash between them leaves a row in AUTHORIZING with a stored provider key.
// That is recoverable: the reconciler presents the same key and the provider
// tells us the truth. A single transaction wrapping the provider call would
// hold a database transaction open across a network round trip to a third
// party, which is how connection pools die.
func (s *Service) Authorize(ctx context.Context, cmd AuthorizeCommand) error {
	pspKey, err := s.deriver.Derive(payment.OpAuthorize, cmd.OrderID, cmd.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("derive provider key: %w", err)
	}

	var pmt *Payment
	var alreadySettled bool

	// ---- tx1 ----
	err = s.store.InTx(ctx, func(tx Tx) error {
		fresh, err := tx.ClaimEvent(ctx, cmd.EventID, consumerGroup)
		if err != nil {
			return err
		}
		if !fresh {
			alreadySettled = true
			return nil
		}

		existing, err := tx.GetPaymentForUpdate(ctx, cmd.PaymentID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}

		if existing != nil {
			// A redelivery after we already acted. If it reached a terminal
			// state, re-emit the event and stop — charging again would be the
			// double-charge FINDINGS §4 is about.
			if existing.State != StatePending {
				alreadySettled = true
				pmt = existing
				return s.emitForState(ctx, tx, existing)
			}
			pmt = existing
		} else {
			pmt = &Payment{
				ID: cmd.PaymentID, OrderID: cmd.OrderID, UserID: cmd.UserID,
				State: StatePending, Amount: cmd.Amount, Currency: cmd.Currency,
				Provider: s.provider.Name(), MethodToken: cmd.MethodToken,
				Method: cmd.Method, PSPIdempotencyKey: pspKey,
				CorrelationID: cmd.CorrelationID,
			}
			if err := tx.InsertPayment(ctx, pmt); err != nil {
				return err
			}
		}

		return tx.UpdatePaymentState(ctx, pmt.ID, pmt.Version, StateAuthorizing, StateFields{})
	})
	if err != nil || alreadySettled {
		return err
	}

	// ---- the provider call ----
	started := s.now()
	result, callErr := s.provider.Authorize(ctx, psp.AuthorizeRequest{
		IdempotencyKey: pspKey,
		OrderID:        cmd.OrderID,
		PaymentID:      cmd.PaymentID,
		UserID:         cmd.UserID,
		Amount:         psp.Money{Amount: cmd.Amount, Currency: cmd.Currency},
		Method:         cmd.Method,
		Token:          cmd.MethodToken,
		WalletPhone:    cmd.WalletPhone,
		Customer:       cmd.Customer,
		ReturnURL:      cmd.ReturnURL,
	})
	latency := int(time.Since(started).Milliseconds())

	// ---- tx2 ----
	return s.store.InTx(ctx, func(tx Tx) error {
		cur, err := tx.GetPaymentForUpdate(ctx, cmd.PaymentID)
		if err != nil {
			return err
		}

		attempt := Attempt{
			PaymentID: cmd.PaymentID, Operation: "AUTHORIZE", AttemptNo: 1,
			PSPKey: pspKey, ProviderRef: result.ProviderRef, LatencyMs: latency,
			RedactedResponse: result.RawResponse,
		}

		switch result.Outcome {
		case psp.OutcomeApproved:
			attempt.Outcome = "SUCCESS"
			if err := tx.RecordAttempt(ctx, attempt); err != nil {
				return err
			}
			var expires *time.Time
			if !result.ExpiresAt.IsZero() {
				expires = &result.ExpiresAt
			}
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateAuthorized, StateFields{
				ProviderAuthID: result.ProviderRef,
				AuthCode:       result.AuthCode,
				AuthExpiresAt:  expires,
			}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.authorized.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID,
				"amount":   money(cur.Amount, cur.Currency),
				"provider": s.provider.Name(), "authCode": result.AuthCode,
				"expiresAt": rfc3339(result.ExpiresAt),
			})

		case psp.OutcomePending:
			// 3-D Secure or a wallet approval. No money has moved. Stay in
			// AUTHORIZING and wait for the callback; the saga's own timeout
			// covers the customer walking away.
			attempt.Outcome = "SUCCESS"
			if err := tx.RecordAttempt(ctx, attempt); err != nil {
				return err
			}
			slog.InfoContext(ctx, "payment awaiting customer action",
				slog.String("paymentId", cur.ID),
				slog.String("redirectUrl", redact(result.RedirectURL)))
			return nil

		case psp.OutcomeUnknown:
			// The dangerous one. Do NOT emit a failure event: the saga would
			// compensate and void a charge that may have succeeded, or worse,
			// cancel an order the customer has paid for.
			attempt.Outcome = "UNKNOWN"
			attempt.ErrorMessage = errString(callErr)
			if err := tx.RecordAttempt(ctx, attempt); err != nil {
				return err
			}
			slog.ErrorContext(ctx, "PAYMENT OUTCOME UNKNOWN — reconciliation required",
				slog.String("paymentId", cur.ID),
				slog.String("orderId", cur.OrderID),
				slog.String("pspKey", pspKey),
				slog.String("runbook", "docs/runbooks/unknown-payment-outcome.md"))
			// Leave the row in AUTHORIZING. The reconciler picks it up.
			return nil

		default: // declined
			attempt.Outcome = "DECLINED"
			attempt.ErrorMessage = string(result.ReasonCode)
			if err := tx.RecordAttempt(ctx, attempt); err != nil {
				return err
			}
			retriable := result.ReasonCode.Retriable()
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateFailed, StateFields{
				DeclineCode: result.DeclineCode,
				ReasonCode:  string(result.ReasonCode),
				Retriable:   &retriable,
			}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.failed.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID,
				"reasonCode":  string(result.ReasonCode),
				"declineCode": result.DeclineCode,
				// The saga backs off on a retriable failure and compensates on
				// a hard one. Getting this flag wrong either hammers a
				// struggling provider or cancels a recoverable order.
				"retriable": retriable,
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Capture

// Capture handles souq.payment.capture.v1.
//
// This runs AFTER inventory has committed. docs/DESIGN-INVARIANTS.md §1 makes that
// ordering load-bearing: capture is irreversible, so it only ever happens
// against stock we already own.
func (s *Service) Capture(ctx context.Context, eventID, orderID, paymentID, idempotencyKey string) error {
	pspKey, err := s.deriver.Derive(payment.OpCapture, orderID, idempotencyKey)
	if err != nil {
		return err
	}

	var pmt *Payment
	var skip bool

	err = s.store.InTx(ctx, func(tx Tx) error {
		fresh, err := tx.ClaimEvent(ctx, eventID, consumerGroup)
		if err != nil || !fresh {
			skip = true
			return err
		}
		pmt, err = tx.GetPaymentForUpdate(ctx, paymentID)
		if err != nil {
			return err
		}
		if pmt.State == StateCaptured {
			// Redelivery. Re-emit so the saga can finish, but do not capture
			// again — that is a second charge.
			skip = true
			return s.emit(ctx, tx, pmt, "souq.payment.captured.v1", map[string]any{
				"orderId": pmt.OrderID, "paymentId": pmt.ID,
				"amount": money(pmt.Amount, pmt.Currency),
			})
		}
		if pmt.State != StateAuthorized {
			return fmt.Errorf("cannot capture payment %s from state %s", paymentID, pmt.State)
		}
		return tx.UpdatePaymentState(ctx, pmt.ID, pmt.Version, StateCapturing, StateFields{})
	})
	if err != nil || skip {
		return err
	}

	// Some rails have no separate capture: mobile wallets and COD move the
	// money in one step. Satisfy the saga locally rather than calling out.
	if !s.provider.SupportsCapture(pmt.Method) {
		return s.settleCapture(ctx, pmt, pspKey, psp.Result{
			Outcome: psp.OutcomeApproved, ProviderRef: pmt.ProviderAuthID,
		}, 0, nil)
	}

	started := s.now()
	res, callErr := s.provider.Capture(ctx, psp.CaptureRequest{
		IdempotencyKey: pspKey, OrderID: orderID, PaymentID: paymentID,
		ProviderRef: pmt.ProviderAuthID,
		Amount:      psp.Money{Amount: pmt.Amount, Currency: pmt.Currency},
	})
	return s.settleCapture(ctx, pmt, pspKey, res, int(time.Since(started).Milliseconds()), callErr)
}

func (s *Service) settleCapture(ctx context.Context, pmt *Payment, pspKey string, res psp.Result, latency int, callErr error) error {
	return s.store.InTx(ctx, func(tx Tx) error {
		cur, err := tx.GetPaymentForUpdate(ctx, pmt.ID)
		if err != nil {
			return err
		}

		attempt := Attempt{
			PaymentID: cur.ID, Operation: "CAPTURE", AttemptNo: 1, PSPKey: pspKey,
			ProviderRef: res.ProviderRef, LatencyMs: latency,
			RedactedResponse: res.RawResponse, ErrorMessage: errString(callErr),
		}

		if res.Outcome != psp.OutcomeApproved {
			attempt.Outcome = map[psp.Outcome]string{
				psp.OutcomeUnknown: "UNKNOWN", psp.OutcomeDeclined: "DECLINED",
				psp.OutcomePending: "SUCCESS",
			}[res.Outcome]
			if err := tx.RecordAttempt(ctx, attempt); err != nil {
				return err
			}
			// Stay in CAPTURING. Stock is already committed, so the only
			// correct move is to keep trying — the saga is past its point of
			// no return and rolling back would deduct stock for free.
			slog.ErrorContext(ctx, "capture did not succeed; the saga will retry",
				slog.String("paymentId", cur.ID), slog.String("outcome", string(res.Outcome)))
			return nil
		}

		attempt.Outcome = "SUCCESS"
		if err := tx.RecordAttempt(ctx, attempt); err != nil {
			return err
		}

		captured := cur.Amount
		if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateCaptured, StateFields{
			ProviderCaptureID: res.ProviderRef,
			CapturedAmount:    &captured,
		}); err != nil {
			return err
		}

		// Double-entry. Finance reconciles against the ledger, not against the
		// payments table, so every movement of money writes a balanced pair.
		group := s.newID("grp")
		if err := tx.WriteLedger(ctx, []LedgerEntry{
			{PaymentID: cur.ID, OrderID: cur.OrderID, Account: "psp_clearing",
				Direction: "DEBIT", Amount: captured, Currency: cur.Currency,
				EntryGroup: group, Description: "capture"},
			{PaymentID: cur.ID, OrderID: cur.OrderID, Account: "revenue",
				Direction: "CREDIT", Amount: captured, Currency: cur.Currency,
				EntryGroup: group, Description: "capture"},
		}); err != nil {
			return err
		}

		return s.emit(ctx, tx, cur, "souq.payment.captured.v1", map[string]any{
			"orderId": cur.OrderID, "paymentId": cur.ID,
			"amount": money(captured, cur.Currency), "capturedAt": rfc3339(s.now()),
		})
	})
}

// ---------------------------------------------------------------------------
// Void

// Void handles souq.payment.void.v1, including the tombstone case.
//
// A Void may arrive for a payment this service has never seen: the saga timed
// out and compensated while the Authorize command was still in the consumer's
// buffer. Ignoring it would let the late Authorize create a charge nobody will
// ever reverse. So we write a VOIDED row anyway, and the late Authorize finds
// it and declines. docs/DESIGN-INVARIANTS.md §2.
func (s *Service) Void(ctx context.Context, eventID, orderID, paymentID, reason, idempotencyKey string) error {
	pspKey, err := s.deriver.Derive(payment.OpVoid, orderID, idempotencyKey)
	if err != nil {
		return err
	}

	var pmt *Payment
	var wasTombstone, skip bool

	err = s.store.InTx(ctx, func(tx Tx) error {
		fresh, err := tx.ClaimEvent(ctx, eventID, consumerGroup)
		if err != nil || !fresh {
			skip = true
			return err
		}

		pmt, err = tx.GetPaymentForUpdate(ctx, paymentID)
		if errors.Is(err, ErrNotFound) {
			// THE TOMBSTONE.
			wasTombstone = true
			pmt = &Payment{
				ID: paymentID, OrderID: orderID, State: StateVoided,
				Amount: 1, Currency: "EGP", Provider: s.provider.Name(),
				PSPIdempotencyKey: pspKey, MethodToken: "tombstone",
			}
			if err := tx.InsertPayment(ctx, pmt); err != nil {
				return err
			}
			slog.WarnContext(ctx, "void arrived before authorize; wrote a tombstone",
				slog.String("paymentId", paymentID), slog.String("orderId", orderID),
				slog.String("reference", "docs/DESIGN-INVARIANTS.md §2"))
			return s.emit(ctx, tx, pmt, "souq.payment.voided.v1", map[string]any{
				"orderId": orderID, "paymentId": paymentID, "wasTombstone": true,
			})
		}
		if err != nil {
			return err
		}

		switch pmt.State {
		case StateVoided, StateFailed:
			// Already settled. Re-emit; the saga is idempotent on receipt.
			skip = true
			return s.emit(ctx, tx, pmt, "souq.payment.voided.v1", map[string]any{
				"orderId": orderID, "paymentId": paymentID, "wasTombstone": false,
			})
		case StateCaptured:
			// Compensating past the point of no return. Refuse loudly: the
			// correct remedy is a refund through support, not a void.
			return fmt.Errorf("payment %s is captured and cannot be voided; issue a refund instead", paymentID)
		}
		return nil
	})
	if err != nil || skip || wasTombstone {
		return err
	}

	started := s.now()
	res, callErr := s.provider.Void(ctx, psp.VoidRequest{
		IdempotencyKey: pspKey, OrderID: orderID, PaymentID: paymentID,
		ProviderRef: pmt.ProviderAuthID, Reason: reason,
	})
	latency := int(time.Since(started).Milliseconds())

	return s.store.InTx(ctx, func(tx Tx) error {
		cur, err := tx.GetPaymentForUpdate(ctx, paymentID)
		if err != nil {
			return err
		}
		if err := tx.RecordAttempt(ctx, Attempt{
			PaymentID: cur.ID, Operation: "VOID", AttemptNo: 1, PSPKey: pspKey,
			Outcome: outcomeLabel(res.Outcome), ProviderRef: res.ProviderRef,
			LatencyMs: latency, ErrorMessage: errString(callErr),
		}); err != nil {
			return err
		}
		if res.Outcome != psp.OutcomeApproved {
			// Do not mark voided. The saga retries; the reconciler escalates.
			return nil
		}
		if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateVoided, StateFields{}); err != nil {
			return err
		}
		return s.emit(ctx, tx, cur, "souq.payment.voided.v1", map[string]any{
			"orderId": orderID, "paymentId": paymentID, "wasTombstone": false,
		})
	})
}

// ---------------------------------------------------------------------------
// Provider callbacks

// HandleCallback applies a verified provider notification.
//
// This is how card and wallet payments actually complete: Authorize returned
// PENDING, the customer did something in a browser or on their phone, and the
// provider is now telling us the outcome. The signature has ALREADY been
// verified by the adapter — this function trusts its input and must only ever
// be called with a Callback whose Verified is true.
func (s *Service) HandleCallback(ctx context.Context, cb psp.Callback) error {
	if !cb.Verified {
		// Defence in depth. If this ever fires, a caller bypassed
		// ParseCallback and the webhook is effectively unauthenticated.
		return errors.New("refusing to apply an unverified callback")
	}

	// cb.OrderID carries the merchant reference, which for Paymob is our
	// deterministic provider key. Map it back to the payment row.
	pmt, err := s.store.GetPaymentByMerchantRef(ctx, cb.OrderID)
	if err != nil {
		return fmt.Errorf("no payment found for merchant reference %q: %w", cb.OrderID, err)
	}

	return s.store.InTx(ctx, func(tx Tx) error {
		// Dedup on the provider's transaction id. Providers retry callbacks
		// aggressively — Paymob will resend for hours until it gets a 200.
		fresh, err := tx.ClaimEvent(ctx, "cb:"+cb.ProviderRef+":"+string(cb.Kind), consumerGroup)
		if err != nil || !fresh {
			return err
		}

		cur, err := tx.GetPaymentForUpdate(ctx, pmt.ID)
		if err != nil {
			return err
		}

		switch {
		case cb.Kind == psp.CallbackAuthorized && cb.Success:
			if cur.State == StateAuthorized || cur.State == StateCaptured {
				return nil // already known
			}
			// A callback must never contradict the amount we asked for. If it
			// does, something is wrong at the provider or the callback is not
			// for this payment — either way, do not accept it.
			if cb.Amount.Amount != cur.Amount {
				return fmt.Errorf("callback amount %d does not match payment %s amount %d",
					cb.Amount.Amount, cur.ID, cur.Amount)
			}
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateAuthorized, StateFields{
				ProviderAuthID: cb.ProviderRef,
			}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.authorized.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID,
				"amount": money(cur.Amount, cur.Currency), "provider": s.provider.Name(),
			})

		case cb.Kind == psp.CallbackCaptured:
			captured := cb.Amount.Amount
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateCaptured, StateFields{
				ProviderCaptureID: cb.ProviderRef, CapturedAmount: &captured,
			}); err != nil {
				return err
			}
			group := s.newID("grp")
			if err := tx.WriteLedger(ctx, []LedgerEntry{
				{PaymentID: cur.ID, OrderID: cur.OrderID, Account: "psp_clearing",
					Direction: "DEBIT", Amount: captured, Currency: cur.Currency, EntryGroup: group},
				{PaymentID: cur.ID, OrderID: cur.OrderID, Account: "revenue",
					Direction: "CREDIT", Amount: captured, Currency: cur.Currency, EntryGroup: group},
			}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.captured.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID, "amount": money(captured, cur.Currency),
			})

		case cb.Kind == psp.CallbackFailed:
			if cur.State == StateCaptured || cur.State == StateAuthorized {
				// A failure callback for something we know succeeded. Log and
				// ignore rather than reversing state on a late message.
				slog.WarnContext(ctx, "ignoring a failure callback for an already-successful payment",
					slog.String("paymentId", cur.ID), slog.String("state", string(cur.State)))
				return nil
			}
			retriable := cb.ReasonCode.Retriable()
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateFailed, StateFields{
				DeclineCode: cb.DeclineCode, ReasonCode: string(cb.ReasonCode), Retriable: &retriable,
			}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.failed.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID,
				"reasonCode": string(cb.ReasonCode), "declineCode": cb.DeclineCode,
				"retriable": retriable,
			})

		case cb.Kind == psp.CallbackVoided:
			if err := tx.UpdatePaymentState(ctx, cur.ID, cur.Version, StateVoided, StateFields{}); err != nil {
				return err
			}
			return s.emit(ctx, tx, cur, "souq.payment.voided.v1", map[string]any{
				"orderId": cur.OrderID, "paymentId": cur.ID, "wasTombstone": false,
			})
		}
		return nil
	})
}

// ---------------------------------------------------------------------------

func (s *Service) emit(ctx context.Context, tx Tx, p *Payment, eventType string, data map[string]any) error {
	eventID := s.newID("evt")
	envelope := map[string]any{
		"specversion": "1.0", "id": eventID, "source": source,
		"type": eventType, "subject": p.OrderID,
		"time": rfc3339(s.now()), "datacontenttype": "application/json",
		"correlationid": p.CorrelationID, "data": data,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return tx.Enqueue(ctx, OutboxEvent{
		AggregateID: p.ID, EventID: eventID, EventType: eventType,
		Topic: topicEvents, PartitionKey: p.OrderID, Payload: payload,
		Headers: map[string]string{
			"ce_id": eventID, "ce_type": eventType, "ce_source": source,
			"correlationid": p.CorrelationID,
		},
	})
}

// emitForState re-publishes the event matching a payment's current state, so a
// redelivered command still unblocks a saga whose original event was lost.
func (s *Service) emitForState(ctx context.Context, tx Tx, p *Payment) error {
	switch p.State {
	case StateAuthorized:
		return s.emit(ctx, tx, p, "souq.payment.authorized.v1", map[string]any{
			"orderId": p.OrderID, "paymentId": p.ID,
			"amount": money(p.Amount, p.Currency), "provider": p.Provider,
		})
	case StateCaptured:
		return s.emit(ctx, tx, p, "souq.payment.captured.v1", map[string]any{
			"orderId": p.OrderID, "paymentId": p.ID, "amount": money(p.CapturedAmount, p.Currency),
		})
	case StateFailed:
		return s.emit(ctx, tx, p, "souq.payment.failed.v1", map[string]any{
			"orderId": p.OrderID, "paymentId": p.ID,
			"reasonCode": "CARD_DECLINED", "retriable": false,
		})
	case StateVoided:
		return s.emit(ctx, tx, p, "souq.payment.voided.v1", map[string]any{
			"orderId": p.OrderID, "paymentId": p.ID, "wasTombstone": false,
		})
	}
	return nil
}

var (
	ErrNotFound = errors.New("payment not found")

	// ErrDuplicate: a second payment for one order, or two payments sharing a
	// provider idempotency key. Both constraints exist to stop a double charge,
	// so both are a domain outcome rather than a 500.
	ErrDuplicate = errors.New("payment already exists for this order")

	// ErrVersionStale: another handler moved this payment between our read and
	// our write. The caller re-reads; the retry usually finds the work done.
	ErrVersionStale = errors.New("payment changed concurrently")
)

func money(amount int64, currency string) map[string]any {
	return map[string]any{"amount": amount, "currency": currency}
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func outcomeLabel(o psp.Outcome) string {
	switch o {
	case psp.OutcomeApproved:
		return "SUCCESS"
	case psp.OutcomeDeclined:
		return "DECLINED"
	case psp.OutcomeUnknown:
		return "UNKNOWN"
	default:
		return "SUCCESS"
	}
}

// redact strips a payment token from a URL before it reaches a log line.
func redact(u string) string {
	if i := indexOf(u, "payment_token="); i >= 0 {
		return u[:i] + "payment_token=REDACTED"
	}
	return u
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
