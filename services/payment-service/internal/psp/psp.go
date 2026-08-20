// Package psp is the payment provider boundary.
//
// One interface, several implementations (Paymob, a mock for local work, and
// room for Stripe/Adyen later). Everything above this package deals in
// SOUQ's own vocabulary; every provider quirk is absorbed here.
//
// The interface is shaped by payment-service internal/psp/paymob_test.go, not by any
// provider's SDK. In particular every method takes an `IdempotencyKey` that
// the caller derived deterministically (internal/payment/psp_key.go) — the
// adapter's job is to translate that into whatever mechanism its provider
// actually offers, and to be honest in its documentation about how strong
// that mechanism is.
package psp

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Money mirrors docs/CONTRACTS.md §2.5. Minor units only.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// Customer is the minimum a provider needs to run a risk check. Anything
// beyond this is not sent — a provider does not need a full order history to
// authorise a card.
type Customer struct {
	FirstName  string
	LastName   string
	Email      string
	Phone      string
	Street     string
	City       string
	State      string
	Country    string // ISO-3166-1 alpha-2
	PostalCode string
}

// AuthorizeRequest asks the provider to ring-fence funds.
type AuthorizeRequest struct {
	// IdempotencyKey is derived, never random. Two attempts at the same
	// logical payment MUST present the same value; see FINDINGS §4.
	IdempotencyKey string

	OrderID   string
	PaymentID string
	UserID    string
	Amount    Money

	// Method selects the rail. Paymob's card and wallet flows are genuinely
	// different APIs, not a parameter.
	Method PaymentMethod

	// Token identifies a saved card or a wallet. Its meaning is
	// provider-specific and it is opaque above this package. A raw PAN never
	// appears here — the browser posts card details straight to the provider.
	Token string

	// WalletPhone is required for MethodWallet. Egyptian mobile wallets
	// (Vodafone Cash, Orange Money, Etisalat Cash) are identified by number.
	WalletPhone string

	Customer Customer

	// ReturnURL is where the provider sends the customer after 3-D Secure or
	// a wallet approval.
	ReturnURL string
}

type PaymentMethod string

const (
	MethodCard   PaymentMethod = "CARD"
	MethodWallet PaymentMethod = "WALLET"
	// Cash on delivery, still a very large share of Egyptian e-commerce.
	// Authorised instantly, captured when the courier hands over the parcel.
	MethodCashOnDelivery PaymentMethod = "COD"
	MethodInstallment    PaymentMethod = "INSTALLMENT"
)

// AuthorizeResult is what the saga needs to decide its next move.
type AuthorizeResult struct {
	// Outcome drives the saga. See the comment on each value.
	Outcome Outcome

	// ProviderRef is the provider's own id for the authorisation. Required for
	// capture, void and refund, and the only thing a support agent can quote
	// to the provider.
	ProviderRef string

	// OrderRef is the provider-side order id, where the provider models one.
	OrderRef string

	AuthCode string

	// ExpiresAt is when the authorisation lapses. Capturing after this fails
	// and needs a fresh authorisation, so the reconciler watches it rather
	// than discovering it at capture time.
	ExpiresAt time.Time

	// RedirectURL is set when the customer must complete something in a
	// browser — 3-D Secure, or approving a wallet push. The saga treats this
	// as pending, not success: no money has moved yet.
	RedirectURL string

	// DeclineCode is the provider's raw code. Kept for support and for
	// tuning the retriable/not-retriable split; never shown to a customer.
	DeclineCode string
	ReasonCode  ReasonCode

	// RawResponse has card data and PII already stripped by the adapter.
	// Never store a provider payload verbatim: it carries the last four, the
	// cardholder name, and a full billing address.
	RawResponse map[string]any
}

// Outcome is deliberately four values, not a boolean.
//
// The one that matters is OutcomeUnknown. A timeout talking to a provider is
// NOT a failure — the charge may have succeeded and only the response was
// lost. Treating it as a decline is how a customer gets charged for an order
// that was cancelled. The saga must reconcile instead.
type Outcome string

const (
	OutcomeApproved Outcome = "APPROVED"
	OutcomeDeclined Outcome = "DECLINED"
	OutcomePending  Outcome = "PENDING"
	OutcomeUnknown  Outcome = "UNKNOWN"
)

type ReasonCode string

const (
	ReasonInsufficientFunds    ReasonCode = "INSUFFICIENT_FUNDS"
	ReasonCardDeclined         ReasonCode = "CARD_DECLINED"
	ReasonCardExpired          ReasonCode = "CARD_EXPIRED"
	ReasonInvalidCVC           ReasonCode = "INVALID_CVC"
	ReasonFraudSuspected       ReasonCode = "FRAUD_SUSPECTED"
	ReasonThreeDSFailed        ReasonCode = "THREE_DS_FAILED"
	ReasonProviderUnavailable  ReasonCode = "PROVIDER_UNAVAILABLE"
	ReasonAuthorizationExpired ReasonCode = "AUTHORIZATION_EXPIRED"
)

