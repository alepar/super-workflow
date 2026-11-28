package wftest

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"reflect"
	"strings"
)

// explainedStop checks the visit that chose a non-success terminal. An earlier
// repaired refusal elsewhere in the run is not a cause of this stop.
func (f *fuzzRun) explainedStop() bool {
	defs := map[string]string{"": f.sim.def.Name()}
	records := f.result.Trace.Journal()
	sawStop := false
	proved := map[string]bool{}
transitions:
	for i, r := range records {
		if r.Kind == wf.JournalChildSpawned {
			var d wf.ChildSpawnedData
			if json.Unmarshal(r.Data, &d) == nil {
				defs[d.ID] = d.Def
			}
		}
		if r.Kind == wf.JournalBudgetStopped {
			proved[r.Instance] = true
		}
		// A child halted on a named failure settles without a terminal; its
		// halt is the cause the parent's failure route carries on.
		if r.Kind == wf.JournalRefused {
			var halt wf.RefusedData
			if json.Unmarshal(r.Data, &halt) == nil && halt.Halt && strings.TrimSpace(strings.Join(halt.Reasons, "")) != "" {
				proved[r.Instance] = true
			}
		}
		if r.Kind == wf.JournalChildSettled {
			var child wf.ChildSettledData
			if json.Unmarshal(r.Data, &child) == nil && !child.Outcome.Delivered() && !child.Cancelled && !proved[child.ID] {
				f.result.Finding = &FuzzFinding{Rule: "ends-silent", State: defs[child.ID] + "/" + child.Terminal, Why: "a non-success child terminal must have a causal named decision", Seed: f.result.Seed}
				return false
			}
		}
		if r.Kind != wf.JournalTransition {
			continue
		}
		var transition wf.TransitionData
		if json.Unmarshal(r.Data, &transition) != nil {
			continue
		}
		view := f.views[defs[r.Instance]]
		terminal := false
		for _, state := range view.States {
			if state.Name == transition.To && state.Kind == "terminal" && state.Outcome != string(wf.Success) {
				terminal = true
				break
			}
		}
		if !terminal {
			continue
		}
		sawStop = true
		proved[r.Instance] = true
		var source wf.StateView
		for _, state := range view.States {
			if state.Name == r.State {
				source = state
				break
			}
		}
		// A human answer is an explicit decision to stop.
		if source.Kind == "gate" {
			continue transitions
		}
		edge := ""
		terminalEdgeIndex := -1
		// settled holds the states whose visit, between here and the stop,
		// ended in an accepted result: a refusal earlier in that same visit
		// was repaired by a retry and caused nothing.
		settled := map[string]bool{}
		for j := i - 1; j >= 0; j-- {
			previous := records[j]
			if previous.Instance != r.Instance {
				continue
			}
			// The cause of a stop can lie upstream of the deciding state when
			// the states between are computed: a failed agent routes to a
			// state that records it, which then ends the run. Walk back
			// through those transitions; an agent visit or a human answer
			// ends the walk.
			if previous.Kind == wf.JournalInstructionIssued || previous.Kind == wf.JournalGateAnswered || previous.Kind == wf.JournalAutoAnswered {
				break
			}
			if previous.Kind == wf.JournalTransition {
				// Crossing into the visit of the state it came from: the
				// state it entered starts its visit here.
				var into wf.TransitionData
				if json.Unmarshal(previous.Data, &into) == nil {
					delete(settled, into.To)
				}
				continue
			}
			var decision struct{ Plan *wf.EdgePlan }
			if json.Unmarshal(previous.Data, &decision) == nil && decision.Plan != nil {
				for _, step := range decision.Plan.Steps {
					if terminalEdgeIndex < 0 && step.From == r.State && step.To == transition.To {
						edge = step.Label
						terminalEdgeIndex = step.Index
					}
					// A declared end reason survives the computed/command
					// states that persist it before entering the terminal.
					// An exhausted MaxVisits budget is likewise a cause.
					if f.declaredStopEdge(view, step) {
						continue transitions
					}
				}
			}
			if previous.Kind == wf.JournalRefused && !settled[previous.State] {
				var d wf.RefusedData
				if json.Unmarshal(previous.Data, &d) == nil && len(d.Reasons) > 0 && strings.TrimSpace(strings.Join(d.Reasons, "")) != "" {
					continue transitions
				}
			}
			if previous.Kind == wf.JournalBudgetStopped {
				continue transitions
			}
			// An agent's accepted result supersedes earlier refusals of its
			// visit: a valid output chose this stop. A computed state only
			// passes on what it read, so the walk goes on past it, and a sub
			// state passes on its child's terminal, whose own deciding
			// transition was checked when the child settled.
			if previous.Kind == wf.JournalAccepted || previous.Kind == wf.JournalCompletedByObservation {
				for k := j - 1; k >= 0; k-- {
					settled := records[k]
					if settled.Instance != r.Instance {
						continue
					}
					if settled.Kind == wf.JournalTransition || settled.Kind == wf.JournalInstructionIssued {
						break
					}
					if settled.Kind == wf.JournalChildSettled && settled.State == previous.State {
						var child wf.ChildSettledData
						if json.Unmarshal(settled.Data, &child) == nil && !child.Outcome.Delivered() && proved[child.ID] {
							continue transitions
						}
					}
				}
				var accepted wf.AcceptedData
				if previous.Kind == wf.JournalAccepted && json.Unmarshal(previous.Data, &accepted) == nil && accepted.Computed {
					settled[previous.State] = true
					continue
				}
				break
			}
		}
		for _, candidate := range source.Edges {
			if edge == "" && candidate.To == transition.To {
				edge = candidate.Label
				break
			}
		}
		if terminalEdgeIndex >= 0 && terminalEdgeIndex < len(source.Edges) {
			candidate := source.Edges[terminalEdgeIndex]
			if candidate.Label == edge && candidate.To == transition.To && candidate.Fallback != "" {
				continue transitions
			}
		}
		f.result.Finding = &FuzzFinding{Rule: "ends-silent", State: view.Name + "/" + r.State, Edge: edge, Target: transition.To, Why: "a stop must follow a human decision or a causal named failure", Seed: f.result.Seed}
		return false
	}
	if sawStop || f.result.Status.Phase != wf.RunEnded || f.result.Status.Outcome.Delivered() {
		return true
	}
	// A terminal at the start, or an engine stop with no deciding transition,
	// needs an explicit engine cause rather than just its terminal name.
	if f.result.Status.Cause != "" && f.result.Status.Terminal == "" {
		return true
	}
	f.find("ends-silent", "the non-success outcome must carry a causal named failure")
	return false
}

// Compatibility shim until wf-gate lands its public EdgeView.Ends field. It
// reads explicit metadata only; an edge label is never treated as intent.
func fuzzEndReason(edge wf.EdgeView) string {
	field := reflect.ValueOf(edge).FieldByName("Ends")
	if field.IsValid() && field.Kind() == reflect.String {
		return field.String()
	}
	return ""
}

// declaredStopEdge checks explicit end metadata on a traversed plan step.
func (f *fuzzRun) declaredStopEdge(view wf.DefView, step wf.EdgeStep) bool {
	for _, state := range view.States {
		if state.Name != step.From {
			continue
		}
		if step.Index < 0 || step.Index >= len(state.Edges) {
			return false
		}
		edge := state.Edges[step.Index]
		return (edge.Over || fuzzEndReason(edge) != "") && edge.Label == step.Label && edge.To == step.To
	}
	return false
}
