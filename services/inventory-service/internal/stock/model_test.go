package stock

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	mc "github.com/souq/go-modelcheck"
)

// Exhaustive concurrency model of the single hottest row in the platform.
//
//	stock_levels(sku, on_hand, reserved)
//
// On a flash sale, hundreds of checkout transactions hit this row in the same
// millisecond. Oversell — promising more units than exist — is the most
// expensive bug this system can ship, because it is only discovered at
// fulfilment, after the customer has been charged.
//
// This model enumerates every interleaving of every transaction's steps for
// four candidate implementations, and lets the explorer decide which of them
// are actually safe. Two are not.
//
// # The two invariants, and why one is not enough
//
//	NoOversell    reserved <= on_hand
//	Conservation  reserved == the sum of what every committed transaction took
//
// Conservation is strictly stronger. `naiveAbsolute` below PASSES NoOversell
// and fails Conservation: two reservations collapse into one, the column looks
// perfectly healthy, and the units are physically double-sold. A test suite
// that asserted only NoOversell would have shipped it.
//
// # Relationship to the shipped code
//
// The `atomicUpdate` strategy is what store.TryTake issues. The model reasons
// about the SQL semantics — when the check happens relative to the write —
// rather than calling the function, because the property being checked is a
// property of the database's row latch, not of Go. The empirical counterpart
// is TestNoOversellUnderRealConcurrency in oversell_test.go, which races 50
// real connections against a real Postgres.

type strategy uint8

const (
	// naiveAbsolute: SELECT reserved; check; UPDATE SET reserved = <read> + q.
	// Classic lost update. Does not oversell the column — which is exactly
	// what makes it dangerous.
	naiveAbsolute strategy = iota

	// naiveRelative: SELECT reserved; check; UPDATE SET reserved = reserved + q.
	// What ORMs and hand-written services actually do. The check is atomic
	// with nothing.
	naiveRelative

	// rowLock: SELECT ... FOR UPDATE, then the same logic. Safe, but
	// serialises every buyer of a hot SKU behind one lock, so throughput
	// collapses exactly when it is needed.
	rowLock

	// atomicUpdate: one conditional statement. The database evaluates the
	// predicate and the increment inside the same row latch, so there is no
	// window between them at all. THIS IS WHAT SHIPS.
	atomicUpdate
)

func (s strategy) String() string {
	return [...]string{"naiveAbsolute", "naiveRelative", "rowLock", "atomicUpdate"}[s]
}

// txPhase is where one transaction has got to.
type txPhase uint8

const (
	phaseStart  txPhase = iota
	phaseRead           // has read, has not written
	phaseLocked         // holds the row lock (rowLock only)
	phaseDone           // committed a take
	phaseAborted
)

func (p txPhase) String() string {
	return [...]string{"start", "read", "locked", "done", "aborted"}[p]
}

const maxTxns = 3

type row struct {
	strat  strategy
	onHand int
	demand int // every transaction wants the same amount, which is the contended case

	reserved int
	lock     int8 // index of the lock holder, -1 for none

	phase [maxTxns]txPhase
	// snapshot[i] is what transaction i read. The gap between reading it and
	// acting on it is where the bug lives.
	snapshot [maxTxns]int
	txns     int
}

func (r row) Key() string {
	var b strings.Builder
	fmt.Fprintf(&b, "res=%d lock=%d", r.reserved, r.lock)
	for i := 0; i < r.txns; i++ {
		fmt.Fprintf(&b, " t%d=%s/%d", i, r.phase[i], r.snapshot[i])
	}
	return b.String()
}

// Terminal when every transaction has finished one way or the other.
func (r row) Terminal() bool {
	for i := 0; i < r.txns; i++ {
		if r.phase[i] != phaseDone && r.phase[i] != phaseAborted {
			return false
		}
	}
	return true
}

