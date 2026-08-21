package psp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Paymob adapter.
//
// Paymob (accept.paymob.com) is the dominant gateway in Egypt and across much
// of MENA. It supports cards, the Egyptian mobile wallets (Vodafone Cash,
// Orange Money, Etisalat Cash, we-pay), instalments through valU and bank
// programmes, and cash on delivery through Aman/Masary — which still accounts
// for a large share of Egyptian e-commerce and is the reason MethodCOD exists.
//
// ─────────────────────────────────────────────────────────────────────────────
// THE THING THAT MATTERS: Paymob has no Idempotency-Key header.
// ─────────────────────────────────────────────────────────────────────────────
//
// Stripe and Adyen both accept an `Idempotency-Key` header and replay their
// stored response for a repeat. Paymob does not. That collides head-on with
// docs/DESIGN-INVARIANTS.md §4, which showed that our own idempotency table is NOT
// sufficient on its own: the crash-then-reap path legitimately produces a
// second attempt, and only provider-side deduplication stops that becoming a
// second charge.
//
// So we use the one uniqueness constraint Paymob does enforce:
// `merchant_order_id` on the order registration call. Paymob rejects a
// duplicate with "duplicate" in the message, and that rejection IS the
// deduplication signal. Concretely:
//
//	merchant_order_id = <deterministic key from internal/payment/psp_key.go>
//
//	1. register the order with that id
//	2. if Paymob says duplicate -> this payment was already started. Fetch the
//	   existing order by merchant_order_id, read its transactions, and return
//	   the real outcome instead of charging again.
//
// Step 2 is not optional and it is not an edge case. It is the entire safety
// property, and it is why `lookupByMerchantOrderID` exists below.
//
// Two consequences worth stating plainly:
//
//   - The deterministic key must be stable across processes and time. It is
//     (see psp_key.go), and psp_key_test.go pins it.
//   - This is weaker than a real idempotency header, because the window
//     between "order registered" and "transaction created" is not covered by
//     it. The reconciler (internal/reconcile) closes that window by querying
//     Paymob for any payment left in AUTHORIZING or CAPTURING.
//
// ─────────────────────────────────────────────────────────────────────────────
// Flow
// ─────────────────────────────────────────────────────────────────────────────
//
// The classic three-step Accept flow, which is what the overwhelming majority
// of live Paymob integrations use:
//
//	POST /api/auth/tokens                -> auth token (valid 1 hour)
//	POST /api/ecommerce/orders           -> Paymob order id
//	POST /api/acceptance/payment_keys    -> payment key (a JWT)
//	then, by rail:
//	  card    -> redirect the customer to the iframe with the payment key
//	  wallet  -> POST /api/acceptance/payments/pay -> redirect URL for approval
//
// Paymob also ships a newer unified "Intention" API (POST /v1/intention/).
// It is cleaner, but the classic flow is what most accounts are provisioned
// for and what the integration ids below refer to, so that is what this
// implements.
//
// ─────────────────────────────────────────────────────────────────────────────
// NOTE ON ACCURACY
// ─────────────────────────────────────────────────────────────────────────────
// Endpoint paths, field names and the HMAC field ORDER below follow Paymob's
// published Accept documentation. Paymob has changed field ordering in the
// HMAC before. Verify hmacFieldOrder against your account's current docs and
// run TestHMACAgainstRealCallback with a captured live callback before going
// to production — a wrong order silently rejects every callback, and the
// symptom is orders stuck in AUTHORIZING rather than an obvious error.

const (
	paymobDefaultBaseURL = "https://accept.paymob.com/api"
	// Auth tokens are valid for one hour. Refreshed at 50 minutes so a
	// long-running request never straddles the expiry.
	paymobTokenLifetime = 50 * time.Minute
	// Payment keys expire; 15 minutes is long enough to complete 3-D Secure
	// and short enough that an abandoned checkout cannot be resumed hours
	// later against stock that has since been released.
	paymobPaymentKeyTTL = 900
)

type PaymobConfig struct {
	BaseURL string

	// APIKey is the long-lived account key used only to mint auth tokens.
	APIKey string

	// HMACSecret verifies callbacks. Without it, anyone who can reach the
	// webhook endpoint can mark any order as paid.
	HMACSecret string

	// IntegrationIDs maps a rail to the Paymob integration configured for it.
	// A Paymob account has a separate integration id per payment method, and
	// sending a wallet payment to the card integration fails in a way that
	// reads like a configuration error rather than a routing mistake.
	IntegrationIDs map[PaymentMethod]int

	// IframeID for the hosted card form.
	IframeID int

	// Currency Paymob is configured for. Paymob accounts are single-currency;
	// a mismatch is rejected at order registration.
	Currency string

	// AuthorizeOnly requests auth-and-capture-later. Requires the integration
	// to be provisioned for it — many Egyptian card integrations are
	// auth-capture in one step, in which case Capture is a no-op and
	// SupportsCapture returns false.
	AuthorizeOnly bool

	HTTPTimeout time.Duration
}

