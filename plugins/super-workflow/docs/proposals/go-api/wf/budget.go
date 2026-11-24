package wf

import (
	"fmt"
	"strings"
)

type budgetTerm struct {
	k int
	c *Counter
}

// CounterOption configures a declared counter. It is sealed: build one with
// ScopedTo.
type CounterOption struct{ scope Counter }

// ScopedTo makes the counter count only the Incs since scope last rose: the
// executor zeroes it whenever scope is incremented. Render shows it as
// name/scope. The scope must be a plain counter declared on the same Def
// (lint counter-scope-def): a child that needs a bound per entry needs no
// scope, its instance is fresh per entry.
func ScopedTo(scope Counter) CounterOption { return CounterOption{scope: scope} }

// Budget is a renderable expression over counters and constants
// (e.g. 3 + redesignCycles).
//
// Owns: caps as data, so render, hash and lint can show and check them and the
// executor evaluates them.
// Does not: hold a closure; the renderer must see every cap.
// Extend by: a new cap is a composition of Counters and constants.
type Budget struct{ terms []budgetTerm }

// Const is a constant budget.
func Const(n int) Budget { return Budget{terms: []budgetTerm{{k: n}}} }

// Count is the current value of a counter.
func Count(c Counter) Budget { return Budget{terms: []budgetTerm{{c: &c}}} }

// Plus adds two budgets.
func (b Budget) Plus(o Budget) Budget {
	return Budget{terms: append(append([]budgetTerm{}, b.terms...), o.terms...)}
}

// String renders the expression, e.g. "3 + redesignCycles".
func (b Budget) String() string {
	parts := make([]string, 0, len(b.terms))
	for _, t := range b.terms {
		if t.c != nil {
			parts = append(parts, t.c.label())
		} else {
			parts = append(parts, fmt.Sprint(t.k))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, " + ")
}

// Counters lists the counters the expression reads.
func (b Budget) Counters() []Counter {
	var out []Counter
	for _, t := range b.terms {
		if t.c != nil {
			out = append(out, *t.c)
		}
	}
	return out
}

// MaxVisits caps visits of the edge's source state at most N times, where N is
// b: the (N+1)th arrival takes over instead of entering the state. The count is
// per instance of the definition that declares the state (the top-level run,
// each Map item instance, each Sub launch) and never resets within it. over is
// a full Edge: its With effects are stored, rendered, hashed and linted like
// those of any other edge.
func MaxVisits[S any](b Budget, over Edge[S]) {
	if over.e == nil || over.e.from == nil {
		return
	}
	n := over.e.from
	n.maxVisits = &visitLimit{max: b, over: over.use()}
	over.e.visits = &b
}
