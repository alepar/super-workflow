package wftest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// EdgeProblems checks the stronger, per-edge coverage property separately
// from Problems, which retains its legacy agent-state coverage contract.
func (c *FuzzCoverage) EdgeProblems() []string {
	missing, violations := c.edgeProblems()
	out := append(missing, violations...)
	sort.Strings(out)
	return out
}

func (c *FuzzCoverage) edgeProblems() (missing, violations []string) {
	for _, key := range c.Edges {
		reason, exempt := c.EdgeUnreachable[key]
		e := c.edgeViews[key]
		label := fmt.Sprintf("%s (%q -> %s)", key, e.Label, e.To)
		if reason, na := c.EdgeNotApplicable[key]; na {
			if c.EdgesReached[key] > 0 {
				violations = append(violations, fmt.Sprintf("%s: n/a by class (%s) but %d seeds traversed it", label, reason, c.EdgesReached[key]))
			}
			continue
		}
		switch {
		case exempt && strings.TrimSpace(reason) == "":
			violations = append(violations, label+": unreachable by design needs a nonempty reason")
		case exempt && c.EdgesReached[key] > 0:
			violations = append(violations, fmt.Sprintf("%s: marked unreachable (%s) but %d seeds traversed it", label, reason, c.EdgesReached[key]))
		case !exempt && c.EdgesReached[key] == 0:
			hint := `: no seed traversed this edge; add a scenario that reaches it or annotate the edge with .Unreachable("reason") explaining why it cannot occur`
			if e.Class != "" {
				hint += `; override an inherited default on a Map with Map.OnClass, or on an agent, compute, command or Sub with Retry(...).OnClass, keeping the same target and adding that annotation`
			}
			missing = append(missing, label+hint)
		}
	}
	sort.Strings(missing)
	sort.Strings(violations)
	return missing, violations
}

// addEdges folds selected plans per instance, crediting them only when their
// transition commits. The declaration index distinguishes even identically
// labelled parallel edges. Child spawn records qualify definition names under
// their parent, rather than conflating equally named nested definitions.
func (c *FuzzCoverage) addEdges(r *FuzzResult) {
	if r.Trace == nil {
		return
	}
	defs := map[string]string{"": c.Def}
	pending := map[string]*wf.EdgePlan{}
	once := map[string]bool{}
	for _, record := range r.Trace.Journal() {
		switch record.Kind {
		case wf.JournalChildSpawned:
			var d wf.ChildSpawnedData
			if json.Unmarshal(record.Data, &d) == nil && defs[record.Instance] != "" {
				defs[d.ID] = defs[record.Instance] + "/" + d.Def
			}
		case wf.JournalAccepted, wf.JournalCompletedByObservation, wf.JournalRefused,
			wf.JournalRouteOffCandidates, wf.JournalPromptOffCandidates,
			wf.JournalAutoAnswered, wf.JournalGateAnswered, wf.JournalRetried:
			var d struct{ Plan *wf.EdgePlan }
			if json.Unmarshal(record.Data, &d) == nil {
				pending[record.Instance] = d.Plan
			}
		case wf.JournalTransition:
			plan := pending[record.Instance]
			delete(pending, record.Instance)
			var d wf.TransitionData
			if plan == nil || len(plan.Steps) == 0 || json.Unmarshal(record.Data, &d) != nil {
				continue
			}
			if plan.Steps[0].From != record.State || plan.Steps[len(plan.Steps)-1].To != d.To {
				continue
			}
			for _, step := range plan.Steps {
				key := fmt.Sprintf("%s/%s/edge[%d]", defs[record.Instance], step.From, step.Index)
				edge, known := c.edgeViews[key]
				if known && edge.Label == step.Label && edge.To == step.To && !once[key] {
					once[key] = true
					c.EdgesReached[key]++
				}
			}
		}
	}
}