// Retriable says whether the saga should back off and try again, or
// compensate now.
//
// Getting this wrong is costly in both directions: treating a hard decline as
// retriable hammers a provider with a card that will never work, and treating
// an outage as a hard decline cancels orders the customer could have paid for.
func (r ReasonCode) Retriable() bool {
	return r == ReasonProviderUnavailable
}

type CaptureRequest struct {
	IdempotencyKey string
	OrderID        string
	PaymentID      string
	ProviderRef    string
	Amount         Money
}

type VoidRequest struct {
	IdempotencyKey string
	OrderID        string
	PaymentID      string
	ProviderRef    string
	Reason         string
}

type RefundRequest struct {
	IdempotencyKey string
	OrderID        string
	PaymentID      string
	RefundID       string
	ProviderRef    string
	Amount         Money
	Reason         string
}

type Result struct {
	Outcome     Outcome
	ProviderRef string
	ReasonCode  ReasonCode
	DeclineCode string
	RawResponse map[string]any
}

// Callback is a provider-initiated notification, already verified and
// normalised. Providers send these when a payment completes out of band —
// after 3-D Secure, after a wallet approval, or when a refund settles.
type Callback struct {
	// Verified is true only when the signature checked out. An adapter must
	// never return an unverified callback with Verified left false and hope
	// the caller notices; ParseCallback returns an error instead.
	Verified bool

	Kind        CallbackKind
	ProviderRef string
	// OrderID is OUR order id, recovered from the merchant reference we sent.
	OrderID     string
	PaymentID   string
	Amount      Money
	Success     bool
	ReasonCode  ReasonCode
	DeclineCode string
	OccurredAt  time.Time
	RawResponse map[string]any
}

type CallbackKind string

const (
	CallbackAuthorized CallbackKind = "AUTHORIZED"
	CallbackCaptured   CallbackKind = "CAPTURED"
	CallbackFailed     CallbackKind = "FAILED"
	CallbackVoided     CallbackKind = "VOIDED"
	CallbackRefunded   CallbackKind = "REFUNDED"
)

// Provider is what payment-service depends on. Nothing above this package
// imports an HTTP client or knows a provider's URL.
type Provider interface {
	// Name is used in events, metrics and the `provider` column.
	Name() string

	// Authorize ring-fences funds. Implementations MUST be idempotent with
	// respect to req.IdempotencyKey.
	Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResult, error)

	// Capture takes the ring-fenced funds. Irreversible except by refund.
	Capture(ctx context.Context, req CaptureRequest) (Result, error)

	// Void releases an authorisation that was never captured.
	Void(ctx context.Context, req VoidRequest) (Result, error)

	// Refund returns captured funds. Partial refunds are supported by amount.
	Refund(ctx context.Context, req RefundRequest) (Result, error)

	// ParseCallback verifies a provider-initiated notification and normalises
	// it. It MUST return an error rather than an unverified Callback when the
	// signature does not check out — a webhook endpoint that trusts its input
	// is a way to mark any order as paid.
	ParseCallback(ctx context.Context, body []byte, headers map[string]string, query map[string]string) (Callback, error)

	// SupportsCapture reports whether the provider separates authorisation
	// from capture. Some rails (most Egyptian mobile wallets) do not: the
	// money moves immediately and there is nothing to capture later. The saga
	// still runs its CAPTURE step for uniformity; the adapter turns it into a
	// no-op and says so here.
	SupportsCapture(method PaymentMethod) bool

	// Health is used by the readiness probe.
	Health(ctx context.Context) error
}

// Errors that the service layer switches on.
var (
	// ErrProviderUnavailable: transport failed, or the provider returned 5xx.
	// Safe to retry.
	ErrProviderUnavailable = errors.New("psp: provider unavailable")

	// ErrOutcomeUnknown: we asked, and we do not know what happened. NOT safe
	// to treat as either success or failure — the reconciler must ask the
	// provider what the truth is.
	ErrOutcomeUnknown = errors.New("psp: outcome unknown, reconciliation required")

	// ErrAlreadyProcessed: the provider recognised this idempotency key and
	// replayed its own result. This is a SUCCESS path, not an error, and the
	// adapter should normally return the replayed result rather than this.
	ErrAlreadyProcessed = errors.New("psp: already processed")

	ErrInvalidSignature = errors.New("psp: callback signature verification failed")
	ErrNotSupported     = errors.New("psp: operation not supported by this provider")
)

// UnavailableError carries the underlying cause for logging without letting it
// escape to a customer.
type UnavailableError struct {
	Provider string
	Op       string
	Cause    error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("psp: %s %s failed: %v", e.Provider, e.Op, e.Cause)
}
func (e *UnavailableError) Unwrap() error { return ErrProviderUnavailable }
