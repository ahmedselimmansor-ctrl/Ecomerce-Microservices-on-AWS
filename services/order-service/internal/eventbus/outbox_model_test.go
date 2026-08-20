package eventbus_test

import (
	"fmt"
	"testing"

	mc "github.com/souq/go-modelcheck"
)

// Exhaustive model of event delivery from Postgres to Kafka.
//
// "Write to the database, then publish to Kafka" is what almost every service
// does on almost every request, and it is wrong. There is no transaction
// spanning the two, so a crash in the gap either loses the event (committed,
// never published) or invents one (published, then rolled back). At 10k
// orders/hour, "rare" means several times a day.
//
// This model checks the three-part mechanism docs/CONTRACTS.md §5.1–5.2
// mandates, and — more usefully — shows what each part is individually buying
// by removing it and watching the model break.
//
//	1. TRANSACTIONAL OUTBOX   the event row is written in the SAME transaction
//	                          as the business row. Buys: no lost events, no
//	                          phantom events.
//	2. RELAY                  a poller publishes pending rows and marks them.
//	                          It can crash between the two, so it is
//	                          at-least-once, never exactly-once. Buys:
//	                          eventual delivery.
//	3. CONSUMER INBOX         processed_events dedups on event_id. Buys: the
//	                          duplicates from (2) become harmless.
//
// Together: effectively-once. Remove any one and the model finds a
// counterexample. docs/DESIGN-INVARIANTS.md §5.

type mode uint8

const (
	// dualWrite: COMMIT, then producer.send(). Two atomicity domains.
	dualWrite mode = iota
	// outbox: the event row commits with the business row.
	outbox
)

type delivery struct {
	mode mode

	// Whether the consumer keeps a processed_events row. Turning this off is
	// §5b.
	consumerDedups bool
	// Whether a relay exists at all. Turning this off is §5c.
	relayRuns bool
	// Whether the producer may die in the gap.
	crashAllowed bool

	committed  bool  // the business row is durable
	outboxRow  uint8 // 0 none, 1 pending, 2 published
	inBroker   bool
	deliveries uint8 // how many times the consumer has been handed it
	consumed   uint8
	processed  bool  // the inbox row exists
	applied    uint8 // how many times the SIDE EFFECT ran. THE number.
	crashed    bool
}

const maxDeliveries = 3

func (d delivery) Key() string {
	return fmt.Sprintf("c=%v ob=%d br=%v del=%d con=%d proc=%v app=%d crash=%v",
		d.committed, d.outboxRow, d.inBroker, d.deliveries, d.consumed,
		d.processed, d.applied, d.crashed)
}

// Terminal when the event has been applied and there is nothing left in flight.
func (d delivery) Terminal() bool {
	return d.applied >= 1 && d.consumed == d.deliveries && d.outboxRow != 1
}

// ---------------------------------------------------------------------------
// Producer: dual write

var dualCommit = mc.Action[delivery]{
	Name: "dual.COMMIT",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != dualWrite || d.crashed || d.committed {
			return d, false
		}
		d.committed = true
		return d, true
	},
}

var dualPublish = mc.Action[delivery]{
	Name: "dual.producer.send",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != dualWrite || d.crashed || !d.committed || d.inBroker {
			return d, false
		}
		d.inBroker = true
		return d, true
	},
}

// The gap. The order exists; the world will never hear about it.
var dualCrash = mc.Action[delivery]{
	Name: "dual.CRASH-IN-THE-GAP",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != dualWrite || !d.crashAllowed || d.crashed || !d.committed || d.inBroker {
			return d, false
		}
		d.crashed = true
		return d, true
	},
}

// ---------------------------------------------------------------------------
// Producer: outbox

// One atomic step: BEGIN; INSERT order; INSERT outbox; COMMIT.
var outboxCommit = mc.Action[delivery]{
	Name: "outbox.COMMIT(order+event)",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != outbox || d.committed {
			return d, false
		}
		d.committed = true
		d.outboxRow = 1 // pending
		return d, true
	},
}

// The relay: SELECT ... WHERE published_at IS NULL FOR UPDATE SKIP LOCKED.
//
// Enabled repeatedly while the row is still pending — that IS the
// crash-after-publish-before-mark path, and it is why duplicates are a
// guarantee of this design rather than an accident of it.
var relayPublish = mc.Action[delivery]{
	Name: "relay.publish",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != outbox || !d.relayRuns || d.outboxRow != 1 || d.deliveries >= maxDeliveries {
			return d, false
		}
		d.inBroker = true
		d.deliveries++
		return d, true
	},
}

var relayMark = mc.Action[delivery]{
	Name: "relay.mark-published",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != outbox || !d.relayRuns || d.outboxRow != 1 || !d.inBroker {
			return d, false
		}
		d.outboxRow = 2
		return d, true
	},
}

// ---------------------------------------------------------------------------
// Consumer

var dualDeliver = mc.Action[delivery]{
	Name: "kafka.deliver",
	Fn: func(d delivery) (delivery, bool) {
		if d.mode != dualWrite || !d.inBroker || d.deliveries >= maxDeliveries {
			return d, false
		}
		d.deliveries++
		return d, true
	},
}

// INSERT INTO processed_events (event_id) ON CONFLICT DO NOTHING;
// rows affected = 0 means somebody already handled it.
var consumeFirst = mc.Action[delivery]{
	Name: "consumer.apply",
	Fn: func(d delivery) (delivery, bool) {
		if d.consumed >= d.deliveries {
			return d, false
		}
		if d.consumerDedups && d.processed {
			return d, false
		}
		d.consumed++
		d.processed = true
		d.applied++
		return d, true
	},
}