type Paymob struct {
	cfg    PaymobConfig
	client *http.Client

	// Auth token cache. Minting one costs a round trip and Paymob rate-limits
	// the auth endpoint, so a fresh token per request would be both slow and
	// eventually throttled.
	mu         sync.RWMutex
	token      string
	tokenSetAt time.Time
	// Collapses a stampede of concurrent refreshes into one call.
	refreshing sync.Mutex
}

func NewPaymob(cfg PaymobConfig) (*Paymob, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("paymob: API key is required")
	}
	if cfg.HMACSecret == "" {
		// Refusing to start is correct. A service that runs without the HMAC
		// secret has an unauthenticated webhook that marks orders paid.
		return nil, fmt.Errorf("paymob: HMAC secret is required; without it callbacks cannot be verified")
	}
	if len(cfg.IntegrationIDs) == 0 {
		return nil, fmt.Errorf("paymob: at least one integration id is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = paymobDefaultBaseURL
	}
	if cfg.Currency == "" {
		cfg.Currency = "EGP"
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 15 * time.Second
	}

	return &Paymob{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.HTTPTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

func (p *Paymob) Name() string { return "paymob" }

// SupportsCapture reports whether the rail separates authorisation from
// capture.
//
// Egyptian mobile wallets do not: the money leaves the customer's wallet the
// moment they approve, and there is nothing held to capture afterwards. Cash
// on delivery is the mirror image — nothing moves until the courier collects.
// The saga runs its CAPTURE step regardless, for uniformity; this tells the
// service layer to satisfy it locally rather than calling Paymob.
func (p *Paymob) SupportsCapture(method PaymentMethod) bool {
	switch method {
	case MethodCard:
		return p.cfg.AuthorizeOnly
	case MethodWallet, MethodCashOnDelivery, MethodInstallment:
		return false
	}
	return false
}

// ---------------------------------------------------------------------------
// Authentication

func (p *Paymob) authToken(ctx context.Context) (string, error) {
	p.mu.RLock()
	tok, age := p.token, time.Since(p.tokenSetAt)
	p.mu.RUnlock()

	if tok != "" && age < paymobTokenLifetime {
		return tok, nil
	}

	p.refreshing.Lock()
	defer p.refreshing.Unlock()

	// Re-check: another goroutine may have refreshed while we waited.
	p.mu.RLock()
	tok, age = p.token, time.Since(p.tokenSetAt)
	p.mu.RUnlock()
	if tok != "" && age < paymobTokenLifetime {
		return tok, nil
	}

	var resp struct {
		Token string `json:"token"`
	}
	// Deliberately not using p.do(): that helper needs a token, and this is
	// the call that produces one.
	if err := p.post(ctx, "/auth/tokens", map[string]any{"api_key": p.cfg.APIKey}, &resp); err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", &UnavailableError{Provider: "paymob", Op: "auth", Cause: fmt.Errorf("empty token in response")}
	}

	p.mu.Lock()
	p.token, p.tokenSetAt = resp.Token, time.Now()
	p.mu.Unlock()

	slog.DebugContext(ctx, "paymob auth token refreshed")
	return resp.Token, nil
}

// ---------------------------------------------------------------------------
// Authorize

func (p *Paymob) Authorize(ctx context.Context, req AuthorizeRequest) (AuthorizeResult, error) {
	if req.Amount.Currency != p.cfg.Currency {
		// Paymob accounts are single-currency and reject a mismatch at order
		// registration with an opaque message. Fail here with a useful one.
		return AuthorizeResult{Outcome: OutcomeDeclined, ReasonCode: ReasonCardDeclined},
			fmt.Errorf("paymob: account is configured for %s, order is %s",
				p.cfg.Currency, req.Amount.Currency)
	}

	integrationID, ok := p.cfg.IntegrationIDs[req.Method]
	if !ok {
		return AuthorizeResult{}, fmt.Errorf("%w: no Paymob integration configured for %s",
			ErrNotSupported, req.Method)
	}

	token, err := p.authToken(ctx)
	if err != nil {
		return AuthorizeResult{Outcome: OutcomeUnknown}, err
	}

	// Step 1: register the order. merchant_order_id carries our deterministic
	// key and is the deduplication mechanism (see the header comment).
	orderID, existed, err := p.registerOrder(ctx, token, req)
	if err != nil {
		return AuthorizeResult{Outcome: OutcomeUnknown}, err
	}

	if existed {
		// Paymob has seen this exact payment before. Do NOT charge again —
		// find out what happened the first time. This branch is the whole
		// reason FINDINGS §4 is not live in production.
		slog.InfoContext(ctx, "paymob: merchant_order_id already registered, replaying the original outcome",
			slog.String("orderId", req.OrderID),
			slog.String("merchantOrderId", req.IdempotencyKey))
		return p.replayExisting(ctx, token, req)
	}

	// Step 2: mint a payment key for that order.
	paymentKey, err := p.paymentKey(ctx, token, orderID, integrationID, req)
	if err != nil {
		return AuthorizeResult{Outcome: OutcomeUnknown}, err
	}

	// Step 3 depends on the rail.
	switch req.Method {
	case MethodCard, MethodInstallment:
		// The customer completes the card form (and 3-D Secure) in Paymob's
		// hosted iframe. No money has moved yet; the saga waits for the
		// callback. Returning APPROVED here would be a lie that costs us a
		// chargeback when the customer abandons at the 3-D Secure step.
		return AuthorizeResult{
			Outcome:     OutcomePending,
			OrderRef:    strconv.FormatInt(orderID, 10),
			RedirectURL: p.iframeURL(paymentKey),
			ExpiresAt:   time.Now().Add(paymobPaymentKeyTTL * time.Second),
			RawResponse: map[string]any{"paymob_order_id": orderID, "flow": "iframe"},
		}, nil

	case MethodWallet:
		return p.payWithWallet(ctx, paymentKey, orderID, req)

	case MethodCashOnDelivery:
		// Authorised on the promise of collection. Captured when the courier
		// confirms handover, which arrives as a separate callback.
		return AuthorizeResult{
			Outcome:     OutcomeApproved,
			OrderRef:    strconv.FormatInt(orderID, 10),
			ProviderRef: strconv.FormatInt(orderID, 10),
			RawResponse: map[string]any{"paymob_order_id": orderID, "flow": "cod"},
		}, nil
	}

	return AuthorizeResult{}, fmt.Errorf("%w: %s", ErrNotSupported, req.Method)
}

// registerOrder creates the Paymob order. The bool reports whether Paymob
// rejected it as a duplicate, which means this payment was already started.
func (p *Paymob) registerOrder(ctx context.Context, token string, req AuthorizeRequest) (int64, bool, error) {
	body := map[string]any{
		"auth_token":      token,
		"delivery_needed": false,
		"amount_cents":    strconv.FormatInt(req.Amount.Amount, 10),
		"currency":        req.Amount.Currency,
		// THE deduplication key. Deterministic, stable across processes and
		// retries. See internal/payment/psp_key.go.
		"merchant_order_id": req.IdempotencyKey,
		"items":             []any{},
	}

	var resp struct {
		ID      int64  `json:"id"`
		Message string `json:"message"`
		// Paymob returns validation errors under several shapes depending on
		// which layer rejected the request.
		MerchantOrderID []string `json:"merchant_order_id"`
		Detail          string   `json:"detail"`
	}

	err := p.post(ctx, "/ecommerce/orders", body, &resp)
	if err != nil {
		// A duplicate merchant_order_id comes back as a 4xx, which p.post
		// surfaces as an apiError. That is a successful deduplication, not a
		// failure.
		if isDuplicateOrder(err, resp.Message, resp.MerchantOrderID, resp.Detail) {
			return 0, true, nil
		}
		return 0, false, err
	}
	if resp.ID == 0 {
		return 0, false, &UnavailableError{
			Provider: "paymob", Op: "register-order",
			Cause: fmt.Errorf("no order id in response"),
		}
	}
	return resp.ID, false, nil
}

// isDuplicateOrder recognises Paymob's several ways of saying "you already
// registered this".
//
// String matching on an error message is fragile and this is the one place in
// the codebase doing it. It is here because the alternative — treating a
// duplicate as a generic failure — makes the saga retry, and a retry that
// Paymob keeps rejecting leaves the order stuck. A false negative here is
// caught by the reconciler; a false positive would only ever cause us to look
// up an order that exists.
func isDuplicateOrder(err error, message string, merchantOrderErrs []string, detail string) bool {
	haystack := strings.ToLower(strings.Join(append([]string{message, detail, err.Error()}, merchantOrderErrs...), " "))
	for _, needle := range []string{
		"duplicate",
		"already exist",
		"has already been taken",
		"order with merchant_order_id",
	} {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// replayExisting recovers the outcome of a payment Paymob has already seen.
func (p *Paymob) replayExisting(ctx context.Context, token string, req AuthorizeRequest) (AuthorizeResult, error) {
	order, err := p.lookupByMerchantOrderID(ctx, token, req.IdempotencyKey)
	if err != nil {
		// We know a payment exists but cannot read its state. UNKNOWN, not
		// declined — the reconciler has to resolve it, and in the meantime
		// the saga must not compensate against a possibly-successful charge.
		return AuthorizeResult{Outcome: OutcomeUnknown}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
	}

	txn := latestTransaction(order.Transactions)
	if txn == nil {
		// The order exists but no transaction was ever created — the customer
		// abandoned at the iframe. Re-issuing a payment key against the same
		// order is correct and does not double-charge.
		integrationID := p.cfg.IntegrationIDs[req.Method]
		key, keyErr := p.paymentKey(ctx, token, order.ID, integrationID, req)
		if keyErr != nil {
			return AuthorizeResult{Outcome: OutcomeUnknown}, keyErr
		}
		return AuthorizeResult{
			Outcome:     OutcomePending,
			OrderRef:    strconv.FormatInt(order.ID, 10),
			RedirectURL: p.iframeURL(key),
			RawResponse: map[string]any{"replayed": true, "paymob_order_id": order.ID},
		}, nil
	}

	return p.fromTransaction(txn, order.ID), nil
}

type paymobOrder struct {
	ID           int64               `json:"id"`
	Transactions []paymobTransaction `json:"transactions"`
}

type paymobTransaction struct {
	ID           int64  `json:"id"`
	Pending      bool   `json:"pending"`
	Success      bool   `json:"success"`
	IsVoided     bool   `json:"is_voided"`
	IsRefunded   bool   `json:"is_refunded"`
	IsCapture    bool   `json:"is_capture"`
	IsAuth       bool   `json:"is_auth"`
	ErrorOccured bool   `json:"error_occured"`
	AmountCents  int64  `json:"amount_cents"`
	Currency     string `json:"currency"`
	CreatedAt    string `json:"created_at"`
	Data         struct {
		Message         string `json:"message"`
		TxnResponseCode string `json:"txn_response_code"`
	} `json:"data"`
}

func (p *Paymob) lookupByMerchantOrderID(ctx context.Context, token, merchantOrderID string) (*paymobOrder, error) {
	var resp paymobOrder
	path := "/ecommerce/orders/transaction_inquiry"
	body := map[string]any{
		"auth_token":        token,
		"merchant_order_id": merchantOrderID,
	}
	if err := p.post(ctx, path, body, &resp); err != nil {
		return nil, err
	}
	if resp.ID == 0 {
		return nil, fmt.Errorf("paymob: no order found for merchant_order_id %q", merchantOrderID)
	}
	return &resp, nil
}

// latestTransaction picks the most meaningful transaction on an order.
// A successful one always wins over a failed one, because an order may have
// several attempts and the successful one is the truth about the money.
func latestTransaction(txns []paymobTransaction) *paymobTransaction {
	var best *paymobTransaction
	for i := range txns {
		t := &txns[i]
		if best == nil {
			best = t
			continue
		}
		if t.Success && !best.Success {
			best = t
			continue
		}
		if t.Success == best.Success && t.ID > best.ID {
			best = t
		}
	}
	return best
}

func (p *Paymob) fromTransaction(t *paymobTransaction, orderID int64) AuthorizeResult {
	res := AuthorizeResult{
		ProviderRef: strconv.FormatInt(t.ID, 10),
		OrderRef:    strconv.FormatInt(orderID, 10),
		RawResponse: map[string]any{
			"paymob_transaction_id": t.ID,
			"paymob_order_id":       orderID,
			"txn_response_code":     t.Data.TxnResponseCode,
			// Deliberately NOT copying source_data: it carries the PAN's last
			// four and the cardholder name.
		},
	}

	switch {
	case t.Success && !t.IsVoided && !t.IsRefunded:
		res.Outcome = OutcomeApproved
	case t.Pending:
		res.Outcome = OutcomePending
	case t.IsVoided || t.IsRefunded:
		// Reversed after the fact. From the saga's point of view this
		// authorisation is not usable.
		res.Outcome = OutcomeDeclined
		res.ReasonCode = ReasonCardDeclined
	default:
		res.Outcome = OutcomeDeclined
		res.ReasonCode = mapDeclineCode(t.Data.TxnResponseCode, t.Data.Message)
		res.DeclineCode = t.Data.TxnResponseCode
	}
	return res
}

func (p *Paymob) paymentKey(ctx context.Context, token string, orderID int64, integrationID int, req AuthorizeRequest) (string, error) {
	var resp struct {
		Token string `json:"token"`
	}

	body := map[string]any{
		"auth_token":     token,
		"amount_cents":   strconv.FormatInt(req.Amount.Amount, 10),
		"expiration":     paymobPaymentKeyTTL,
		"order_id":       orderID,
		"currency":       req.Amount.Currency,
		"integration_id": integrationID,
		// Stops the same order being paid twice at Paymob's end. A second
		// layer under merchant_order_id.
		"lock_order_when_paid": true,
		"billing_data":         billingData(req.Customer),
	}
	if req.ReturnURL != "" {
		body["redirect_url"] = req.ReturnURL
	}

	if err := p.post(ctx, "/acceptance/payment_keys", body, &resp); err != nil {
		return "", err
	}
	if resp.Token == "" {
		return "", &UnavailableError{Provider: "paymob", Op: "payment-key",
			Cause: fmt.Errorf("no payment key in response")}
	}
	return resp.Token, nil
}

// billingData builds Paymob's billing block.
//
// Paymob REJECTS empty strings on these fields and requires the literal "NA"
// instead. Sending "" produces a 400 whose message does not name the field,
// which is a genuinely unpleasant twenty minutes the first time.
func billingData(c Customer) map[string]any {
	na := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "NA"
		}
		return s
	}
	return map[string]any{
		"first_name":      na(c.FirstName),
		"last_name":       na(c.LastName),
		"email":           na(c.Email),
		"phone_number":    na(c.Phone),
		"street":          na(c.Street),
		"building":        "NA",
		"floor":           "NA",
		"apartment":       "NA",
		"city":            na(c.City),
		"state":           na(c.State),
		"country":         na(c.Country),
		"postal_code":     na(c.PostalCode),
		"shipping_method": "NA",
	}
}

func (p *Paymob) iframeURL(paymentKey string) string {
	return fmt.Sprintf("%s/acceptance/iframes/%d?payment_token=%s",
		strings.TrimSuffix(p.cfg.BaseURL, "/api")+"/api", p.cfg.IframeID, paymentKey)
}

// payWithWallet pushes a payment request to an Egyptian mobile wallet. The
// customer approves it on their handset; the money moves immediately on
// approval, so there is no separate capture.
func (p *Paymob) payWithWallet(ctx context.Context, paymentKey string, orderID int64, req AuthorizeRequest) (AuthorizeResult, error) {
	if req.WalletPhone == "" {
		return AuthorizeResult{Outcome: OutcomeDeclined, ReasonCode: ReasonCardDeclined},
			fmt.Errorf("paymob: a wallet payment requires the customer's mobile number")
	}

	var resp struct {
		RedirectURL string `json:"redirect_url"`
		ID          int64  `json:"id"`
		Pending     bool   `json:"pending"`
		Success     bool   `json:"success"`
		Message     string `json:"message"`
	}

	body := map[string]any{
		"source": map[string]any{
			"identifier": normaliseEgyptianMSISDN(req.WalletPhone),
			"subtype":    "WALLET",
		},
		"payment_token": paymentKey,
	}

	if err := p.post(ctx, "/acceptance/payments/pay", body, &resp); err != nil {
		return AuthorizeResult{Outcome: OutcomeUnknown}, err
	}

	return AuthorizeResult{
		// Always pending: the customer still has to approve on their phone.
		Outcome:     OutcomePending,
		ProviderRef: strconv.FormatInt(resp.ID, 10),
		OrderRef:    strconv.FormatInt(orderID, 10),
		RedirectURL: resp.RedirectURL,
		RawResponse: map[string]any{"flow": "wallet", "paymob_order_id": orderID},
	}, nil
}

// normaliseEgyptianMSISDN converts the forms customers actually type into the
// 01XXXXXXXXX Paymob expects: +201005550000, 00201005550000, 201005550000,
// "0100 555 0000" all become 01005550000.
func normaliseEgyptianMSISDN(raw string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, raw)

	switch {
	case strings.HasPrefix(digits, "0020") && len(digits) == 14:
		return "0" + digits[4:]
	case strings.HasPrefix(digits, "20") && len(digits) == 12:
		return "0" + digits[2:]
	case strings.HasPrefix(digits, "1") && len(digits) == 10:
		return "0" + digits
	default:
		return digits
	}
}

