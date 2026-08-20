// Package events builds CloudEvents envelopes and writes them to the outbox.
//
// Every constructor here returns a store.OutboxRecord and every caller passes
// it to Enqueue with a transaction. There is no path that publishes directly.
package events

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"github.com/souq/inventory-service/internal/store"
)

const (
	TopicInventoryEvents = "souq.inventory.events.v1"
	Source               = "souq/inventory-service"
)

type UnavailableSKU struct {
	SKU       string `json:"sku"`
	Requested int    `json:"requested"`
	Available int    `json:"available"`
}

// build wraps a payload in the CloudEvents envelope.
//
// The event id is generated exactly once, here, and stored. It is the dedup
// key every consumer inboxes on; regenerating it on a republish would make the
// retry look like a new event and the side effect would run twice.
func build(eventType, aggregateID, partitionKey, correlationID, aggregateType string, data any) store.OutboxRecord {
	eventID := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	envelope := map[string]any{
		"specversion":     "1.0",
		"id":              eventID,
		"source":          Source,
		"type":            eventType,
		"subject":         aggregateID,
		"time":            now,
		"datacontenttype": "application/json",
		"correlationid":   correlationID,
		"dataschema":      "https://schemas.souq.dev/" + eventType + ".json",
		"data":            data,
	}
	payload, _ := json.Marshal(envelope)

	return store.OutboxRecord{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		EventID:       eventID,
		EventType:     eventType,
		Topic:         TopicInventoryEvents,
		PartitionKey:  partitionKey,
		Payload:       payload,
		Headers: map[string]string{
			"ce_id": eventID, "ce_type": eventType, "ce_source": Source,
			"ce_time": now, "correlationid": correlationID,
		},
	}
}

// Reserved and friends are keyed by ORDER, not by SKU.
//
// Per-key ordering is the only ordering Kafka gives, and the saga needs its own
// events ordered relative to each other far more than it needs one SKU's
// events ordered across unrelated orders.
type Item struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

func Reserved(orderID, reservationID string, items []Item, expiresAt time.Time, correlationID string) store.OutboxRecord {
	return build("souq.inventory.reserved.v1", reservationID, orderID, correlationID, "reservation",
		map[string]any{
			"orderId": orderID, "reservationId": reservationID,
			"expiresAt": expiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			"items":     items,
		})
}

func ReservationFailed(orderID, reservationID, reasonCode string, unavailable []UnavailableSKU, correlationID string) store.OutboxRecord {
	return build("souq.inventory.reservation_failed.v1", reservationID, orderID, correlationID, "reservation",
		map[string]any{
			"orderId": orderID, "reservationId": reservationID,
			"reasonCode": reasonCode, "unavailable": unavailable,
		})
}

func Released(orderID, reservationID string, wasTombstone bool, correlationID string) store.OutboxRecord {
	return build("souq.inventory.released.v1", reservationID, orderID, correlationID, "reservation",
		map[string]any{
			"orderId": orderID, "reservationId": reservationID,
			// Tells the saga, and anyone reading the log, that this release
			// compensated something that had not happened yet.
			"wasTombstone": wasTombstone,
		})
}

func Committed(orderID, reservationID, correlationID string) store.OutboxRecord {
	return build("souq.inventory.committed.v1", reservationID, orderID, correlationID, "reservation",
		map[string]any{"orderId": orderID, "reservationId": reservationID})
}

// StockChanged is keyed by SKU: it is a fact about a product, and its consumers
// (the search index, the low-stock alert, the PDP badge) care about per-SKU
// ordering.
func StockChanged(sku string, onHand, reserved int, reasonCode, correlationID string) store.OutboxRecord {
	available := onHand - reserved
	if available < 0 {
		available = 0
	}
	return build("souq.inventory.stock_changed.v1", sku, sku, correlationID, "stock",
		map[string]any{
			"sku": sku, "onHand": onHand, "reserved": reserved,
			"available": available, "reasonCode": reasonCode,
		})
}

func Enqueue(ctx context.Context, tx pgx.Tx, r store.OutboxRecord) error {
	if err := store.EnqueueOutbox(ctx, tx, r); err != nil {
		return fmt.Errorf("enqueue %s: %w", r.EventType, err)
	}
	return nil
}
