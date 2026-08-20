// Package eventbus is the Kafka boundary: CloudEvents encoding, the outbox
// relay that publishes, and the consumer that feeds the saga.
package eventbus

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// Topics — docs/CONTRACTS.md §3.1.
const (
	TopicOrderEvents     = "souq.order.events.v1"
	TopicOrderCommands   = "souq.order.commands.v1"
	TopicInventoryEvents = "souq.inventory.events.v1"
	TopicPaymentEvents   = "souq.payment.events.v1"
	TopicNotifications   = "souq.notification.commands.v1"
)

func DLQ(topic string) string { return topic + ".dlq" }

// Envelope is a CloudEvents 1.0 structured message. Every value on every topic
// is one of these; `Data` holds the event-specific payload.
//
// The envelope exists so that a consumer can route, dedup and trace a message
// without knowing anything about its body — which is what lets
// inventory-service skip payment commands on the shared command topic without
// paying to deserialise them.
type Envelope struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	TraceParent     string          `json:"traceparent,omitempty"`
	CorrelationID   string          `json:"correlationid,omitempty"`
	DataSchema      string          `json:"dataschema,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// NewEnvelope builds an envelope around a payload.
//
// The event id is a ULID and it is the dedup key every consumer uses
// (docs/CONTRACTS.md §5.2). It must be generated once, at the moment the
// outbox row is written — never regenerated on a republish, or the inbox
// would treat the retry as a new event and apply the side effect twice.
func NewEnvelope(source, eventType, subject, correlationID, traceParent string, payload any) (Envelope, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	return Envelope{
		SpecVersion:     "1.0",
		ID:              ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String(),
		Source:          source,
		Type:            eventType,
		Subject:         subject,
		Time:            time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		DataContentType: "application/json",
		TraceParent:     traceParent,
		CorrelationID:   correlationID,
		DataSchema:      "https://schemas.souq.dev/" + eventType + ".json",
		Data:            data,
	}, nil
}

// Bind unmarshals the payload into v.
func (e Envelope) Bind(v any) error {
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("unmarshal %s data: %w", e.Type, err)
	}
	return nil
}

// Validate rejects a message that cannot be safely processed. A message
// missing its id cannot be deduplicated, which is worse than dropping it —
// it would be applied on every redelivery forever.
func (e Envelope) Validate() error {
	switch {
	case e.SpecVersion != "1.0":
		return fmt.Errorf("unsupported cloudevents specversion %q", e.SpecVersion)
	case e.ID == "":
		return fmt.Errorf("event has no id and therefore cannot be deduplicated")
	case e.Type == "":
		return fmt.Errorf("event has no type")
	case len(e.Data) == 0:
		return fmt.Errorf("event %s has no data", e.ID)
	}
	return nil
}

// Headers returns the Kafka headers that mirror the envelope. Consumers filter
// on these before deserialising the value, which matters on the shared command
// topic where most messages are for somebody else.
func (e Envelope) Headers() map[string]string {
	h := map[string]string{
		"ce_id":     e.ID,
		"ce_type":   e.Type,
		"ce_source": e.Source,
		"ce_time":   e.Time,
	}
	if e.TraceParent != "" {
		h["traceparent"] = e.TraceParent
	}
	if e.CorrelationID != "" {
		h["correlationid"] = e.CorrelationID
	}
	return h
}
