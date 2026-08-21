package psp

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testHMACSecret = "AB1234567890ABCDEF1234567890ABCD"
	testAPIKey     = "test-api-key"
)

func newTestPaymob(t *testing.T, baseURL string) *Paymob {
	t.Helper()
	p, err := NewPaymob(PaymobConfig{
		BaseURL:    baseURL,
		APIKey:     testAPIKey,
		HMACSecret: testHMACSecret,
		IntegrationIDs: map[PaymentMethod]int{
			MethodCard:           1001,
			MethodWallet:         1002,
			MethodCashOnDelivery: 1003,
		},
		IframeID: 5001,
		Currency: "EGP",
	})
	if err != nil {
		t.Fatalf("NewPaymob: %v", err)
	}
	return p
}

// A realistic transaction callback body. Field values chosen to exercise every
// rendering rule: integral floats, booleans, a nested order, and a nested
// source_data.
func sampleCallback() map[string]any {
	return map[string]any{
		"id":                     float64(123456789),
		"amount_cents":           float64(129900),
		"created_at":             "2026-08-17T10:00:00.123456",
		"currency":               "EGP",
		"error_occured":          false,
		"has_parent_transaction": false,
		"integration_id":         float64(1001),
		"is_3d_secure":           true,
		"is_auth":                false,
		"is_capture":             false,
		"is_refunded":            false,
		"is_standalone_payment":  true,
		"is_voided":              false,
		"owner":                  float64(987654),
		"pending":                false,
		"success":                true,
		"order": map[string]any{
			"id":                float64(555444),
			"merchant_order_id": "souq6ecd285104e9c5ff7e6a14b56f7893a0",
		},
		"source_data": map[string]any{
			"pan":      "2346",
			"sub_type": "MasterCard",
			"type":     "card",
		},
		"data": map[string]any{
			"message":           "Approved",
			"txn_response_code": "APPROVED",
		},
	}
}

