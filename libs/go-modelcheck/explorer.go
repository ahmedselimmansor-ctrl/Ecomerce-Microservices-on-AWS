// Package modelcheck is an exhaustive state-space explorer.
//
// It exists to answer the question a unit test cannot: "is there ANY
// interleaving of these actions that reaches a bad state?" A unit test checks
// the orderings you thought of. This enumerates all of them.
//
// # What it does
//
// Given an initial state and a set of actions, it computes the full set of
// reachable states by breadth-first search, checking every invariant at every
// state. On a violation it reports the SHORTEST sequence of actions that
// produced it, which is the difference between a useful counterexample and a
// 200-step trace nobody reads.
//
// It also checks a liveness property that matters for sagas: from every
// reachable state, can a terminal state still be reached? A system that can
// wedge in some corner of its state space passes every safety check and still
// strands customers.
//
// # What it is not
//
// This is not a theorem prover. It exhausts a FINITE model — two orders, three
// transactions — and says nothing about arbitrary N. That is a real limit and
// it is stated wherever a result from this package is quoted.
//
// It is also a model of the DESIGN, not of the code. The bridge is that each
// model here drives the same pure decision function the production code calls,
// so a divergence between them is a compile error rather than a documentation
// drift.
//
// # Why this rather than a dedicated model checker
//
// A separate specification language proves more and proves it better. It also
// needs its own toolchain in CI, its own reviewers, and a bridge to the code
// that nothing enforces. Expressing the model in Go against the real decision
// function means it cannot drift, every engineer can read and change it, and
// it runs in the same `go test` as everything else.
package modelcheck

import (
	"fmt"
	"sort"
	"strings"
)

// State is a point in the model. Implementations must be value types with no
// pointers or maps reachable from them, because the explorer relies on Key()
// being a faithful identity — two states with the same Key are treated as the
// same state and one of them is not explored.
type State interface {
	// Key uniquely identifies this state. Two states with equal keys must be
	// genuinely interchangeable: if a field affects behaviour, it belongs in
	// the key, or the explorer will silently prune a distinct branch.
	Key() string

	// Terminal reports whether the model has finished. Used by the liveness
	// check and to stop exploring past the end.
	Terminal() bool
}

// Action is one possible step. It returns the resulting state and whether it
// was enabled — an action that is not enabled in a given state returns false
// and is skipped.
//
// Actions must be deterministic. Nondeterminism is expressed by having several
// actions enabled in the same state, not by one action behaving differently on
// different calls; otherwise the search is not reproducible and a
// counterexample cannot be replayed.
type Action[S State] struct {
	Name string
	Fn   func(S) (S, bool)
}

// Invariant is a property that must hold in every reachable state. Returning
// an error fails the check and the message becomes part of the counterexample.
type Invariant[S State] struct {
	Name string
	Fn   func(S) error
}

// Model is what gets explored.
type Model[S State] struct {
	Initial    S
	Actions    []Action[S]
	Invariants []Invariant[S]

	// MaxStates bounds the search. A model that blows past this has a bug in
	// its Key() — usually a monotonically increasing counter that makes every
	// state distinct — and reporting that is more useful than running out of
	// memory.
	MaxStates int

	// MaxDepth bounds path length. Zero means unbounded.
	MaxDepth int
}

// Result is what the exploration found.
type Result[S State] struct {
	StatesExplored int
	MaxDepthSeen   int
	TerminalStates int

	// Violation is non-nil when an invariant failed.
	Violation *Violation[S]

	// Deadlocks are non-terminal states with no enabled action. For a saga
	// these are always bugs: an order sitting in a state nothing can move it
	// out of.
	Deadlocks []Trace[S]

	// Wedged are reachable states from which NO terminal state is reachable.
	// Strictly worse than a deadlock, because the model keeps taking steps and
	// looks alive while making no progress towards finishing.
	Wedged []Trace[S]

	// Reached is every distinct state the search visited.
	//
	// Exposed so a caller can assert COVERAGE rather than a state count. A
	// green model that explored the wrong thing is worse than a red one, and
	// a guard that is accidentally always false produces a small, clean,
	// meaningless result that no count threshold will catch.
	Reached []S
}

type Violation[S State] struct {
	Invariant string
	Detail    error
	Trace     Trace[S]
}

// Trace is a path from the initial state, recorded as the action names taken
// and the states passed through.
type Trace[S State] struct {
	Actions []string
	States  []S
}

func (t Trace[S]) String() string {
	var b strings.Builder
	for i, a := range t.Actions {
		fmt.Fprintf(&b, "%2d. %-28s -> %s\n", i+1, a, t.States[i+1].Key())
	}
	return b.String()
}