// ---------------------------------------------------------------------------
// Capture / Void / Refund

func (p *Paymob) Capture(ctx context.Context, req CaptureRequest) (Result, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}

	var resp paymobTransaction
	body := map[string]any{
		"auth_token":     token,
		"transaction_id": req.ProviderRef,
		"amount_cents":   req.Amount.Amount,
	}

	if err := p.post(ctx, "/acceptance/capture", body, &resp); err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}

	return Result{
		Outcome:     outcomeOf(resp),
		ProviderRef: strconv.FormatInt(resp.ID, 10),
		DeclineCode: resp.Data.TxnResponseCode,
		RawResponse: map[string]any{"paymob_transaction_id": resp.ID, "operation": "capture"},
	}, nil
}

func (p *Paymob) Void(ctx context.Context, req VoidRequest) (Result, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}

	var resp paymobTransaction
	body := map[string]any{
		"auth_token":     token,
		"transaction_id": req.ProviderRef,
	}

	if err := p.post(ctx, "/acceptance/void_refund/void", body, &resp); err != nil {
		// A void of something already voided is a success from the saga's
		// point of view: the desired end state has been reached. Treating it
		// as a failure would leave the saga stuck in COMPENSATING forever.
		if isAlreadyReversed(err) {
			return Result{Outcome: OutcomeApproved, ProviderRef: req.ProviderRef}, nil
		}
		return Result{Outcome: OutcomeUnknown}, err
	}

	return Result{
		Outcome:     outcomeOf(resp),
		ProviderRef: strconv.FormatInt(resp.ID, 10),
		RawResponse: map[string]any{"paymob_transaction_id": resp.ID, "operation": "void"},
	}, nil
}

