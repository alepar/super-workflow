package wf

import "strings"

// RuleUnreachableReasonEmpty rejects an undocumented coverage exemption.
const RuleUnreachableReasonEmpty RuleID = "unreachable-reason-empty"

// LintReachability validates declarations that exempt edges from seed coverage.
func (d *Def) LintReachability() []Problem {
	return lintDef(d, "", map[*Def]bool{}, []rule{ruleUnreachableReason}, false)
}

func ruleUnreachableReason(c *lintCtx) []Problem {
	var out []Problem
	for _, s := range c.states {
		for i, e := range s.Edges {
			if e.Unreachable && strings.TrimSpace(e.UnreachableReason) == "" {
				out = append(out, s.node.problem(RuleUnreachableReasonEmpty, "edge[%d] %q marked unreachable by design needs a nonempty reason", i, e.Label))
			}
		}
	}
	return out
}

// UnreachableEdges counts declared coverage exemptions, including children.
// Invalid blank reasons count as declarations but fail LintReachability.
func (d *Def) UnreachableEdges() int {
	var count func(DefView) int
	count = func(v DefView) int {
		n := 0
		for _, s := range v.States {
			for _, e := range s.Edges {
				if e.Unreachable {
					n++
				}
			}
		}
		for _, child := range v.Children {
			n += count(child)
		}
		return n
	}
	return count(Inspect(d))
}
