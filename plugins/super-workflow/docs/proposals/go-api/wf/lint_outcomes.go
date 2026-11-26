package wf

import (
	"fmt"
	"strings"
)

const (
	RuleEndsOutcomeMismatch   RuleID = "ends-outcome-mismatch"
	RuleGateEndsMissing       RuleID = "gate-ends-missing"
	RuleGateAnswersEquivalent RuleID = "gate-answers-equivalent"
)

// Intent documents what reaching this terminal means, especially when Success
// means producing a result rather than that result being favorable.
func (t *Terminal[S]) Intent(description string) *Terminal[S] {
	if t.n.def.sealed {
		t.n.def.misuse("Terminal.Intent after the definition was sealed")
		return t
	}
	if strings.TrimSpace(description) == "" {
		t.n.def.misuse("Terminal.Intent needs a nonblank description")
	}
	t.k.intent = description
	return t
}

// PartialWhen makes a Success terminal end Partial when notReady holds as the
// run reaches it: the result is delivered, but not ready (a hand-off with work
// outstanding), so the run's outcome must not read success. A child that ends
// Partial settles Accepted like a Success one. notReady reads Ctx like a
// Yield; it runs before the Yield.
func (t *Terminal[S]) PartialWhen(notReady func(Ctx) bool) *Terminal[S] {
	if t.n.def.sealed {
		t.n.def.misuse("Terminal.PartialWhen after the definition was sealed")
		return t
	}
	if t.k.outcome != Success || notReady == nil {
		t.n.def.misuse(fmt.Sprintf("Terminal.PartialWhen needs a Success terminal and a predicate (%s is %s)", t.n.name, t.k.outcome))
		return t
	}
	t.k.partial = notReady
	return t
}

// EquivalentAnswers explains why distinct answers deliberately take the same
// effect-free continuation. It does not exempt terminal outcome contracts.
func (g *Gate[S, A]) EquivalentAnswers(reason string) *Gate[S, A] {
	if g.n.def.sealed {
		g.n.def.misuse("Gate.EquivalentAnswers after the definition was sealed")
		return g
	}
	if strings.TrimSpace(reason) == "" {
		g.n.def.misuse("Gate.EquivalentAnswers needs a nonblank reason")
	}
	g.k.equivalentAnswers = reason
	return g
}

func ruleEndsOutcomes(c *lintCtx) []Problem {
	var out []Problem
	for i, n := range c.d.nodes {
		for j, e := range n.edges {
			if e.endsOutcome == "" {
				continue
			}
			var actual Outcome
			if e.to != nil {
				if terminal, ok := e.to.kind.(*terminalKind); ok {
					actual = terminal.outcome
				}
			}
			if actual != e.endsOutcome {
				out = append(out, Problem{Rule: RuleEndsOutcomeMismatch, Path: c.states[i].Path,
					Msg: edgeMsg(c.states[i].Edges[j], fmt.Sprintf("Ends expects %s; target terminal outcome is %q (empty means nonterminal)", e.endsOutcome, actual))})
			}
		}
	}
	return out
}

func ruleGateAnswerOutcomes(c *lintCtx) []Problem {
	var out []Problem
	for i, n := range c.d.nodes {
		gate, ok := n.kind.(*gateKind)
		if !ok || n.cases == nil {
			continue
		}
		checked := map[*edgeData]bool{}
		check := func(use edgeUse) {
			e := use.e
			if e == nil || e.to == nil || checked[e] {
				return
			}
			checked[e] = true
			if _, terminal := e.to.kind.(*terminalKind); terminal && e.endsOutcome == "" {
				out = append(out, Problem{Rule: RuleGateEndsMissing, Path: c.states[i].Path,
					Msg: fmt.Sprintf("answer edge %q -> %s needs Ends(expectedOutcome, reason)", e.label, e.to.name)})
			}
		}
		for _, row := range n.cases.cases {
			if row.hasRoute {
				for _, use := range row.cands {
					check(use)
				}
			} else {
				check(row.e)
			}
		}
		if n.cases.otherwise != nil {
			check(*n.cases.otherwise)
		}
		if gate.equivalentAnswers != "" {
			continue
		}
		for a, first := range n.cases.cases {
			if first.hasRoute || first.e.e == nil || first.e.e.to == nil || len(first.e.effects) != 0 || len(first.e.e.effects) != 0 {
				continue
			}
			for _, second := range n.cases.cases[a+1:] {
				if second.hasRoute || second.e.e == nil || first.value == second.value || len(second.e.effects) != 0 || len(second.e.e.effects) != 0 {
					continue
				}
				if first.e.e.to == second.e.e.to {
					out = append(out, Problem{Rule: RuleGateAnswersEquivalent, Path: c.states[i].Path,
						Msg: fmt.Sprintf("answers %q and %q both continue to %s without effects; declare EquivalentAnswers(reason) if intentional", first.value, second.value, first.e.e.to.name)})
				}
			}
		}
	}
	return out
}
