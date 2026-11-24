package wftest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCheckMaxRunsCapsAllSearchLayers(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("budget")
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedDone](d, wf.Success)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("work"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")))
		a.Then(a.To(end, "done"))
		return New(d)
	}
	cfg := CheckConfig{MaxRuns: 2, CalibrationConfig: CalibrationConfig{Seeds: 4, MaxSteps: 8, Valid: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}}, Scenarios: map[string]FuzzConfig{"last": {}}}
	report := Check(factory, cfg)
	if report.Runs != 2 || report.MixedSeeds != 1 || report.LivenessSeeds != 0 || report.CorpusSeeds != 0 || report.AgentSeeds != 0 || report.ScenarioSeeds != 0 {
		t.Fatalf("budget exceeded or priority changed: %+v", report)
	}
	if len(report.RealProblems) != 0 || len(report.CoverageProblems) == 0 || len(report.NotExplored) == 0 || !strings.Contains(report.String(), "not explored (budget)") || len(report.Problems) == 0 {
		t.Fatalf("silent incomplete check: %s", report)
	}
	again := Check(factory, cfg)
	if !reflect.DeepEqual(report.NotExplored, again.NotExplored) || !reflect.DeepEqual(report.Problems, again.Problems) {
		t.Fatal("budget report changed between runs")
	}
	cfg.MaxRuns = 0
	unlimited := Check(factory, cfg)
	if unlimited.Runs <= 2 || len(unlimited.NotExplored) != 0 || unlimited.ScenarioSeeds != 1 {
		t.Fatalf("unlimited changed: %s", unlimited)
	}
	cfg.MaxRuns = unlimited.Runs
	exact := Check(factory, cfg)
	if exact.Runs != unlimited.Runs || len(exact.NotExplored) != 0 {
		t.Fatalf("exact budget incorrectly exhausted: %s", exact)
	}
}

func TestCheckMaxRunsRejectsNegative(t *testing.T) {
	report := Check(func() *Sim { t.Fatal("negative budget invoked factory"); return nil }, CheckConfig{MaxRuns: -1})
	if len(report.Problems) == 0 {
		t.Fatal("negative budget accepted")
	}
}

func TestCheckBudgetStopsGatePreludeAndTimeoutExpansion(t *testing.T) {
	cfg := CheckConfig{MaxRuns: 4, CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 12}}
	report := check(gatePreludeFactory, cfg, false)
	if report.Runs != 4 || len(report.NotExplored) == 0 {
		t.Fatalf("unbounded expansion: %s", report)
	}
	labels := strings.Join(report.NotExplored, "\n")
	for _, want := range []string{"gate child/gatePathReview answer continue", "fault child/gatePathReview timeout", "repeated fault"} {
		if !strings.Contains(labels, want) {
			t.Errorf("missing skipped target %q: %s", want, labels)
		}
	}
	if len(report.UnreachedFaults) != 0 {
		t.Fatalf("budget exhaustion misreported as fixture gap: %v", report.UnreachedFaults)
	}
	if report.GateSeeds != 0 || report.FaultSeeds != 0 {
		t.Fatal("skipped searches counted as executed")
	}
}

func TestCheckBudgetCountsLivenessPrefixAndReduction(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("budget-live")
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedDone](d, wf.Success)
		a.Spec(wf.PromptText("work"), wf.Retry[corpusStart](wf.Const(2), a.To(end, "stop")))
		a.Then(a.To(end, "done"))
		return New(d)
	}
	b := &checkRunBudget{max: 1}
	cfg := CalibrationConfig{budget: b}
	result := cfg.run(factory, "liveness", FuzzConfig{MaxSteps: 8, Liveness: true, LivenessAfter: -1, Valid: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}})
	if result != nil || b.used != 1 || !b.seen["liveness"] {
		t.Fatalf("liveness prefix not budgeted: result=%v budget=%+v", result, b)
	}
	original := &FuzzResult{Finding: &FuzzFinding{Rule: "example"}, Script: []FuzzAction{{Kind: FuzzSubmit}}}
	reduced := original.minimize(func() *Sim { t.Fatal("replay exceeded budget"); return nil }, func() bool { return b.take("minimize example") })
	if reduced != original || reduced.ShrinkComplete || !b.seen["minimize example"] {
		t.Fatal("lost original finding or claimed completed reduction")
	}
}

func TestCheckSeparatesCoverageFromTraversedExemptions(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("exemption")
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedDone](d, wf.Success)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("work"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")))
		a.Then(a.To(end, "impossible").Unreachable("deliberately false proof"))
		return New(d)
	}
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 8, Valid: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}}}
	report := check(factory, cfg, false)
	if !strings.Contains(strings.Join(report.RealProblems, "\n"), "seeds traversed it") {
		t.Fatalf("traversed exemption hidden as coverage: %s", report)
	}
	for _, problem := range report.CoverageProblems {
		if strings.Contains(problem, "seeds traversed") {
			t.Fatal(problem)
		}
	}
	if len(report.Problems) != len(report.RealProblems)+len(report.CoverageProblems) {
		t.Fatal("combined verdict lost diagnostics")
	}
}

func TestCoverageDiagnosticCategories(t *testing.T) {
	c := &FuzzCoverage{Def: "d", States: []string{"unseen", "false-exemption"}, Reached: map[string]int{"false-exemption": 1}, Unreachable: map[string]string{"false-exemption": "reason", "unknown": "reason"}, Ends: map[string]int{}}
	missing, real := c.stateProblems()
	if len(missing) != 2 || len(real) != 2 {
		t.Fatalf("state diagnostics mixed: missing=%v real=%v", missing, real)
	}
	c.Edges = []string{"unseen", "false-exemption", "na", "blank"}
	c.EdgesReached = map[string]int{"false-exemption": 1, "na": 1}
	c.EdgeUnreachable = map[string]string{"false-exemption": "reason", "blank": ""}
	c.EdgeNotApplicable = map[string]string{"na": "reason"}
	missing, real = c.edgeProblems()
	if len(missing) != 1 || len(real) != 3 {
		t.Fatalf("edge diagnostics mixed: missing=%v real=%v", missing, real)
	}
}