func (r row) committed() int {
	n := 0
	for i := 0; i < r.txns; i++ {
		if r.phase[i] == phaseDone {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Actions, one set per strategy

func actionsFor(strat strategy, txns int) []mc.Action[row] {
	var actions []mc.Action[row]

	for i := 0; i < txns; i++ {
		i := i

		switch strat {
		case naiveAbsolute, naiveRelative:
			actions = append(actions,
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.SELECT", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseStart {
							return r, false
						}
						r.snapshot[i] = r.reserved
						r.phase[i] = phaseRead
						return r, true
					},
				},
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.UPDATE", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseRead {
							return r, false
						}
						// The check, made on data that may already be stale.
						if r.snapshot[i]+r.demand > r.onHand {
							return r, false
						}
						if r.strat == naiveAbsolute {
							r.reserved = r.snapshot[i] + r.demand // lost update
						} else {
							r.reserved = r.reserved + r.demand // oversell
						}
						r.phase[i] = phaseDone
						return r, true
					},
				},
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.abort", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseRead || r.snapshot[i]+r.demand <= r.onHand {
							return r, false
						}
						r.phase[i] = phaseAborted
						return r, true
					},
				},
			)

		case rowLock:
			actions = append(actions,
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.SELECT FOR UPDATE", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseStart || r.lock != -1 {
							return r, false
						}
						r.lock = int8(i)
						r.snapshot[i] = r.reserved
						r.phase[i] = phaseLocked
						return r, true
					},
				},
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.COMMIT", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseLocked || r.lock != int8(i) {
							return r, false
						}
						// Re-reads under the lock, so the snapshot cannot be stale.
						if r.reserved+r.demand <= r.onHand {
							r.reserved += r.demand
							r.phase[i] = phaseDone
						} else {
							r.phase[i] = phaseAborted
						}
						r.lock = -1
						return r, true
					},
				},
			)

		case atomicUpdate:
			// One statement. No read phase exists to interleave with.
			actions = append(actions,
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.UPDATE...WHERE on_hand-reserved>=q", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseStart || r.onHand-r.reserved < r.demand {
							return r, false
						}
						r.reserved += r.demand
						r.phase[i] = phaseDone
						return r, true
					},
				},
				mc.Action[row]{
					Name: fmt.Sprintf("t%d.rows-affected-0", i),
					Fn: func(r row) (row, bool) {
						if r.phase[i] != phaseStart || r.onHand-r.reserved >= r.demand {
							return r, false
						}
						r.phase[i] = phaseAborted
						return r, true
					},
				},
			)
		}
	}

	return actions
}

var stockInvariants = []mc.Invariant[row]{
	{
		// THE property. Violating it means we sold something we do not have.
		Name: "NoOversell",
		Fn: func(r row) error {
			if r.reserved > r.onHand {
				return fmt.Errorf("reserved=%d exceeds on_hand=%d", r.reserved, r.onHand)
			}
			return nil
		},
	},
	{
		// Strictly stronger, and the one that catches a lost update. The
		// reserved column must equal exactly what the committed transactions
		// took between them.
		Name: "Conservation",
		Fn: func(r row) error {
			want := r.committed() * r.demand
			if r.reserved != want {
				return fmt.Errorf("reserved=%d but %d committed transactions took %d each (=%d)",
					r.reserved, r.committed(), r.demand, want)
			}
			return nil
		},
	},
	{
		Name: "NoLockLeak",
		Fn: func(r row) error {
			if r.lock >= 0 && r.phase[r.lock] != phaseLocked {
				return fmt.Errorf("t%d holds the lock but is %s", r.lock, r.phase[r.lock])
			}
			return nil
		},
	},
}

func modelFor(strat strategy, onHand, demand, txns int) mc.Model[row] {
	init := row{strat: strat, onHand: onHand, demand: demand, lock: -1, txns: txns}
	return mc.Model[row]{
		Initial:    init,
		Actions:    actionsFor(strat, txns),
		Invariants: stockInvariants,
		MaxStates:  200_000,
	}
}

// ---------------------------------------------------------------------------
// The tests

// The shipped strategy, under the worst contention the model can express.
func TestAtomicUpdateNeverOversellsUnderAnyInterleaving(t *testing.T) {
	// Several shapes, because a single (on_hand, demand) pair can hide a bug
	// that only appears when the demand does not divide the stock evenly.
	shapes := []struct{ onHand, demand, txns int }{
		{onHand: 1, demand: 1, txns: 3}, // the last unit, three buyers
		{onHand: 2, demand: 2, txns: 3}, // exactly one can win
		{onHand: 4, demand: 2, txns: 3}, // two win, one loses
		{onHand: 5, demand: 2, txns: 3}, // does not divide evenly
		{onHand: 0, demand: 1, txns: 2}, // nothing in stock at all
	}

	for _, s := range shapes {
		name := fmt.Sprintf("onHand=%d demand=%d txns=%d", s.onHand, s.demand, s.txns)
		t.Run(name, func(t *testing.T) {
			res := mc.Explore(modelFor(atomicUpdate, s.onHand, s.demand, s.txns))
			if !res.OK() {
				t.Fatalf("the shipped strategy is not safe for %s:\n%s", name, res.Report())
			}
			t.Logf("%s: %d states, %d terminal", name, res.StatesExplored, res.TerminalStates)
		})
	}
}

