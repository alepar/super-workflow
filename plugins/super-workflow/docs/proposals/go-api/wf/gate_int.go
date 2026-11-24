package wf

import (
	"fmt"
	"strings"
)

// GateIntValueView describes an answer's integer range. Proposal is present
// on a live instruction; Minimum names an optional context-dependent bound.
type GateIntValueView struct {
	Answer   string
	Min, Max int
	Minimum  string `json:",omitempty"`
	Proposal *int   `json:",omitempty"`
}

type gateIntSpec struct {
	view     GateIntValueView
	proposal func(Ctx) int
	minimum  func(Ctx) int
}

// IntValue declares an optional integer for one reserved answer. Omitting the
// number accepts proposal. Bounds are inclusive. The proposal reads only Ctx.
func (g *Gate[S, A]) IntValue(answer A, min, max int, proposal func(Ctx) int) *Gate[S, A] {
	name := fmt.Sprint(answer)
	if g.k.intValues == nil {
		g.k.intValues = map[string]*gateIntSpec{}
	}
	if _, exists := g.k.intValues[name]; exists {
		g.k.intErrors = append(g.k.intErrors, "duplicate integer declaration for "+name)
		return g
	}
	g.k.intValues[name] = &gateIntSpec{view: GateIntValueView{Answer: name, Min: min, Max: max}, proposal: proposal}
	return g
}

// IntMinimum raises an IntValue's minimum using current journaled context.
// name identifies the bound in inspection and hashing; change it when its
// meaning changes. Declare IntValue first. A supplied value below this bound
// is rejected without answering the gate.
func (g *Gate[S, A]) IntMinimum(answer A, name string, minimum func(Ctx) int) *Gate[S, A] {
	spec := g.k.intValues[fmt.Sprint(answer)]
	if spec == nil || spec.minimum != nil || strings.TrimSpace(name) == "" || minimum == nil {
		g.k.intErrors = append(g.k.intErrors, "IntMinimum requires an existing IntValue, a nonempty name and one nonnil callback")
		return g
	}
	spec.view.Minimum, spec.minimum = name, minimum
	return g
}

// AnswerValue reads the latest accepted integer from this gate in the current
// instance. It is absent before an answer or after an answer without an integer.
// Like ComputedOf, it is not scoped to the current visit.
func (g *Gate[S, A]) AnswerValue(c Ctx) (int, bool) {
	value := read[*int](c, "gateInt", g.n)
	if value == nil {
		return 0, false
	}
	return *value, true
}

func (k *gateKind) intValueViews() []GateIntValueView {
	var out []GateIntValueView
	if k.answers != nil {
		for _, answer := range k.answers.values {
			if spec := k.intValues[answer]; spec != nil {
				out = append(out, spec.view)
			}
		}
	}
	return out
}

func (k *gateKind) lintIntValues(n *node) []Problem {
	var out []Problem
	for _, err := range k.intErrors {
		out = append(out, n.problem(RuleGateAnswers, "%s", err))
	}
	for answer, spec := range k.intValues {
		switch {
		case k.reservation != Reserved:
			out = append(out, n.problem(RuleGateAnswers, "integer answer %q requires a reserved gate", answer))
		case k.answers == nil || !contains(k.answers.values, answer):
			out = append(out, n.problem(RuleGateAnswers, "integer answer %q is not in the answer set", answer))
		case spec.view.Min > spec.view.Max:
			out = append(out, n.problem(RuleGateAnswers, "integer answer %q has min greater than max", answer))
		case spec.proposal == nil:
			out = append(out, n.problem(RuleGateAnswers, "integer answer %q requires a proposal", answer))
		}
	}
	return out
}

func (k *gateKind) liveIntValues(c *cycle, in *instance) ([]GateIntValueView, error) {
	views := k.intValueViews()
	if len(views) == 0 {
		return views, nil
	}
	ctx, err := c.ctx(in)
	if err != nil {
		return nil, err
	}
	for i := range views {
		v := &views[i]
		spec := k.intValues[v.Answer]
		if spec.minimum != nil {
			v.Min = max(v.Min, spec.minimum(ctx))
		}
		if spec.proposal == nil {
			return nil, fmt.Errorf("integer answer %q has no proposal", v.Answer)
		}
		proposal := spec.proposal(ctx)
		if ctx.blocked {
			return nil, ctx.blockedErr()
		}
		if v.Min > v.Max || proposal < v.Min || proposal > v.Max {
			return nil, fmt.Errorf("integer answer %q proposal %d is outside [%d,%d]", v.Answer, proposal, v.Min, v.Max)
		}
		v.Proposal = &proposal
	}
	return views, nil
}

func (k *gateKind) resolveIntValue(c *cycle, in *instance, answer string, number *int) (*int, error) {
	spec := k.intValues[answer]
	if spec == nil {
		if number != nil {
			return nil, fmt.Errorf("answer %q does not accept an integer", answer)
		}
		return nil, nil
	}
	views, err := k.liveIntValues(c, in)
	if err != nil {
		return nil, err
	}
	for _, v := range views {
		if v.Answer != answer {
			continue
		}
		value := *v.Proposal
		if number != nil {
			value = *number
		}
		if value < v.Min || value > v.Max {
			return nil, fmt.Errorf("answer %q integer %d is outside [%d,%d]", answer, value, v.Min, v.Max)
		}
		return &value, nil
	}
	return nil, fmt.Errorf("unknown integer answer %q", answer)
}

func gateIntInstructions(views []GateIntValueView, submit string) string {
	var b strings.Builder
	for _, v := range views {
		if v.Proposal == nil {
			continue
		}
		line := strings.Replace(submit, "<value>", shellQuote(v.Answer), 1)
		fmt.Fprintf(&b, "\nFor %s, the proposed number is %d (allowed %d..%d). Omit --number to accept the proposal; to use the person's number, run:\n%s --number <integer>", v.Answer, *v.Proposal, v.Min, v.Max, line)
	}
	return b.String()
}
