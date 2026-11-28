package wf

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Automatic returns (sw-5blp): send-back as a default property of the
// engine. A compute or a check that finds an upstream output wrong returns
// wf.Reject(reasons...) instead of a plain error; with no edge declared, the
// engine returns the work to the agent whose output that call read, a
// bounded number of times (d.Returns, default 1), then fails over exactly as
// the plain error would have.
//
// Owns: the typed error, identifying the producer from the call's own reads,
// the bound per (rejecting state, producer, instance), the plan that returns
// the work, and the degraded note when no producer can be identified.
// Does not: guess. A call that read no agent output, or several, returns
// nothing; RejectOf names the producer then.

// RejectError is the error Reject and RejectOf return.
type RejectError struct {
	Reasons  []string
	producer *node
}

func (e *RejectError) Error() string { return "rejected: " + strings.Join(e.Reasons, "; ") }

func rejectReasons(reasons []string) []string {
	var out []string
	for _, r := range reasons {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		out = []string{"no reason given"}
	}
	return out
}

// Reject is the error a compute body or a check returns when an upstream
// output is wrong, with the reasons as data. The engine returns the work to
// the one agent whose output the call read (see Def.Returns); when it cannot
// tell which, or the returns are spent, the error fails the state as any
// other error does.
func Reject(reasons ...string) error { return &RejectError{Reasons: rejectReasons(reasons)} }

// RejectOf is Reject naming the producer, for a call that reads several
// agents' outputs.
func RejectOf(producer State, reasons ...string) error {
	e := &RejectError{Reasons: rejectReasons(reasons)}
	if producer != nil {
		e.producer = producer.node()
	}
	return e
}

// Returns sets how many times a state that raises Reject returns the work to
// one producer in an instance before its failure handling applies (default
// 1); Const(0) switches automatic returns off. An explicit SendBack edge to
// that producer takes precedence with its own bound and fallback.
func (d *Def) Returns(b Budget) *Def {
	if d.sealed {
		d.misuse("Returns after the definition was used")
		return d
	}
	d.returns = &b
	return d
}

// returnsBudget is the definition's bound on automatic returns.
func (d *Def) returnsBudget() Budget {
	if d.returns != nil {
		return *d.returns
	}
	return Const(1)
}

// producerTracker notes the agent outputs a call reads through its Ctx.
type producerTracker struct {
	Ctx
	read []*node
}

func (t *producerTracker) value(k ctxKey) (any, error) {
	var n *node
	switch k.kind {
	case "output":
		n, _ = k.ref.(*node)
	case "ref", "artifact":
		if a, ok := k.ref.(*artifactData); ok {
			n = a.producer
		}
	}
	if n != nil {
		if _, agent := n.kind.(*agentKind); agent && !slices.Contains(t.read, n) {
			t.read = append(t.read, n)
		}
	}
	return t.Ctx.value(k)
}

// rejectProducer is the agent state a Reject from state n returns the work
// to: the one it names, else the one agent of n's definition, upstream of n
// and other than n and except, whose output the call read. why says why
// there is none.
func rejectProducer(n *node, rej *RejectError, read []*node, except *node) (*node, string) {
	ok := func(p *node) bool {
		_, agent := p.kind.(*agentKind)
		return agent && p.def == n.def && p != n && p != except && reach(p)[n]
	}
	if p := rej.producer; p != nil {
		if !ok(p) {
			return nil, fmt.Sprintf("RejectOf(%s) names no agent upstream of %s in its definition", p.name, n.name)
		}
		return p, ""
	}
	var cands []string
	var p *node
	for _, r := range read {
		if ok(r) {
			p = r
			cands = append(cands, r.name)
		}
	}
	switch len(cands) {
	case 1:
		return p, ""
	case 0:
		return nil, "the call read no upstream agent's output: name the producer with wf.RejectOf"
	}
	return nil, "the call read " + strings.Join(cands, " and ") + ": name the producer with wf.RejectOf"
}

// rejectPlan plans the return of n's work to producer for reasons: through
// an explicit SendBack edge to producer when n declares one (its bound and
// fallback), else the automatic step. ok is false when the automatic returns
// are spent (the caller fails over as for a plain error).
func (c *cycle) rejectPlan(in *instance, n *node, producer *node, reasons []string) (EdgePlan, bool, error) {
	for _, e := range n.edges {
		if e.sendBack == nil || e.to != producer {
			continue
		}
		step, err := c.stepOf(in, edgeUse{e: e, effects: e.effects, optional: e.optional})
		if err != nil {
			return EdgePlan{}, false, err
		}
		max := evalCounters(e.sendBack.max, in.countersAfter([]EdgeStep{step}))
		k := in.Returns[sendBackKey(step.From, step.Label)]
		if k >= max {
			p, err := c.planOf(in, e.sendBack.fallback)
			if err == nil && len(p.Steps) > 0 {
				p.Steps[0].SendBackSpent = e.label
			}
			return p, err == nil, err
		}
		step.SendBack = &SendBackStep{Round: k + 1, Of: max, Reasons: reasons}
		return c.withVisitCap(in, step)
	}
	step := EdgeStep{From: n.name, Index: -1, To: producer.name, Label: "return to " + producer.name, Auto: true}
	max := evalCounters(in.def.returnsBudget(), in.Counters)
	k := in.Returns[sendBackKey(step.From, step.Label)]
	if k >= max {
		return EdgePlan{}, false, nil
	}
	step.SendBack = &SendBackStep{Round: k + 1, Of: max, Reasons: reasons}
	return c.withVisitCap(in, step)
}

// withVisitCap is the plan of a return step: the producer's MaxVisits over
// edge follows it when the producer's visits are spent, as planOf does.
func (c *cycle) withVisitCap(in *instance, step EdgeStep) (EdgePlan, bool, error) {
	p := EdgePlan{Steps: []EdgeStep{step}}
	var to *node
	for _, x := range in.def.nodes {
		if x.name == step.To {
			to = x
		}
	}
	if to == nil || to.maxVisits == nil || in.Visits[to.name] < evalCounters(to.maxVisits.max, in.countersAfter(p.Steps)) {
		return p, true, nil
	}
	rest, err := c.planOf(in, to.maxVisits.over)
	if err != nil {
		return p, false, err
	}
	p.Steps = append(p.Steps, rest.Steps...)
	return p, true, nil
}

// rejected handles an error from state n's call: ok is false when it is no
// Reject, or the work cannot be returned (then the caller fails as before;
// an unidentified producer is journaled as degraded first). plan returns the
// work when ok.
func (c *cycle) rejected(in *instance, n *node, err error, read []*node, except *node) (plan EdgePlan, reasons []string, ok bool, ferr error) {
	var rej *RejectError
	if !errors.As(err, &rej) {
		return EdgePlan{}, nil, false, nil
	}
	producer, why := rejectProducer(n, rej, read, except)
	if producer == nil {
		d := DegradedData{Unit: c.st.qualified(in), Kind: DegradedRejectUnattributed, Cause: why,
			Action: "failed as before, with no return: " + strings.Join(rej.Reasons, "; ")}
		return EdgePlan{}, rej.Reasons, false, c.emit(in, JournalDegraded, 0, d)
	}
	p, ok, perr := c.rejectPlan(in, n, producer, rej.Reasons)
	return p, rej.Reasons, ok, perr
}

// DegradedRejectUnattributed: a state raised wf.Reject but the engine could
// not tell which agent's output it rejected (it read none, or several), so
// it failed as a plain error would; RejectOf names the producer.
const DegradedRejectUnattributed DegradedKind = "reject unattributed"