// signLikePaymob reproduces what Paymob does on its side, independently of the
// production code path, so the test is not just asserting that computeHMAC
// equals itself.
func signLikePaymob(t *testing.T, obj map[string]any) string {
	t.Helper()

	get := func(path string) string {
		var v any = obj
		for _, part := range strings.Split(path, ".") {
			m, ok := v.(map[string]any)
			if !ok {
				return ""
			}
			v = m[part]
		}
		switch x := v.(type) {
		case nil:
			return ""
		case bool:
			if x {
				return "true"
			}
			return "false"
		case string:
			return x
		case float64:
			return fmt.Sprintf("%d", int64(x))
		default:
			return fmt.Sprintf("%v", x)
		}
	}

	var sb strings.Builder
	for _, f := range hmacFieldOrder {
		sb.WriteString(get(f))
	}

	mac := hmac.New(sha512.New, []byte(testHMACSecret))
	mac.Write([]byte(sb.String()))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// HMAC — the security boundary
// ---------------------------------------------------------------------------

func TestValidCallbackIsAccepted(t *testing.T) {
	p := newTestPaymob(t, "http://unused")
	obj := sampleCallback()

	body, _ := json.Marshal(map[string]any{
		"type": "TRANSACTION",
		"obj":  obj,
		"hmac": signLikePaymob(t, obj),
	})

	cb, err := p.ParseCallback(context.Background(), body, nil, nil)
	if err != nil {
		t.Fatalf("a correctly signed callback was rejected: %v", err)
	}
	if !cb.Verified {
		t.Error("Verified is false on a callback that passed verification")
	}
	if cb.Kind != CallbackAuthorized {
		t.Errorf("Kind = %s, want AUTHORIZED", cb.Kind)
	}
	if cb.ProviderRef != "123456789" {
		t.Errorf("ProviderRef = %q, want 123456789", cb.ProviderRef)
	}
	if cb.Amount.Amount != 129900 || cb.Amount.Currency != "EGP" {
		t.Errorf("Amount = %+v, want 129900 EGP", cb.Amount)
	}
	// This is how the service finds the payment row. Losing it means a
	// verified callback that cannot be applied to anything.
	if cb.OrderID != "souq6ecd285104e9c5ff7e6a14b56f7893a0" {
		t.Errorf("OrderID = %q, want the merchant_order_id we sent", cb.OrderID)
	}
}

// THE test. A webhook endpoint that accepts a forged callback lets anyone mark
// any order as paid.
func TestForgedCallbackIsRejected(t *testing.T) {
	p := newTestPaymob(t, "http://unused")

	cases := []struct {
		name   string
		mutate func(obj map[string]any) (map[string]any, string)
	}{
		{
			name:   "no hmac at all",
			mutate: func(o map[string]any) (map[string]any, string) { return o, "" },
		},
		{
			name: "hmac from a different secret",
			mutate: func(o map[string]any) (map[string]any, string) {
				mac := hmac.New(sha512.New, []byte("the-attackers-guess-at-our-secret"))
				mac.Write([]byte("whatever"))
				return o, hex.EncodeToString(mac.Sum(nil))
			},
		},
		{
			name: "amount raised after signing",
			mutate: func(o map[string]any) (map[string]any, string) {
				sig := signLikePaymob(t, o)
				o["amount_cents"] = float64(1) // pay 1 piastre for a 1299 EGP order
				return o, sig
			},
		},
		{
			name: "success flipped to true after signing",
			mutate: func(o map[string]any) (map[string]any, string) {
				o["success"] = false
				sig := signLikePaymob(t, o)
				o["success"] = true
				return o, sig
			},
		},
		{
			name: "order id swapped after signing",
			mutate: func(o map[string]any) (map[string]any, string) {
				sig := signLikePaymob(t, o)
				o["order"] = map[string]any{
					"id": float64(999), "merchant_order_id": "somebody-elses-order",
				}
				return o, sig
			},
		},
		{
			name: "hmac truncated",
			mutate: func(o map[string]any) (map[string]any, string) {
				return o, signLikePaymob(t, o)[:32]
			},
		},
		{
			name:   "hmac is empty string",
			mutate: func(o map[string]any) (map[string]any, string) { return o, "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, sig := tc.mutate(sampleCallback())
			body, _ := json.Marshal(map[string]any{"type": "TRANSACTION", "obj": obj, "hmac": sig})

			cb, err := p.ParseCallback(context.Background(), body, nil, nil)
			if err == nil {
				t.Fatalf("FORGED CALLBACK ACCEPTED — anyone can mark an order paid. Result: %+v", cb)
			}
			if cb.Verified {
				t.Error("Verified is true on a rejected callback")
			}
		})
	}
}

// encoding/json turns every JSON number into float64. Formatting 129900.0
// naively gives "129900.0", which does not match Paymob's "129900" and rejects
// every single callback. This is the most common way a Go Paymob integration
// fails, and the symptom is silent: orders stuck in AUTHORIZING while the
// Paymob dashboard shows them paid.
func TestIntegralNumbersRenderWithoutADecimalPoint(t *testing.T) {
	cases := map[string]struct {
		in   any
		want string
	}{
		"integral float":     {float64(129900), "129900"},
		"zero":               {float64(0), "0"},
		"large id":           {float64(123456789012), "123456789012"},
		"genuine fraction":   {float64(1.5), "1.5"},
		"bool true":          {true, "true"},
		"bool false":         {false, "false"},
		"python-cased true":  {"True", "true"},
		"python-cased false": {"False", "false"},
		"nil":                {nil, ""},
		"plain string":       {"EGP", "EGP"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := renderHMACScalar(c.in); got != c.want {
				t.Errorf("renderHMACScalar(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The GET redirect flattens nesting into dotted query keys. Both shapes must
// produce the same signature or the browser redirect can never be verified.
func TestFlattenedQueryCallbackVerifies(t *testing.T) {
	p := newTestPaymob(t, "http://unused")
	obj := sampleCallback()
	sig := signLikePaymob(t, obj)

	query := map[string]string{
		"id":                     "123456789",
		"amount_cents":           "129900",
		"created_at":             "2026-08-17T10:00:00.123456",
		"currency":               "EGP",
		"error_occured":          "false",
		"has_parent_transaction": "false",
		"integration_id":         "1001",
		"is_3d_secure":           "true",
		"is_auth":                "false",
		"is_capture":             "false",
		"is_refunded":            "false",
		"is_standalone_payment":  "true",
		"is_voided":              "false",
		"order":                  "555444",
		"order.id":               "555444",
		"owner":                  "987654",
		"pending":                "false",
		"source_data.pan":        "2346",
		"source_data.sub_type":   "MasterCard",
		"source_data.type":       "card",
		"success":                "true",
		"merchant_order_id":      "souq6ecd285104e9c5ff7e6a14b56f7893a0",
		"hmac":                   sig,
	}

	cb, err := p.ParseCallback(context.Background(), nil, nil, query)
	if err != nil {
		t.Fatalf("the flattened redirect callback was rejected: %v", err)
	}
	if !cb.Success {
		t.Error("Success is false on a successful redirect callback")
	}
}

// The field order is not negotiable. If someone reorders the slice while
// tidying up, every callback silently stops verifying.
func TestHMACFieldOrderIsPinned(t *testing.T) {
	want := []string{
		"amount_cents", "created_at", "currency", "error_occured",
		"has_parent_transaction", "id", "integration_id", "is_3d_secure",
		"is_auth", "is_capture", "is_refunded", "is_standalone_payment",
		"is_voided", "order.id", "owner", "pending", "source_data.pan",
		"source_data.sub_type", "source_data.type", "success",
	}

	if len(hmacFieldOrder) != len(want) {
		t.Fatalf("hmacFieldOrder has %d fields, Paymob documents %d",
			len(hmacFieldOrder), len(want))
	}
	for i := range want {
		if hmacFieldOrder[i] != want[i] {
			t.Errorf("field %d is %q, want %q — reordering this rejects every callback",
				i, hmacFieldOrder[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Idempotency — the FINDINGS §4 property, adapted to a provider with no
// idempotency header
// ---------------------------------------------------------------------------

// A duplicate merchant_order_id must NOT produce a second charge. This is the
// entire safety argument for using Paymob at all.
func TestDuplicateOrderDoesNotChargeTwice(t *testing.T) {
	var orderRegistrations, payCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/auth/tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "auth-token"})

		case "/ecommerce/orders":
			orderRegistrations++
			// First registration succeeds; every later one is rejected the
			// way Paymob rejects a duplicate merchant_order_id.
			if orderRegistrations == 1 {
				json.NewEncoder(w).Encode(map[string]any{"id": 555444})
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"message":           "duplicate",
				"merchant_order_id": []string{"Order with merchant_order_id already exist"},
			})

		case "/ecommerce/orders/transaction_inquiry":
			// The lookup that replaces the second charge.
			json.NewEncoder(w).Encode(map[string]any{
				"id": 555444,
				"transactions": []map[string]any{{
					"id": 123456789, "success": true, "pending": false,
					"amount_cents": 129900, "currency": "EGP",
				}},
			})

		case "/acceptance/payment_keys":
			json.NewEncoder(w).Encode(map[string]any{"token": "payment-key"})

		case "/acceptance/payments/pay":
			payCalls++
			json.NewEncoder(w).Encode(map[string]any{
				"id": 123456789, "pending": true, "redirect_url": "https://wallet/approve",
			})

		default:
			t.Errorf("unexpected call to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := newTestPaymob(t, srv.URL)

	req := AuthorizeRequest{
		IdempotencyKey: "souq6ecd285104e9c5ff7e6a14b56f7893a0",
		OrderID:        "ord_01J8Z",
		PaymentID:      "pay_01J8Z",
		Amount:         Money{Amount: 129900, Currency: "EGP"},
		Method:         MethodWallet,
		WalletPhone:    "01005550000",
		Customer:       Customer{FirstName: "Ahmed", Email: "a@example.com", Phone: "01005550000"},
	}

	first, err := p.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("first authorize: %v", err)
	}
	if first.Outcome != OutcomePending {
		t.Errorf("first outcome = %s, want PENDING (the customer still has to approve)", first.Outcome)
	}

	// The retry. Same deterministic key, exactly as it would be after a crash
	// and a reaper release.
	second, err := p.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("second authorize: %v", err)
	}

	if payCalls != 1 {
		t.Fatalf("DOUBLE CHARGE: the wallet pay endpoint was called %d times for one logical payment "+
			"(docs/DESIGN-INVARIANTS.md §4)", payCalls)
	}
	if second.Outcome != OutcomeApproved {
		t.Errorf("the replay returned %s, want APPROVED — it should reflect what actually happened",
			second.Outcome)
	}
	if second.ProviderRef != "123456789" {
		t.Errorf("the replay lost the original transaction reference: %q", second.ProviderRef)
	}
}

func TestDuplicateDetectionRecognisesPaymobsWordings(t *testing.T) {
	cases := []struct {
		name    string
		message string
		field   []string
		want    bool
	}{
		{"plain duplicate", "duplicate", nil, true},
		{"already exists", "", []string{"Order with merchant_order_id already exist"}, true},
		{"has already been taken", "", []string{"This field has already been taken."}, true},
		{"unrelated validation error", "amount_cents must be positive", nil, false},
		{"auth failure", "invalid auth token", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &apiError{StatusCode: 400, Body: tc.message, Op: "/ecommerce/orders"}
			if got := isDuplicateOrder(err, tc.message, tc.field, ""); got != tc.want {
				t.Errorf("isDuplicateOrder = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Outcome mapping
// ---------------------------------------------------------------------------

// A transport failure is not a decline. Treating it as one cancels an order
// whose money may already have moved.
func TestTransportFailureIsUnknownNotDeclined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/tokens" {
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
			return
		}
		// Hang up mid-response.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	p := newTestPaymob(t, srv.URL)

	res, err := p.Authorize(context.Background(), AuthorizeRequest{
		IdempotencyKey: "k", OrderID: "ord_1", Amount: Money{Amount: 100, Currency: "EGP"},
		Method: MethodCard,
	})

	if err == nil {
		t.Fatal("a dropped connection produced no error")
	}
	if res.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %s, want UNKNOWN — a lost response is not a decline", res.Outcome)
	}
}

func TestFiveHundredIsRetriable(t *testing.T) {
	err := &apiError{StatusCode: 503, Body: "gateway down", Op: "/acceptance/capture"}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Error("a 503 should unwrap to ErrProviderUnavailable so the saga backs off")
	}

	err = &apiError{StatusCode: 400, Body: "bad request", Op: "/acceptance/capture"}
	if errors.Is(err, ErrProviderUnavailable) {
		t.Error("a 400 must not be retriable; retrying it just burns the budget")
	}
}

func TestDeclineCodeMapping(t *testing.T) {
	cases := []struct {
		code, message string
		want          ReasonCode
		retriable     bool
	}{
		{"51", "Insufficient funds", ReasonInsufficientFunds, false},
		{"54", "Expired card", ReasonCardExpired, false},
		{"82", "Invalid CVV", ReasonInvalidCVC, false},
		{"91", "Issuer unavailable", ReasonProviderUnavailable, true},
		{"96", "System malfunction", ReasonProviderUnavailable, true},
		{"05", "Do not honour", ReasonCardDeclined, false},
		{"", "3D Secure authentication failed", ReasonThreeDSFailed, false},
		{"", "Transaction flagged by risk engine", ReasonFraudSuspected, false},
	}

	for _, c := range cases {
		got := mapDeclineCode(c.code, c.message)
		if got != c.want {
			t.Errorf("mapDeclineCode(%q, %q) = %s, want %s", c.code, c.message, got, c.want)
		}
		if got.Retriable() != c.retriable {
			t.Errorf("%s retriable = %v, want %v — getting this wrong either hammers a "+
				"struggling provider or cancels a recoverable order",
				got, got.Retriable(), c.retriable)
		}
	}
}

// ---------------------------------------------------------------------------
// Egyptian specifics
// ---------------------------------------------------------------------------

func TestEgyptianPhoneNormalisation(t *testing.T) {
	cases := map[string]string{
		"01005550000":      "01005550000",
		"+201005550000":    "01005550000",
		"00201005550000":   "01005550000",
		"201005550000":     "01005550000",
		"0100 555 0000":    "01005550000",
		"+20 100 555 0000": "01005550000",
		"1005550000":       "01005550000",
	}

	for in, want := range cases {
		if got := normaliseEgyptianMSISDN(in); got != want {
			t.Errorf("normaliseEgyptianMSISDN(%q) = %q, want %q", in, got, want)
		}
	}
}

// Paymob rejects empty strings on billing fields with a message that does not
// name the offending field.
func TestBillingDataNeverContainsEmptyStrings(t *testing.T) {
	bd := billingData(Customer{FirstName: "Ahmed"}) // everything else blank

	for field, v := range bd {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if strings.TrimSpace(s) == "" {
			t.Errorf("billing field %q is empty; Paymob requires the literal \"NA\"", field)
		}
	}
	if bd["first_name"] != "Ahmed" {
		t.Error("a supplied value was overwritten with NA")
	}
	if bd["city"] != "NA" {
		t.Errorf("city = %v, want NA", bd["city"])
	}
}

func TestCurrencyMismatchIsRejectedLocally(t *testing.T) {
	p := newTestPaymob(t, "http://unused")

	_, err := p.Authorize(context.Background(), AuthorizeRequest{
		IdempotencyKey: "k", OrderID: "ord_1",
		Amount: Money{Amount: 1000, Currency: "USD"}, // account is EGP
		Method: MethodCard,
	})
	if err == nil {
		t.Fatal("a currency mismatch reached Paymob instead of failing locally")
	}
	if !strings.Contains(err.Error(), "EGP") {
		t.Errorf("the error does not name the configured currency: %v", err)
	}
}

// Wallets and COD move money in one step; there is nothing to capture later.
func TestSupportsCaptureReflectsTheRail(t *testing.T) {
	p := newTestPaymob(t, "http://unused")

	if p.SupportsCapture(MethodWallet) {
		t.Error("wallets have no separate capture: the money moves on approval")
	}
	if p.SupportsCapture(MethodCashOnDelivery) {
		t.Error("COD has no provider-side capture")
	}
	// Card depends on how the integration is provisioned.
	if p.SupportsCapture(MethodCard) {
		t.Error("with AuthorizeOnly=false, card is auth-and-capture in one step")
	}
}

func TestRefusesToStartWithoutAnHMACSecret(t *testing.T) {
	_, err := NewPaymob(PaymobConfig{
		APIKey:         testAPIKey,
		IntegrationIDs: map[PaymentMethod]int{MethodCard: 1},
	})
	if err == nil {
		t.Fatal("started without an HMAC secret — the webhook would be unauthenticated")
	}
	if !strings.Contains(err.Error(), "HMAC") {
		t.Errorf("the error does not explain what is missing: %v", err)
	}
}

// A void of something already voided must read as success, or the saga sits in
// COMPENSATING forever waiting for an acknowledgement that will never come.
func TestVoidOfAnAlreadyVoidedTransactionSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/tokens" {
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"message": "Transaction already voided"})
	}))
	defer srv.Close()

	p := newTestPaymob(t, srv.URL)

	res, err := p.Void(context.Background(), VoidRequest{
		IdempotencyKey: "k", OrderID: "ord_1", ProviderRef: "123456789",
	})
	if err != nil {
		t.Fatalf("an already-voided transaction produced an error: %v", err)
	}
	if res.Outcome != OutcomeApproved {
		t.Errorf("outcome = %s, want APPROVED — the desired end state was already reached", res.Outcome)
	}
}

// The auth token is cached; a burst of concurrent calls must mint exactly one.
func TestAuthTokenIsCachedAcrossConcurrentCalls(t *testing.T) {
	var authCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/tokens" {
			authCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"token": "t"})
	}))
	defer srv.Close()

	p := newTestPaymob(t, srv.URL)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = p.authToken(context.Background())
		}()
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	if authCalls != 1 {
		t.Errorf("minted %d auth tokens for 20 concurrent calls; Paymob rate-limits this endpoint", authCalls)
	}
}

