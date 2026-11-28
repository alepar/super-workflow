package wf

// RuleCheckExamples is a check without both sides of its contract.
const RuleCheckExamples RuleID = "check-examples"

// LintChecks is the calibration contract lint. It is enforced by check;
// keeping it separate permits existing live runs during corpus migration.
func (d *Def) LintChecks() []Problem {
	return lintDef(d, "", map[*Def]bool{}, []rule{ruleCheckExamples}, false)
}

func ruleCheckExamples(c *lintCtx) []Problem {
	var out []Problem
	for _, state := range c.states {
		if state.Kind != "agent" {
			continue
		}
		examples := state.CheckExamples()
		for _, name := range state.Checks {
			accepting, refusing := false, false
			for _, example := range examples[name] {
				if example.Refusal == "" {
					accepting = true
				} else {
					refusing = true
				}
			}
			if !accepting || !refusing {
				out = append(out, state.node.problem(RuleCheckExamples, "check %q: examples must declare acceptance and refusal", name))
			}
		}
	}
	return out
}
