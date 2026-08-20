// Package domain holds the order aggregate and the value objects it is made
// of. It has no dependency on the database, on Kafka, or on HTTP — everything
// in here can be constructed and asserted on in a unit test with no fixtures.
package domain

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/souq/order-service/internal/saga"
)

// ---------------------------------------------------------------------------
// Identifiers

// NewID mints a prefixed ULID: sortable by creation time, unambiguous to read
// aloud, and self-describing enough that a stray id in the wrong field fails
// validation at the edge instead of 404-ing three services deep.
func NewID(prefix string) string {
	return prefix + "_" + ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// ValidID checks the shape without allocating. Crockford base32 excludes
// I, L, O and U precisely so a human transcribing an id cannot go wrong.
func ValidID(prefix, id string) bool {
	want := len(prefix) + 1 + 26
	if len(id) != want || !strings.HasPrefix(id, prefix+"_") {
		return false
	}
	for _, r := range id[len(prefix)+1:] {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z' && r != 'I' && r != 'L' && r != 'O' && r != 'U':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Money

// Money is minor units plus an ISO-4217 code. There is no float anywhere in
// this platform (docs/CONTRACTS.md §2.5): 0.1 + 0.2 != 0.3, and a cart total
// that is off by a cent is a support ticket today and an audit finding later.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

var ErrCurrencyMismatch = errors.New("currency mismatch")

func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return Money{Amount: m.Amount + o.Amount, Currency: m.Currency}, nil
}

func (m Money) Mul(qty int) Money {
	return Money{Amount: m.Amount * int64(qty), Currency: m.Currency}
}

func (m Money) IsZero() bool       { return m.Amount == 0 }
func (m Money) Equal(o Money) bool { return m.Amount == o.Amount && m.Currency == o.Currency }

func (m Money) String() string {
	return fmt.Sprintf("%d.%02d %s", m.Amount/100, abs(m.Amount%100), m.Currency)
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------------------------------------------------------------------------
// Address

type Address struct {
	Recipient   string `json:"recipient"`
	Line1       string `json:"line1"`
	Line2       string `json:"line2,omitempty"`
	City        string `json:"city"`
	Region      string `json:"region,omitempty"`
	PostalCode  string `json:"postalCode"`
	CountryCode string `json:"countryCode"`
	Phone       string `json:"phone,omitempty"`
}

func (a Address) Validate() error {
	switch {
	case a.Recipient == "":
		return errors.New("recipient is required")
	case a.Line1 == "":
		return errors.New("line1 is required")
	case a.City == "":
		return errors.New("city is required")
	case a.PostalCode == "":
		return errors.New("postalCode is required")
	case len(a.CountryCode) != 2 || strings.ToUpper(a.CountryCode) != a.CountryCode:
		return errors.New("countryCode must be ISO-3166-1 alpha-2, uppercase")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Order aggregate

type OrderItem struct {
	LineNo    int    `json:"lineNo"`
	SKU       string `json:"sku"`
	ProductID string `json:"productId"`
	Title     string `json:"title"`
	ImageURL  string `json:"image,omitempty"`
	Quantity  int    `json:"quantity"`
	UnitPrice Money  `json:"unitPrice"`
	LineTotal Money  `json:"lineTotal"`
}

type Order struct {
	ID     string     `json:"id"`
	UserID string     `json:"userId"`
	Status saga.State `json:"status"`

	Items []OrderItem `json:"lines"`

	Subtotal      Money `json:"subtotal"`
	DiscountTotal Money `json:"discountTotal"`
	ShippingTotal Money `json:"shippingTotal"`
	TaxTotal      Money `json:"taxTotal"`
	Total         Money `json:"total"`

	ShippingAddress Address  `json:"shippingAddress"`
	BillingAddress  *Address `json:"billingAddress"`

	PaymentID          string `json:"paymentId,omitempty"`
	ReservationID      string `json:"reservationId,omitempty"`
	PaymentMethodToken string `json:"-"` // never serialised outward

	CancellationReason saga.CancelReason `json:"cancellationReason,omitempty"`
	FailedStep         saga.Step         `json:"-"`
	TrackingNumber     string            `json:"trackingNumber,omitempty"`

	// Pricing rule set the totals were computed against. Without it the order
	// cannot be re-priced identically at capture time, and a promotion that
	// expires mid-checkout silently changes what the customer agreed to.
	RulesVersion string `json:"rulesVersion"`

	CorrelationID  string `json:"-"`
	IdempotencyKey string `json:"-"`

	PlacedAt  time.Time `json:"placedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Version   int       `json:"-"`
}

// RecomputeTotal derives the grand total from the parts. Called before
// persisting so a caller-supplied total can never disagree with the lines.
func (o *Order) RecomputeTotal() error {
	sub := Money{Currency: o.Subtotal.Currency}
	for _, it := range o.Items {
		lt := it.UnitPrice.Mul(it.Quantity)
		if !lt.Equal(it.LineTotal) {
			return fmt.Errorf("line %d: lineTotal %s does not equal unitPrice x quantity %s",
				it.LineNo, it.LineTotal, lt)
		}
		var err error
		if sub, err = sub.Add(lt); err != nil {
			return err
		}
	}
	if !sub.Equal(o.Subtotal) {
		return fmt.Errorf("subtotal %s does not equal the sum of lines %s", o.Subtotal, sub)
	}

	total := sub
	for _, part := range []Money{o.DiscountTotal, o.ShippingTotal, o.TaxTotal} {
		var err error
		if total, err = total.Add(part); err != nil {
			return err
		}
	}
	if total.Amount < 0 {
		return fmt.Errorf("computed total is negative: %s", total)
	}
	o.Total = total
	return nil
}

// Cancellable reports whether a customer may still cancel this order
// themselves. Past the point of no return the answer is no, and support has
// to issue a refund instead — see docs/DESIGN-INVARIANTS.md §1.
func (o *Order) Cancellable() bool {
	return o.Status == saga.StatePending || o.Status == saga.StateStockReserved
}