// The PAN's last four and the cardholder name must never reach our storage.
func TestCallbackDoesNotRetainCardData(t *testing.T) {
	p := newTestPaymob(t, "http://unused")
	obj := sampleCallback()
	body, _ := json.Marshal(map[string]any{
		"type": "TRANSACTION", "obj": obj, "hmac": signLikePaymob(t, obj),
	})

	cb, err := p.ParseCallback(context.Background(), body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	serialised, _ := json.Marshal(cb.RawResponse)
	for _, forbidden := range []string{"2346", "source_data", "pan", "MasterCard"} {
		if strings.Contains(string(serialised), forbidden) {
			t.Errorf("RawResponse leaked %q into storage: %s", forbidden, serialised)
		}
	}
}

// ---------------------------------------------------------------------------
// Capture, Refund and Health.
//
// These were the uncovered half of this file, and they are the half that moves
// money in the direction the customer notices: a capture that reports success
// when Paymob declined it leaves the saga believing it has been paid.

func TestCaptureMapsPaymobsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		success bool
		pending bool
		want    Outcome
	}{
		{"an approved capture", true, false, OutcomeApproved},
		{"a pending capture", false, true, OutcomePending},
		// Neither success nor pending is a decline. Defaulting the other way —
		// treating an unrecognised shape as approved — is how a saga confirms
		// an order nothing was charged for.
		{"a declined capture", false, false, OutcomeDeclined},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/auth/tokens" {
					json.NewEncoder(w).Encode(map[string]any{"token": "t"})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{
					"id": 987654, "success": tc.success, "pending": tc.pending,
					"data": map[string]any{"txn_response_code": "APPROVED"},
				})
			}))
			defer srv.Close()

			res, err := newTestPaymob(t, srv.URL).Capture(context.Background(), CaptureRequest{
				IdempotencyKey: "k", OrderID: "ord_1", ProviderRef: "987654",
				Amount: Money{Amount: 129900, Currency: "EGP"},
			})
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if res.Outcome != tc.want {
				t.Errorf("outcome = %s, want %s", res.Outcome, tc.want)
			}
			if res.ProviderRef != "987654" {
				t.Errorf("providerRef = %q, want the transaction id back", res.ProviderRef)
			}
		})
	}
}