// The regression pin for the oversell bug.
//
// Two buyers, 2 units, 2 each. Both read reserved=0, both pass the check, both
// increment. The row lands at 4. docs/DESIGN-INVARIANTS.md §3a.
func TestNaiveRelativeOversells(t *testing.T) {
	res := mc.Explore(modelFor(naiveRelative, 2, 2, 2))

	if res.Violation == nil {
		t.Fatal("read-modify-write did NOT oversell.\n" +
			"Either the model no longer interleaves the read and the write, or an " +
			"invariant was weakened. See docs/DESIGN-INVARIANTS.md §3a.")
	}
	if res.Violation.Invariant != "NoOversell" {
		t.Errorf("expected NoOversell to catch it, got %s", res.Violation.Invariant)
	}

	t.Logf("the counterexample (%d steps):\n%s",
		len(res.Violation.Trace.Actions), res.Violation.Trace)
}

// The regression pin for the SUBTLE oversell — the one a NoOversell-only test
// misses entirely.
//
// The column stays within on_hand the whole time. Two committed reservations
// collapse into one. The units are double-sold and the row looks healthy.
// docs/DESIGN-INVARIANTS.md §3b.
func TestNaiveAbsoluteLosesUpdatesWithoutOverselling(t *testing.T) {
	res := mc.Explore(modelFor(naiveAbsolute, 2, 1, 2))

	if res.Violation == nil {
		t.Fatal("the lost-update strategy was reported as safe. " +
			"See docs/DESIGN-INVARIANTS.md §3b.")
	}

	// This is the whole point of the test: NoOversell does NOT catch it.
	if res.Violation.Invariant != "Conservation" {
		t.Errorf("expected Conservation to be the invariant that catches this, got %s.\n"+
			"If NoOversell caught it, the model shape changed and this test no longer "+
			"demonstrates why Conservation is needed.", res.Violation.Invariant)
	}

	t.Logf("reserved stayed within on_hand throughout — only Conservation caught it (%d steps):\n%s",
		len(res.Violation.Trace.Actions), res.Violation.Trace)
}

// Row locking is safe. Kept as a checked model because it is the strategy
// someone will reach for when the atomic UPDATE cannot express a requirement,
// and it should be adopted knowing it is correct but slow rather than guessed at.
func TestRowLockIsSafeButSerialises(t *testing.T) {
	res := mc.Explore(modelFor(rowLock, 4, 2, 3))
	if !res.OK() {
		t.Fatalf("SELECT FOR UPDATE should be safe:\n%s", res.Report())
	}

	// The cost, made visible: with a lock, the interleavings collapse. Far
	// fewer reachable states than the lock-free strategy means far less
	// concurrency.
	lockStates := res.StatesExplored
	atomic := mc.Explore(modelFor(atomicUpdate, 4, 2, 3))

	t.Logf("rowLock reaches %d states, atomicUpdate reaches %d — "+
		"the difference is concurrency the lock gives up",
		lockStates, atomic.StatesExplored)
}

// Whatever the strategy, a safe one must let exactly the right number of
// buyers through. Safety without this would be satisfied by refusing everyone.
func TestSafeStrategiesLetTheRightNumberWin(t *testing.T) {
	for _, strat := range []strategy{rowLock, atomicUpdate} {
		t.Run(strat.String(), func(t *testing.T) {
			// 4 units, 2 each, 3 buyers -> exactly 2 must win.
			res := mc.Explore(modelFor(strat, 4, 2, 3))
			if !res.OK() {
				t.Fatalf("not safe:\n%s", res.Report())
			}

			var winnerCounts []int
			for _, r := range res.Reached {
				if r.Terminal() {
					winnerCounts = append(winnerCounts, r.committed())
				}
			}
			sort.Ints(winnerCounts)

			if len(winnerCounts) == 0 {
				t.Fatal("no terminal state was reached")
			}
			for _, n := range winnerCounts {
				if n != 2 {
					t.Errorf("a terminal state had %d winners; with 4 units at 2 each it must be exactly 2", n)
				}
			}
			t.Logf("every one of the %d terminal states had exactly 2 winners", len(winnerCounts))
		})
	}
}