// Explore runs the search.
//
// Breadth-first, so the first violation found is via a shortest path. That
// matters more than it sounds: a depth-first search on a saga model happily
// returns a 40-step trace through five redeliveries when a 6-step one exists,
// and nobody debugs the 40-step one.
func Explore[S State](m Model[S]) Result[S] {
	if m.MaxStates == 0 {
		m.MaxStates = 500_000
	}

	var res Result[S]

	// key -> how we first reached it. Shortest path by construction, because
	// BFS reaches every state at its minimum depth.
	seen := map[string]Trace[S]{}

	initialTrace := Trace[S]{Actions: nil, States: []S{m.Initial}}
	seen[m.Initial.Key()] = initialTrace

	// Check the initial state too. A model whose initial state already
	// violates an invariant is a real and easily-missed mistake.
	if v := checkInvariants(m, m.Initial, initialTrace); v != nil {
		res.Violation = v
		res.StatesExplored = 1
		return res
	}

	// The full successor relation, kept so the liveness pass does not have to
	// re-run every action.
	successors := map[string][]string{}

	queue := []Trace[S]{initialTrace}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		state := cur.States[len(cur.States)-1]
		depth := len(cur.Actions)

		if depth > res.MaxDepthSeen {
			res.MaxDepthSeen = depth
		}
		if state.Terminal() {
			res.TerminalStates++
		}
		if m.MaxDepth > 0 && depth >= m.MaxDepth {
			continue
		}

		enabled := 0
		for _, action := range m.Actions {
			next, ok := action.Fn(state)
			if !ok {
				continue
			}
			// A self-loop is not progress. Counting it as an enabled action
			// would hide a deadlock behind an action that does nothing.
			if next.Key() == state.Key() {
				continue
			}
			enabled++

			successors[state.Key()] = append(successors[state.Key()], next.Key())

			if _, visited := seen[next.Key()]; visited {
				continue
			}

			trace := Trace[S]{
				Actions: append(append([]string{}, cur.Actions...), action.Name),
				States:  append(append([]S{}, cur.States...), next),
			}
			seen[next.Key()] = trace

			if v := checkInvariants(m, next, trace); v != nil {
				res.Violation = v
				res.StatesExplored = len(seen)
				return res
			}

			queue = append(queue, trace)

			if len(seen) >= m.MaxStates {
				res.StatesExplored = len(seen)
				res.Violation = &Violation[S]{
					Invariant: "state-space bound",
					Detail: fmt.Errorf(
						"explored %d states without finishing; the model's Key() is probably "+
							"including a value that always changes (a counter, a timestamp)",
						len(seen)),
					Trace: trace,
				}
				return res
			}
		}

		if enabled == 0 && !state.Terminal() {
			res.Deadlocks = append(res.Deadlocks, cur)
		}
	}

	res.StatesExplored = len(seen)

	res.Reached = make([]S, 0, len(seen))
	for _, trace := range seen {
		res.Reached = append(res.Reached, trace.States[len(trace.States)-1])
	}

	// Liveness: from every reachable state, is a terminal state still
	// reachable? Computed by working backwards from the terminal states over
	// the successor relation, which is linear rather than re-searching from
	// each state.
	res.Wedged = findWedged(seen, successors)

	return res
}

func checkInvariants[S State](m Model[S], s S, trace Trace[S]) *Violation[S] {
	for _, inv := range m.Invariants {
		if err := inv.Fn(s); err != nil {
			return &Violation[S]{Invariant: inv.Name, Detail: err, Trace: trace}
		}
	}
	return nil
}

// findWedged returns the states from which no terminal state is reachable.
//
// Backwards reachability: start from the terminal states, repeatedly add any
// state with an edge into the set. Whatever is left over cannot finish.
func findWedged[S State](seen map[string]Trace[S], successors map[string][]string) []Trace[S] {
	canFinish := map[string]bool{}

	for key, trace := range seen {
		if trace.States[len(trace.States)-1].Terminal() {
			canFinish[key] = true
		}
	}

	// Predecessor index, so each round is a scan of edges rather than of
	// states x edges.
	predecessors := map[string][]string{}
	for from, tos := range successors {
		for _, to := range tos {
			predecessors[to] = append(predecessors[to], from)
		}
	}

	frontier := make([]string, 0, len(canFinish))
	for k := range canFinish {
		frontier = append(frontier, k)
	}

	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		for _, pred := range predecessors[cur] {
			if !canFinish[pred] {
				canFinish[pred] = true
				frontier = append(frontier, pred)
			}
		}
	}

	var wedged []Trace[S]
	for key, trace := range seen {
		if !canFinish[key] {
			wedged = append(wedged, trace)
		}
	}

	// Shortest first, and stable, so a failure message does not change
	// between runs on an unrelated edit.
	sort.Slice(wedged, func(i, j int) bool {
		if len(wedged[i].Actions) != len(wedged[j].Actions) {
			return len(wedged[i].Actions) < len(wedged[j].Actions)
		}
		return wedged[i].States[len(wedged[i].States)-1].Key() <
			wedged[j].States[len(wedged[j].States)-1].Key()
	})

	return wedged
}

// Report renders a result for a test failure message.
func (r Result[S]) Report() string {
	var b strings.Builder

	fmt.Fprintf(&b, "explored %d states, max depth %d, %d terminal\n",
		r.StatesExplored, r.MaxDepthSeen, r.TerminalStates)

	if r.Violation != nil {
		fmt.Fprintf(&b, "\nINVARIANT VIOLATED: %s\n  %v\n\nshortest counterexample (%d steps):\n\n%s",
			r.Violation.Invariant, r.Violation.Detail,
			len(r.Violation.Trace.Actions), r.Violation.Trace)
	}

	if len(r.Deadlocks) > 0 {
		fmt.Fprintf(&b, "\n%d DEADLOCK(S) — a non-terminal state with nothing enabled\n\n%s",
			len(r.Deadlocks), r.Deadlocks[0])
	}

	if len(r.Wedged) > 0 {
		fmt.Fprintf(&b, "\n%d WEDGED STATE(S) — reachable, but no terminal state is reachable from them\n\n%s",
			len(r.Wedged), r.Wedged[0])
	}

	return b.String()
}

// OK reports whether the model passed everything.
func (r Result[S]) OK() bool {
	return r.Violation == nil && len(r.Deadlocks) == 0 && len(r.Wedged) == 0
}
