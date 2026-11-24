package wftest_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func checkFactory() *wftest.Sim {
	d := wf.New("suite")
	a := wf.NewAgent[fuzzAgent, bool](d)
	done := wf.NewTerminal[fuzzDone](d, wf.Success)
	stop := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
	stop.Answers(wf.On(fuzzVerdict("yes"), stop.To(done, "approved").Ends(wf.Success, "the person approves completion")))
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "exhausted")))
	a.Then(a.To(done, "accepted"))
	return wftest.New(d)
}

func TestCheckMissingFixturesCannotPass(t *testing.T) {
	got := wftest.Check(checkFactory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1}})
	if len(got.Problems) == 0 || !strings.Contains(got.String(), "fixture gap") {
		t.Fatalf("missing fixtures passed: %s", got)
	}
}

func TestCheckRunsDeterministicSeedsAndEnforcesCoverage(t *testing.T) {
	// This candidate is deliberately never returned. Targeted failure runs now
	// cover the ordinary retry routes even with just one random seed.
	factory := func() *wftest.Sim {
		d := wf.New("suite")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "exhausted")))
		used, unused := a.To(done, "accepted"), a.To(stop, "never selected").Ends("deliberately unused candidate")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { return used }, used, unused)
		return wftest.New(d)
	}
	cfg := wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}}
	got := wftest.Check(factory, cfg)
	if got.Coverage == nil || got.MixedSeeds != 1 || got.LivenessSeeds != 1 || len(got.Problems) == 0 || !strings.Contains(got.String(), "edge") {
		t.Fatalf("coverage not enforced: %s", got)
	}
	again := wftest.Check(factory, cfg)
	if strings.Join(got.Problems, "\n") != strings.Join(again.Problems, "\n") {
		t.Fatalf("nondeterministic problems: %v vs %v", got.Problems, again.Problems)
	}
}

func TestCheckCommandPrintsReportAndReturnsVerdict(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("terminal")
		wf.NewTerminal[fuzzDone](d, wf.Success)
		return wftest.New(d)
	}
	var out bytes.Buffer
	if err := wftest.CheckCommand(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1}})(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "calibration:") || !strings.Contains(out.String(), "fuzz coverage") {
		t.Fatalf("missing report: %s", &out)
	}
	out.Reset()
	if err := wftest.CheckCommand(checkFactory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1}})(&out); err == nil {
		t.Fatal("fixture gaps returned success")
	}
}

func TestCheckRetainsReducedSimulationFindings(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("broken-route")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "exhausted")))
		edge := a.To(done, "accepted")
		a.Route(func(bool, wf.Ctx) wf.Edge[fuzzAgent] { panic("route broke") }, edge)
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 8, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}})
	if len(got.Findings) == 0 {
		t.Fatalf("missing findings: %s", got)
	}
	sites := map[string]bool{}
	for _, finding := range got.Findings {
		if sites[finding.Finding.Site()] {
			t.Fatalf("duplicate minimized site: %s", finding.Finding.Site())
		}
		sites[finding.Finding.Site()] = true
		if !finding.ShrinkComplete || finding.Finding == nil || !strings.Contains(finding.String(), "script:") {
			t.Fatalf("missing reduced reproducer: %s", finding)
		}
	}
}

func TestCheckCompleteAgentSuitePasses(t *testing.T) {
	got := wftest.Check(checkFactory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}})
	if len(got.Problems) != 0 {
		t.Fatalf("complete suite failed: %v", got.Problems)
	}
	if got.MixedSeeds != 64 || got.LivenessSeeds != 64 {
		t.Fatalf("default seeds=%d", got.Coverage.Seeds)
	}
}

func TestCheckExercisesEveryGateAnswer(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("gate-suite")
		g := wf.NewGate[fuzzStop, fuzzVerdict](d, "decision?", wf.NewEnum(d, "answer", fuzzVerdict("yes"), fuzzVerdict("no"), fuzzVerdict("later")), wf.Reserved)
		end := wf.NewTerminal[fuzzDone](d, wf.Success)
		g.EquivalentAnswers("the fixture records each answer and then finishes").Answers(wf.On(fuzzVerdict("yes"), g.To(end, "approved").Ends(wf.Success, "answer recorded")), wf.On(fuzzVerdict("no"), g.To(end, "declined").Ends(wf.Success, "answer recorded")), wf.On(fuzzVerdict("later"), g.To(end, "deferred").Ends(wf.Success, "answer recorded")))
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1}})
	if len(got.Problems) != 0 {
		t.Fatalf("gate suite: %v", got.Problems)
	}
	if got.GateSeeds != 3 || len(got.Coverage.EdgeProblems()) != 0 {
		t.Fatalf("gate answers not exercised: %v", got.Coverage.EdgeProblems())
	}
}

func TestCheckRequiresExplicitProgressExamples(t *testing.T) {
	got := wftest.Check(checkFactory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}})
	if !strings.Contains(strings.Join(got.Problems, "\n"), "no-progress-example") {
		t.Fatalf("missing progress fixture passed: %v", got.Problems)
	}
}

func TestCheckLivenessUsesProgressInsteadOfOtherValidResults(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("progress-suite")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "exhausted")))
		good := a.To(done, "completed")
		other := a.To(stop, "declined").Ends("the valid result declines work")
		a.Route(func(value bool, _ wf.Ctx) wf.Edge[fuzzAgent] {
			if value {
				return good
			}
			return other
		}, good, other)
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 8, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`false`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}})
	for _, finding := range got.Findings {
		if finding.Finding.Rule == "liveness" || finding.Finding.Rule == "no-progress-example" {
			t.Fatalf("progress fixture was not propagated: %s", finding.Finding)
		}
	}
	if got.LivenessSeeds != 8 {
		t.Fatalf("liveness did not run: %v", got.Problems)
	}
}