var consumeDuplicate = mc.Action[delivery]{
	Name: "consumer.ack-duplicate",
	Fn: func(d delivery) (delivery, bool) {
		if !d.consumerDedups || d.consumed >= d.deliveries || !d.processed {
			return d, false
		}
		d.consumed++ // ack the offset, do nothing else
		return d, true
	},
}

// ---------------------------------------------------------------------------

var deliveryInvariants = []mc.Invariant[delivery]{
	{
		// No duplicate side effects, however many times Kafka redelivers.
		Name: "AtMostOnceEffect",
		Fn: func(d delivery) error {
			if d.applied > 1 {
				return fmt.Errorf("the side effect ran %d times", d.applied)
			}
			return nil
		},
	},
	{
		// No phantom: never act on an event whose transaction did not commit.
		Name: "NoPhantomEvent",
		Fn: func(d delivery) error {
			if d.applied >= 1 && !d.committed {
				return fmt.Errorf("applied an event for a transaction that never committed")
			}
			return nil
		},
	},
}

func deliveryActions() []mc.Action[delivery] {
	return []mc.Action[delivery]{
		dualCommit, dualPublish, dualCrash, dualDeliver,
		outboxCommit, relayPublish, relayMark,
		consumeFirst, consumeDuplicate,
	}
}

// ---------------------------------------------------------------------------

// The shipped design: all three parts present.
//
// The liveness check is what proves NoLostEvent — a durable business row from
// which no terminal state (applied == 1) is reachable would be reported as
// wedged.
func TestOutboxDeliveryIsEffectivelyOnce(t *testing.T) {
	res := mc.Explore(mc.Model[delivery]{
		Initial: delivery{
			mode: outbox, consumerDedups: true, relayRuns: true, crashAllowed: true,
		},
		Actions:    deliveryActions(),
		Invariants: deliveryInvariants,
	})

	if !res.OK() {
		t.Fatalf("outbox + relay + inbox is not effectively-once:\n%s", res.Report())
	}
	t.Logf("%s", res.Report())

	// Coverage: the duplicate path must actually have been exercised, or the
	// inbox is not being tested at all.
	sawDuplicate := false
	for _, d := range res.Reached {
		if d.deliveries > 1 && d.applied == 1 {
			sawDuplicate = true
		}
	}
	if !sawDuplicate {
		t.Error("the model never redelivered an event, so the inbox was never exercised")
	}
}

// §5a. The consumer here is perfectly well behaved and dedups correctly. It
// makes no difference: the event never existed.
func TestDualWriteLosesEvents(t *testing.T) {
	res := mc.Explore(mc.Model[delivery]{
		Initial: delivery{
			mode: dualWrite, consumerDedups: true, relayRuns: true, crashAllowed: true,
		},
		Actions:    deliveryActions(),
		Invariants: deliveryInvariants,
	})

	// Not a safety violation — nothing bad was DONE. The failure is liveness:
	// a state where the business fact is durable and no terminal state is
	// reachable.
	if len(res.Wedged) == 0 && len(res.Deadlocks) == 0 {
		t.Fatal("commit-then-publish did NOT lose an event.\n" +
			"Either the crash action is no longer reachable or the liveness check is broken.\n" +
			"See docs/DESIGN-INVARIANTS.md §5a.")
	}

	// Confirm it is the crash-in-the-gap state specifically.
	found := false
	for _, w := range res.Wedged {
		last := w.States[len(w.States)-1]
		if last.committed && !last.inBroker {
			found = true
			t.Logf("the lost-event trace (%d steps):\n%s", len(w.Actions), w)
			break
		}
	}
	for _, dl := range res.Deadlocks {
		last := dl.States[len(dl.States)-1]
		if last.committed && !last.inBroker {
			found = true
			t.Logf("the lost-event trace (%d steps):\n%s", len(dl.Actions), dl)
			break
		}
	}
	if !found {
		t.Errorf("expected a committed-but-never-published state, got: %s", res.Report())
	}
}

// §5b. Correct producer, no consumer inbox. The relay's
// crash-after-publish-before-mark path is a guarantee, not an edge case.
func TestWithoutTheInboxTheSideEffectRunsTwice(t *testing.T) {
	res := mc.Explore(mc.Model[delivery]{
		Initial: delivery{
			mode: outbox, consumerDedups: false, relayRuns: true, crashAllowed: true,
		},
		Actions:    deliveryActions(),
		Invariants: deliveryInvariants,
	})

	if res.Violation == nil {
		t.Fatal("removing the consumer inbox did NOT produce a duplicate side effect.\n" +
			"See docs/DESIGN-INVARIANTS.md §5b.")
	}
	if res.Violation.Invariant != "AtMostOnceEffect" {
		t.Errorf("expected AtMostOnceEffect to catch it, got %s", res.Violation.Invariant)
	}

	t.Logf("two reservations for one order (%d steps):\n%s",
		len(res.Violation.Trace.Actions), res.Violation.Trace)
}

// §5c. An outbox row nothing drains is a durable event nobody receives.
func TestWithoutTheRelayNothingIsEverDelivered(t *testing.T) {
	res := mc.Explore(mc.Model[delivery]{
		Initial: delivery{
			mode: outbox, consumerDedups: true, relayRuns: false, crashAllowed: false,
		},
		Actions:    deliveryActions(),
		Invariants: deliveryInvariants,
	})

	if len(res.Wedged) == 0 && len(res.Deadlocks) == 0 {
		t.Fatal("removing the relay did NOT strand the event. " +
			"See docs/DESIGN-INVARIANTS.md §5c.")
	}
	t.Logf("without a relay: %d wedged, %d deadlocked — the row sits pending forever",
		len(res.Wedged), len(res.Deadlocks))
}
