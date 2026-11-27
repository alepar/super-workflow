package wf

import "fmt"

// ObservationDesc is the renderable descriptor of a read-only adapter
// operation; Result is the result type name.
type ObservationDesc struct {
	Adapter, Op string
	Args        map[string]any
	Result      string
}

// Observation is a sealed descriptor of a read-only adapter operation
// (adapter, op, args, result type T).
//
// Owns: what a check, guard, wait predicate or map source reads from the world;
// the unit the executor digests into the observation token.
// Does not: perform effects (Effect does that), or say how the read is
// performed and classified (ok or unavailable): that is executor territory.
// Extend by: a new read is a constructor in an adapter package calling
// Observe; no framework change.
type Observation[T any] struct{ desc ObservationDesc }

// Observe builds an Observation; Result is filled from T's type name.
func Observe[T any](adapter, op string, args map[string]any) Observation[T] {
	return Observation[T]{desc: ObservationDesc{Adapter: adapter, Op: op, Args: args, Result: fmt.Sprintf("%T", *new(T))}}
}

// Desc returns the descriptor.
func (o Observation[T]) Desc() ObservationDesc { return o.desc }

// Reader is any observation, untyped.
type Reader interface{ Desc() ObservationDesc }

// Observed is the typed read of an observation; it returns an error when the
// read was classified unavailable. In a Compute that error is classed
// Unavailable and takes Compute.Retry's routes. Reading an Observation the
// Check or Compute did not declare with Observes is a run-time Blocked outcome
// (an executor obligation; lint runs no closure).
func Observed[T any](c Ctx, o Observation[T]) (T, error) {
	v, err := c.value(ctxKey{kind: "observed", ref: o.desc})
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeValue[T](v), nil
}

type doneWhenData struct {
	obs  ObservationDesc
	cond string
}

// DoneWhen declares the state's postcondition: when the observation shows the
// state's work already done (work finished out of band, or before a crash),
// the executor skips the state and records completed_by_observation. Render
// shows it as "done when: <cond>".
//
// Owns: one data-only postcondition per state, read through an Observation.
// Does not: choose a route (a postcondition only establishes that an effect
// happened; decisions are replayed from the journal, never inferred from
// artifacts), run a closure, or run at lint or render time.
// Extend by: a new postcondition is a new Observation constructor in an
// adapter package.
func DoneWhen[T any](s State, o Observation[T], cond string) {
	s.node().doneWhen = &doneWhenData{obs: o.desc, cond: cond}
}