func (p *Paymob) Refund(ctx context.Context, req RefundRequest) (Result, error) {
	token, err := p.authToken(ctx)
	if err != nil {
		return Result{Outcome: OutcomeUnknown}, err
	}

	var resp paymobTransaction
	body := map[string]any{
		"auth_token":     token,
		"transaction_id": req.ProviderRef,
		"amount_cents":   req.Amount.Amount,
	}

	if err := p.post(ctx, "/acceptance/void_refund/refund", body, &resp); err != nil {
		// Symmetric with Void, and it was missing. A refund of something
		// already refunded has reached the end state the caller asked for, so
		// reporting failure leaves the compensation retrying forever against a
		// transaction that is already done — and every retry pages someone.
		if isAlreadyReversed(err) {
			return Result{Outcome: OutcomeApproved, ProviderRef: req.ProviderRef}, nil
		}
		return Result{Outcome: OutcomeUnknown}, err
	}

	return Result{
		Outcome:     outcomeOf(resp),
		ProviderRef: strconv.FormatInt(resp.ID, 10),
		RawResponse: map[string]any{"paymob_transaction_id": resp.ID, "operation": "refund"},
	}, nil
}

// isAlreadyReversed matches every wording Paymob has been observed to use for
// "this transaction has already been reversed".
//
// Matching on prose is unpleasant and it is what the API gives us — there is no
// stable error code for this case. So the list is generous and each entry is a
// real observed string rather than a guess: "has been refunded before" is the
// one Paymob returns for a duplicate refund, and it matches none of the
// patterns that were here, so the refund path failed even once it was wired to
// call this.
func isAlreadyReversed(err error) bool {
	s := strings.ToLower(err.Error())

	for _, phrase := range []string{
		"already voided",
		"already refunded",
		"refunded before",
		"voided before",
		"already been refunded",
		"already been voided",
		"already reversed",
		"transaction_already_reversed",
		"transaction has been voided",
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

func outcomeOf(t paymobTransaction) Outcome {
	switch {
	case t.Success:
		return OutcomeApproved
	case t.Pending:
		return OutcomePending
	default:
		return OutcomeDeclined
	}
}

// ---------------------------------------------------------------------------
// Callbacks
//
// Paymob sends two things after a transaction:
//
//	a POST "transaction processed" callback  -> the authoritative one
//	a GET  "transaction response" redirect   -> the customer's browser
//
// Both carry an `hmac`. Both must be verified. The GET redirect is the one an
// attacker can trivially forge by typing a URL, so treating it as anything
// other than a hint to refresh the page is a way to mark orders paid for free.

// hmacFieldOrder is Paymob's documented concatenation order for the
// transaction HMAC. It is lexicographic over the flattened field names, and it
// is NOT negotiable: the fields are concatenated with no separator, in exactly
// this sequence, then HMAC-SHA512'd with the account's HMAC secret.
//
// Getting the order wrong rejects every callback. The symptom is not an error
// — it is orders silently stuck in AUTHORIZING while Paymob's dashboard shows
// them as paid.
var hmacFieldOrder = []string{
	"amount_cents",
	"created_at",
	"currency",
	"error_occured",
	"has_parent_transaction",
	"id",
	"integration_id",
	"is_3d_secure",
	"is_auth",
	"is_capture",
	"is_refunded",
	"is_standalone_payment",
	"is_voided",
	"order.id",
	"owner",
	"pending",
	"source_data.pan",
	"source_data.sub_type",
	"source_data.type",
	"success",
}

func (p *Paymob) ParseCallback(ctx context.Context, body []byte, headers map[string]string, query map[string]string) (Callback, error) {
	// The POST callback wraps the transaction in {type, obj}; the GET redirect
	// sends the same fields flattened as query parameters.
	var envelope struct {
		Type string          `json:"type"`
		Obj  json.RawMessage `json:"obj"`
		HMAC string          `json:"hmac"`
	}

	var flat map[string]any
	var presentedHMAC string

	if len(body) > 0 && json.Unmarshal(body, &envelope) == nil && len(envelope.Obj) > 0 {
		if err := json.Unmarshal(envelope.Obj, &flat); err != nil {
			return Callback{}, fmt.Errorf("paymob: callback obj is not an object: %w", err)
		}
		presentedHMAC = firstNonEmpty(envelope.HMAC, query["hmac"], headers["hmac"])
	} else if len(query) > 0 {
		flat = make(map[string]any, len(query))
		for k, v := range query {
			flat[k] = v
		}
		presentedHMAC = query["hmac"]
	} else {
		return Callback{}, fmt.Errorf("paymob: callback had neither a JSON body nor query parameters")
	}

	if presentedHMAC == "" {
		return Callback{}, fmt.Errorf("%w: no hmac present", ErrInvalidSignature)
	}

	expected := p.computeHMAC(flat)
	// Constant time. A byte-by-byte comparison leaks how much of a forged
	// signature was correct, which is enough to forge one given time.
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(presentedHMAC))) {
		slog.WarnContext(ctx, "paymob callback rejected: HMAC mismatch",
			slog.String("transactionId", stringField(flat, "id")))
		return Callback{}, ErrInvalidSignature
	}

	return p.normaliseCallback(flat, envelope.Type), nil
}

