package modelcheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A tiny model used to prove the explorer itself works. A model checker that
// cannot find a bug it was pointed at is worse than no model checker, so this
// file exists to show that every failure mode it claims to detect is actually
// detected.

type counter struct {
	n      int
	capped bool
	stuck  bool
}

func (c counter) Key() string    { return fmt.Sprintf("n=%d capped=%v stuck=%v", c.n, c.capped, c.stuck) }
func (c counter) Terminal() bool { return c.capped }

func inc(limit int) Action[counter] {
	return Action[counter]{
		Name: "inc",
		Fn: func(c counter) (counter, bool) {
			if c.capped || c.stuck || c.n >= limit {
				return c, false
			}
			c.n++
			return c, true
		},
	}
}

var finish = Action[counter]{
	Name: "finish",
	Fn: func(c counter) (counter, bool) {
		if c.capped || c.stuck || c.n < 3 {
			return c, false
		}
		c.capped = true
		return c, true
	},
}

func TestFindsAViolationAndReportsTheShortestPath(t *testing.T) {
	res := Explore(Model[counter]{
		Initial: counter{},
		Actions: []Action[counter]{inc(10), finish},
		Invariants: []Invariant[counter]{{
			Name: "n stays below 5",
			Fn: func(c counter) error {
				if c.n >= 5 {
					return fmt.Errorf("n reached %d", c.n)
				}
				return nil
			},
		}},
	})

	if res.Violation == nil {
		t.Fatal("the explorer did not find a violation it was pointed straight at")
	}
	// BFS guarantees shortest. Five increments is the only way to reach 5.
	if len(res.Violation.Trace.Actions) != 5 {
		t.Errorf("counterexample is %d steps, want the shortest (5): %v",
			len(res.Violation.Trace.Actions), res.Violation.Trace.Actions)
	}
	if !strings.Contains(res.Report(), "INVARIANT VIOLATED") {
		t.Error("the report does not name the violation")
	}
}

func TestPassesWhenNothingIsWrong(t *testing.T) {
	res := Explore(Model[counter]{
		Initial: counter{},
		Actions: []Action[counter]{inc(4), finish},
		Invariants: []Invariant[counter]{{
			Name: "n stays at or below 4",
			Fn: func(c counter) error {
				if c.n > 4 {
					return errors.New("overflow")
				}
				return nil
			},
		}},
	})

	if !res.OK() {
		t.Fatalf("a sound model was reported as broken:\n%s", res.Report())
	}
	if res.TerminalStates == 0 {
		t.Error("no terminal state was reached; the model cannot finish")
	}
}

func TestDetectsADeadlock(t *testing.T) {
	// `stick` leads to a non-terminal state with nothing enabled.
	stick := Action[counter]{
		Name: "stick",
		Fn: func(c counter) (counter, bool) {
			if c.stuck || c.capped {
				return c, false
			}
			c.stuck = true
			return c, true
		},
	}

	res := Explore(Model[counter]{
		Initial: counter{},
		Actions: []Action[counter]{inc(3), finish, stick},
	})

	if len(res.Deadlocks) == 0 {
		t.Fatal("a state with no enabled action and no terminal flag was not reported")
	}
}

// The property that matters most for a saga: a state that keeps taking steps
// but can never finish. It is not a deadlock — actions stay enabled — so a
// deadlock check alone misses it entirely.
func TestDetectsAWedgedState(t *testing.T) {
	// Once `derail` fires, inc still works but finish never can.
	derail := Action[counter]{
		Name: "derail",
		Fn: func(c counter) (counter, bool) {
			if c.n != 1 || c.capped {
				return c, false
			}
			c.n = 100 // past the limit finish requires, but inc(200) still runs
			return c, true
		},
	}
	incHigh := Action[counter]{
		Name: "inc-high",
		Fn: func(c counter) (counter, bool) {
			if c.capped || c.n < 100 || c.n >= 103 {
				return c, false
			}
			c.n++
			return c, true
		},
	}
	finishLow := Action[counter]{
		Name: "finish-low",
		Fn: func(c counter) (counter, bool) {
			// Only reachable below 100, so the derailed branch can never finish.
			if c.capped || c.n < 2 || c.n >= 100 {
				return c, false
			}
			c.capped = true
			return c, true
		},
	}

	res := Explore(Model[counter]{
		Initial: counter{},
		Actions: []Action[counter]{inc(5), derail, incHigh, finishLow},
	})

	if len(res.Wedged) == 0 {
		t.Fatal("a branch from which no terminal state is reachable was not reported")
	}
	if !strings.Contains(res.Report(), "WEDGED") {
		t.Error("the report does not mention the wedged states")
	}
}

// A model whose Key() includes something that always changes never terminates.
// Reporting that clearly beats running out of memory.
func TestBoundsRunawayStateSpaces(t *testing.T) {
	res := Explore(Model[counter]{
		Initial:   counter{},
		Actions:   []Action[counter]{inc(1_000_000)},
		MaxStates: 500,
	})

	if res.Violation == nil {
		t.Fatal("an unbounded state space was not caught")
	}
	if !strings.Contains(res.Violation.Detail.Error(), "Key()") {
		t.Errorf("the error does not point at the likely cause: %v", res.Violation.Detail)
	}
}

func TestChecksTheInitialState(t *testing.T) {
	res := Explore(Model[counter]{
		Initial: counter{n: 99},
		Actions: []Action[counter]{inc(100)},
		Invariants: []Invariant[counter]{{
			Name: "n starts at zero",
			Fn: func(c counter) error {
				if c.n != 0 {
					return fmt.Errorf("n is %d", c.n)
				}
				return nil
			},
		}},
	})

	if res.Violation == nil {
		t.Fatal("a violation present in the INITIAL state was not caught")
	}
	if len(res.Violation.Trace.Actions) != 0 {
		t.Error("the counterexample should be zero steps long")
	}
}