// A transport failure during capture must be UNKNOWN, never DECLINED. The
// money may well have moved; only the answer was lost. Reporting DECLINED
// makes the saga compensate a capture that actually succeeded.
func TestCaptureTransportFailureIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/tokens" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
			return
		}
		// Close without a response: the request left, the answer did not.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("the test server does not support hijacking")
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()

	res, _ := newTestPaymob(t, srv.URL).Capture(context.Background(), CaptureRequest{
		IdempotencyKey: "k", OrderID: "ord_1", ProviderRef: "1",
		Amount: Money{Amount: 100, Currency: "EGP"},
	})
	if res.Outcome != OutcomeUnknown {
		t.Errorf("outcome = %s, want UNKNOWN — the money may have moved", res.Outcome)
	}
}

func TestRefundMapsPaymobsOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/tokens" {
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 111222, "success": true})
	}))
	defer srv.Close()

	res, err := newTestPaymob(t, srv.URL).Refund(context.Background(), RefundRequest{
		IdempotencyKey: "k", OrderID: "ord_1", ProviderRef: "111222",
		Amount: Money{Amount: 5000, Currency: "EGP"},
	})
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if res.Outcome != OutcomeApproved {
		t.Errorf("outcome = %s, want APPROVED", res.Outcome)
	}
}

