package wf

import (
	"encoding/json"
	"testing"
)

// causeRun builds the cycle runCause reads from hand-written records: a root
// with the given child instances, ended at term with outcome.
func causeRun(term string, outcome Outcome, children map[string]*instance, recs ...JournalRecord) *cycle {
	insts := map[string]*instance{"": {ID: ""}}
	for id, in := range children {
		in.ID = id
		insts[id] = in
	}
	for i := range recs {
		recs[i].Seq = int64(i + 1)
	}
	return &cycle{recs: recs, st: &runState{ended: true, term: term, outcome: outcome, insts: insts}}
}

func causeRec(kind JournalKind, inst, state string, data any) JournalRecord {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return JournalRecord{Kind: kind, Instance: inst, State: state, Data: raw}
}

func obsFail(inst, state, msg string) JournalRecord {
	return causeRec(JournalObserved, inst, state, ObservedData{Desc: ObservationDesc{Adapter: "gh", Op: "pr"}, Error: msg})
}

func obsOK(inst, state string) JournalRecord {
	return causeRec(JournalObserved, inst, state, ObservedData{Desc: ObservationDesc{Adapter: "gh", Op: "pr"}})
}

func planTo(term, label string) AcceptedData {
	return AcceptedData{Plan: EdgePlan{Steps: []EdgeStep{{From: "A", To: term, Label: label}}}}
}

func TestRunCause(t *testing.T) {
	ended := func(term string, o Outcome) JournalRecord {
		return causeRec(JournalRunEnded, "", "", RunEndedData{Terminal: term, Outcome: o})
	}
	failedChild := func(id string) JournalRecord {
		return causeRec(JournalChildSettled, "", "Fan", ChildSettledData{ID: id, State: "Fan", Terminal: "cBad", Outcome: Stopped, Class: Refused})
	}
	okChild := func(id string) JournalRecord {
		return causeRec(JournalChildSettled, "", "Fan", ChildSettledData{ID: id, State: "Fan", Terminal: "cOK", Outcome: Success, Class: Accepted})
	}
	kid := func() *instance { return &instance{Parent: "", DefName: "c-item"} }

	cases := []struct {
		name string
		c    *cycle
		want string
	}{
		{"the deciding state and edge when nothing failed", causeRun("Bad", Stopped, nil,
			causeRec(JournalAccepted, "", "A", planTo("Bad", "gave up")), ended("Bad", Stopped)), "A: gave up"},
		{"empty when neither source exists", causeRun("Bad", Stopped, nil,
			causeRec(JournalAccepted, "", "A", planTo("Bad", "")), ended("Bad", Stopped)), ""},
		{"last failed observation wins over the label", causeRun("Bad", Stopped, nil,
			obsFail("", "A", "first"), obsFail("", "A", "boom"),
			causeRec(JournalAccepted, "", "A", planTo("Bad", "gave up")), ended("Bad", Stopped)), "A: gh.pr failed: boom"},
		{"effect error", causeRun("Bad", Stopped, nil,
			causeRec(JournalEffectIntent, "", "A", EffectIntentData{Effect: EffectData{Kind: "external", Adapter: "git", Op: "merge"}}),
			causeRec(JournalEffectResult, "", "A", EffectResultData{Intent: 1, Outcome: "failed", Error: "conflict"}),
			causeRec(JournalAccepted, "", "A", planTo("Bad", "gave up")), ended("Bad", Stopped)), "A: git.merge failed: conflict"},
		{"retried and recovered observation is skipped", causeRun("Bad", Stopped, nil,
			obsFail("", "A", "flaky"), obsOK("", "A"),
			causeRec(JournalAccepted, "", "A", planTo("Bad", "gave up")), ended("Bad", Stopped)), "A: gave up"},
		{"a later failure of another state is not superseded", causeRun("Bad", Stopped, nil,
			obsFail("", "A", "flaky"), obsOK("", "A"), obsFail("", "B", "real"),
			causeRec(JournalAccepted, "", "B", planTo("Bad", "x")), ended("Bad", Stopped)), "B: gh.pr failed: real"},
		{"failure inside a failed child surfaces", causeRun("Bad", Stopped, map[string]*instance{"c#1": kid()},
			obsFail("c#1", "W", "child boom"), failedChild("c#1"),
			causeRec(JournalAccepted, "", "Fan", planTo("Bad", "bad")), ended("Bad", Stopped)), "c-item[c#1]/W: gh.pr failed: child boom"},
		{"a failed child without a record falls back to its settlement, not the parent label", causeRun("Bad", Stopped, map[string]*instance{"c#1": kid()},
			failedChild("c#1"),
			causeRec(JournalAccepted, "", "Fan", planTo("Bad", "bad")), ended("Bad", Stopped)), "c-item[c#1] ended at cBad (stopped)"},
		{"a sibling that succeeded stays off the path", causeRun("Bad", Stopped, map[string]*instance{"c#1": kid(), "c#2": kid()},
			obsFail("c#2", "W", "sibling noise"), okChild("c#2"), failedChild("c#1"),
			causeRec(JournalAccepted, "", "Fan", planTo("Bad", "bad")), ended("Bad", Stopped)), "c-item[c#1] ended at cBad (stopped)"},
		{"a child instance that never settled failed stays off the path", causeRun("Bad", Stopped, map[string]*instance{"c#1": kid()},
			obsFail("c#1", "W", "unrelated"),
			causeRec(JournalAccepted, "", "Fan", planTo("Bad", "bad")), ended("Bad", Stopped)), "A: bad"},
		// D3 (hermes-ds rung 3): a child that stopped on its own route names
		// the state, the edge (and the record it names) instead of only the
		// terminal.
		{"a failed child names its own deciding edge", causeRun("Bad", Stopped, map[string]*instance{"c#1": kid()},
			causeRec(JournalAccepted, "c#1", "Gated", AcceptedData{Plan: EdgePlan{Steps: []EdgeStep{{From: "Gated", To: "cBad", Label: "blocked: the graph gate refused the task tree (graph-gate-stop.json)"}}}}),
			failedChild("c#1"),
			causeRec(JournalAccepted, "", "Fan", planTo("Bad", "bad")), ended("Bad", Stopped)),
			"c-item[c#1] ended at cBad (stopped): c-item[c#1]/Gated: blocked: the graph gate refused the task tree (graph-gate-stop.json)"},
		{"an exhausted refusal names the deciding check", causeRun("Bad", Stopped, nil,
			causeRec(JournalRefused, "", "R", RefusedData{Reasons: []string{"graph-gate: the graph gate refused the task tree\ngate: cycle a -> b -> a"}, Exhausted: ExhaustedBudget,
				Plan: &EdgePlan{Steps: []EdgeStep{{From: "R", To: "Bad", Label: "blocked: repair failed (graph-gate-stop.json)"}}}}),
			ended("Bad", Stopped)),
			"R: blocked: repair failed (graph-gate-stop.json) · refused: graph-gate: the graph gate refused the task tree"},
		{"budget stop", causeRun("", Stopped, nil,
			causeRec(JournalBudgetStopped, "", "A", BudgetStoppedData{Reason: "turns spent"}), ended("", Stopped)), "A: budget stopped: turns spent"},
		{"a successful run names its edge, never an old failure", causeRun("Done", Success, nil,
			obsFail("", "A", "recovered by a loop"),
			causeRec(JournalAccepted, "", "A", planTo("Done", "done")), ended("Done", Success)), "done"},
	}
	for _, tc := range cases {
		if got := tc.c.runCause(); got != tc.want {
			t.Errorf("%s: runCause = %q, want %q", tc.name, got, tc.want)
		}
	}
}