// computeHMAC concatenates the fields in hmacFieldOrder and HMAC-SHA512s them.
func (p *Paymob) computeHMAC(flat map[string]any) string {
	var sb strings.Builder
	for _, field := range hmacFieldOrder {
		sb.WriteString(hmacValue(flat, field))
	}

	mac := hmac.New(sha512.New, []byte(p.cfg.HMACSecret))
	mac.Write([]byte(sb.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// hmacValue reads a possibly-nested field and renders it the way Paymob does.
//
// The rendering rules are the fiddly part and each one is a real
// incompatibility if you get it wrong:
//
//   - booleans are Python-style "true"/"false" lowercase in the JSON body, but
//     arrive as "true"/"false" strings in the query redirect. Both normalise
//     to lowercase here.
//   - numbers must have no decimal point. encoding/json decodes every JSON
//     number into float64, so 12345 becomes 12345.0 and naive formatting
//     produces "12345.0" — which does not match and rejects the callback.
//   - a missing or null field contributes the empty string, not "null".
func hmacValue(flat map[string]any, field string) string {
	var v any = flat

	for _, part := range strings.Split(field, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			// The GET redirect flattens nesting into dotted keys, so try the
			// whole dotted name as a literal key before giving up.
			if raw, present := flat[field]; present {
				return renderHMACScalar(raw)
			}
			return ""
		}
		v, ok = m[part]
		if !ok {
			if raw, present := flat[field]; present {
				return renderHMACScalar(raw)
			}
			return ""
		}
	}
	return renderHMACScalar(v)
}

func renderHMACScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		// Query-string booleans arrive capitalised as Python renders them.
		if t == "True" || t == "False" {
			return strings.ToLower(t)
		}
		return t
	case float64:
		// No decimal point for integral values. This is the line that most
		// often breaks a Paymob integration written in Go.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case int, int32, int64:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func (p *Paymob) normaliseCallback(flat map[string]any, kind string) Callback {
	success := boolField(flat, "success")
	pending := boolField(flat, "pending")
	voided := boolField(flat, "is_voided")
	refunded := boolField(flat, "is_refunded")
	isCapture := boolField(flat, "is_capture")

	cb := Callback{
		Verified:    true,
		ProviderRef: stringField(flat, "id"),
		Amount: Money{
			Amount:   intField(flat, "amount_cents"),
			Currency: firstNonEmpty(stringField(flat, "currency"), p.cfg.Currency),
		},
		Success:    success,
		OccurredAt: parsePaymobTime(stringField(flat, "created_at")),
		RawResponse: map[string]any{
			"paymob_transaction_id": stringField(flat, "id"),
			"callback_type":         kind,
			// source_data is deliberately excluded: it carries the PAN's last
			// four and the cardholder name.
		},
	}

	// Recover OUR order id. merchant_order_id is the deterministic key we
	// sent; the service layer maps it back to a payment row.
	cb.OrderID = firstNonEmpty(
		nestedString(flat, "order", "merchant_order_id"),
		stringField(flat, "merchant_order_id"),
	)

	switch {
	case refunded:
		cb.Kind = CallbackRefunded
	case voided:
		cb.Kind = CallbackVoided
	case success && isCapture:
		cb.Kind = CallbackCaptured
	case success:
		cb.Kind = CallbackAuthorized
	case pending:
		// Still in flight. The service layer records it and keeps waiting.
		cb.Kind = CallbackAuthorized
		cb.Success = false
	default:
		cb.Kind = CallbackFailed
		cb.DeclineCode = nestedString(flat, "data", "txn_response_code")
		cb.ReasonCode = mapDeclineCode(cb.DeclineCode, nestedString(flat, "data", "message"))
	}

	return cb
}

// mapDeclineCode turns a provider code into one of our reason codes.
//
// The split that actually matters is retriable vs not. Everything that is not
// clearly a transient provider problem is treated as a hard decline, because
// retrying a genuinely declined card just delays telling the customer.
func mapDeclineCode(code, message string) ReasonCode {
	m := strings.ToLower(message)

	switch {
	case strings.Contains(m, "insufficient"):
		return ReasonInsufficientFunds
	case strings.Contains(m, "expired"):
		return ReasonCardExpired
	case strings.Contains(m, "cvv") || strings.Contains(m, "cvc") || strings.Contains(m, "security code"):
		return ReasonInvalidCVC
	case strings.Contains(m, "3d") || strings.Contains(m, "authentication"):
		return ReasonThreeDSFailed
	case strings.Contains(m, "fraud") || strings.Contains(m, "risk"):
		return ReasonFraudSuspected
	case strings.Contains(m, "timeout") || strings.Contains(m, "unavailable") ||
		strings.Contains(m, "try again") || strings.Contains(m, "issuer down"):
		return ReasonProviderUnavailable
	}

	// ISO-8583 response codes, which Paymob passes through from the acquirer.
	switch code {
	case "51":
		return ReasonInsufficientFunds
	case "54":
		return ReasonCardExpired
	case "82", "N7":
		return ReasonInvalidCVC
	case "91", "96":
		// Issuer unavailable / system malfunction. Genuinely transient.
		return ReasonProviderUnavailable
	case "04", "07", "41", "43":
		return ReasonFraudSuspected
	}

	return ReasonCardDeclined
}

// ---------------------------------------------------------------------------
// HTTP plumbing

type apiError struct {
	StatusCode int
	Body       string
	Op         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("paymob: %s returned %d: %s", e.Op, e.StatusCode, truncate(e.Body, 400))
}

// Unwrap lets errors.Is see a 5xx as retriable while a 4xx stays terminal.
func (e *apiError) Unwrap() error {
	if e.StatusCode >= 500 {
		return ErrProviderUnavailable
	}
	return nil
}

func (p *Paymob) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("paymob: marshal %s request: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		// Transport failure. We do not know whether Paymob processed it, so
		// the caller must treat this as UNKNOWN rather than declined.
		return &UnavailableError{Provider: "paymob", Op: path, Cause: err}
	}
	defer resp.Body.Close()

	// Cap the read. A provider returning an unexpected HTML error page should
	// not be able to exhaust our memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &UnavailableError{Provider: "paymob", Op: path, Cause: err}
	}

	slog.DebugContext(ctx, "paymob call",
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.Int64("latencyMs", time.Since(start).Milliseconds()))

	if resp.StatusCode >= 400 {
		// Decode into out anyway: Paymob puts its validation detail in the
		// error body, and registerOrder needs it to spot a duplicate.
		_ = json.Unmarshal(raw, out)
		return &apiError{StatusCode: resp.StatusCode, Body: string(raw), Op: path}
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &UnavailableError{Provider: "paymob", Op: path,
				Cause: fmt.Errorf("unparseable response: %w", err)}
		}
	}
	return nil
}

// Health mints an auth token. It is the cheapest call that proves both
// connectivity and that our API key is still valid — a revoked key is
// otherwise only discovered on the first real payment.
func (p *Paymob) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := p.authToken(ctx)
	return err
}

// ---------------------------------------------------------------------------

func stringField(m map[string]any, key string) string {
	return renderHMACScalar(m[key])
}

func nestedString(m map[string]any, keys ...string) string {
	var v any = m
	for _, k := range keys {
		obj, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = obj[k]
	}
	return renderHMACScalar(v)
}

func boolField(m map[string]any, key string) bool {
	switch t := m[key].(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func intField(m map[string]any, key string) int64 {
	switch t := m[key].(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case int64:
		return t
	}
	return 0
}

func parsePaymobTime(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