// Same reasoning as the void case: a refund of something already refunded has
// reached the end state the caller asked for. Failing it would leave the
// compensation retrying forever against a transaction that is already done.
func TestRefundOfAnAlreadyRefundedTransactionSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth/tokens" {
			json.NewEncoder(w).Encode(map[string]any{"token": "t"})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"message": "Transaction has been refunded before"})
	}))
	defer srv.Close()

	res, err := newTestPaymob(t, srv.URL).Refund(context.Background(), RefundRequest{
		IdempotencyKey: "k", OrderID: "ord_1", ProviderRef: "1",
		Amount: Money{Amount: 100, Currency: "EGP"},
	})
	if err != nil {
		t.Fatalf("an already-refunded transaction produced an error: %v", err)
	}
	if res.Outcome != OutcomeApproved {
		t.Errorf("outcome = %s, want APPROVED", res.Outcome)
	}
}

func TestHealthReflectsWhetherPaymobAnswers(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"token": "t"})
	}))
	defer ok.Close()

	if err := newTestPaymob(t, ok.URL).Health(context.Background()); err != nil {
		t.Errorf("health against a working Paymob: %v", err)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()

	if err := newTestPaymob(t, down.URL).Health(context.Background()); err == nil {
		t.Error("health reported healthy while Paymob was returning 503")
	}
}

func TestNameIdentifiesTheProvider(t *testing.T) {
	if got := newTestPaymob(t, "http://unused").Name(); got != "paymob" {
		t.Errorf("Name() = %q, want %q — the ledger records this", got, "paymob")
	}
}

// Every wording Paymob has been observed to use for an already-reversed
// transaction. There is no stable error code for this case, so the match is on
// prose — which makes it exactly the kind of thing that silently stops working.
func TestAlreadyReversedRecognisesPaymobsWordings(t *testing.T) {
	reversed := []string{
		"Transaction already voided",
		"Transaction has been refunded before",
		"transaction has already been refunded",
		"TRANSACTION_ALREADY_REVERSED",
		"This transaction has been voided",
	}
	for _, message := range reversed {
		if !isAlreadyReversed(errors.New(message)) {
			t.Errorf("did not recognise %q as already reversed", message)
		}
	}

	// And must not swallow a genuine failure as success.
	notReversed := []string{
		"Insufficient funds",
		"Invalid transaction id",
		"refund amount exceeds the captured amount",
		"",
	}
	for _, message := range notReversed {
		if isAlreadyReversed(errors.New(message)) {
			t.Errorf("wrongly treated %q as already reversed", message)
		}
	}
}
