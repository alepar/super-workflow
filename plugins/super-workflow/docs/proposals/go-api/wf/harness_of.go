package wf

import "encoding/json"

// HarnessOf reads the canonical name of the harness that produced the output
// agent state a last accepted in this instance: the child harness the
// accepted attempt's hand-out dispatched it to (OnFamily), otherwise the
// harness of the coordinator that hand-out went to. Like OutputOf it is the
// latest accepted visit's, not this round's; a's own checks and route read
// the attempt they decide. It is "" before a has accepted an output, and for
// an output no hand-out produced.
//
// The value comes from the journal's hand-out record of the accepted
// attempt, so it stays the same across a resume, including one on another
// coordinator harness. What a harness name means to the run (the home
// family, the other) is the definition's policy.
func HarnessOf[S, O any](c Ctx, a *Agent[S, O]) string {
	return read[string](c, "harness", a.n)
}

// unitHarness is the canonical harness that produced u's output: its child
// harness, else its hand-out's coordinator harness, else the harness the
// hand-out's segment was bound to; "" without a unit.
func (st *runState) unitHarness(u *unit) string {
	switch {
	case u == nil:
		return ""
	case u.ChildHarness != "":
		return normalizeHarness(u.ChildHarness)
	case u.HandHarness != "":
		return normalizeHarness(u.HandHarness)
	}
	for i := len(st.segments) - 1; i >= 0; i-- {
		if st.segments[i].Start == u.HandSegment {
			return st.segmentHarness(i)
		}
	}
	return st.segmentHarness(len(st.segments) - 1)
}

// acceptHarness records the harness of the attempt whose output state
// accepted, or forgets an earlier visit's when none is known: the record
// always belongs to the latest accepted output.
func (in *instance) acceptHarness(state, h string) {
	if h == "" {
		delete(in.Harnesses, state)
		return
	}
	if in.Harnesses == nil {
		in.Harnesses = map[string]string{}
	}
	in.Harnesses[state] = h
}

// harnessValue serves the harness read.
func (x *runCtx) harnessValue(n *node) (any, error) {
	h := x.in.Harnesses[n.name]
	if u := x.in.Unit; x.in.Cur == n.name && x.in.Phase == phaseIssued && u != nil && !u.State.terminal() {
		// The state's own checks and route read the attempt being decided.
		h = x.c.st.unitHarness(u)
	}
	b, err := json.Marshal(h)
	return json.RawMessage(b), err
}
