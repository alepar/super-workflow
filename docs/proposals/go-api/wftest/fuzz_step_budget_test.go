package wftest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type stepBudgetFirst struct{}
type stepBudgetNext struct{}
type stepBudgetDone struct{}

func progressingStepFactory(counter bool) func() *Sim {
	return func() *Sim {
		d := wf.New("progressing-steps")
		a := wf.NewAgent[stepBudgetFirst, bool](d)
		end := wf.NewTerminal[stepBudgetDone](d, wf.Success)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("work"), wf.Retry[stepBudgetFirst](wf.Const(2), a.To(end, "failed")))
		if counter {
			round := d.Counter("round")
			again, done := a.To(a, "next round").With(round.Inc()), a.To(end, "done")
			a.Route(func(bool, wf.Ctx) wf.Edge[stepBudgetFirst] { return again }, again, done)
			wf.MaxVisits(wf.Const(3), done)
		} else {
			b := wf.NewAgent[stepBudgetNext, bool](d)
			b.Spec(wf.PromptText("next"), wf.Retry[stepBudgetNext](wf.Const(2), b.To(end, "failed")))
			a.Then(a.To(b, "advance"))
			b.Then(b.To(end, "done"))
		}
		return New(d)
	}
}

func TestFuzzProgressingStepBoundIsBudget(t *testing.T) {
	for _, counter := range []bool{false, true} {
		factory := progressingStepFactory(counter)
		got := Fuzz(factory, FuzzConfig{MaxSteps: 1, FailPercent: -1, SchemaPercent: -1})
		if got.Finding != nil || got.End != "budget" || got.Status.Phase == wf.RunEnded {
			t.Fatalf("pending progressing work misclassified (counter=%v): %s", counter, got)
		}
		if replay := got.Replay(factory); replay.Finding != nil || replay.End != "budget" {
			t.Fatalf("budget replay changed: %s", replay)
		}
		finished := Fuzz(factory, FuzzConfig{MaxSteps: 8, FailPercent: -1, SchemaPercent: -1})
		if finished.Finding != nil || finished.Status.Outcome != wf.Success {
			t.Fatalf("larger bound did not finish finite work: %s", finished)
		}
	}
}

func TestCheckProgressingStepBoundRemainsStrictCoverageFailure(t *testing.T) {
	progress := map[string][]json.RawMessage{"stepBudgetFirst": {json.RawMessage(`true`)}, "stepBudgetNext": {json.RawMessage(`true`)}}
	report := check(progressingStepFactory(false), CheckConfig{CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 8, Valid: progress, Progress: progress}, Scenarios: map[string]FuzzConfig{"short": {MaxSteps: 1, FailPercent: -1, SchemaPercent: -1}}}, false)
	if !strings.Contains(strings.Join(report.CoverageProblems, "\n"), "not explored (budget): scenario short: step bound") || !strings.Contains(strings.Join(report.NotExplored, "\n"), "step bound") {
		t.Fatalf("step budget must be explicit incomplete exploration: %s", report)
	}
	for _, finding := range report.Findings {
		if finding.config.MaxSteps == 1 {
			t.Fatalf("short progressing scenario became a real finding: %s", finding)
		}
	}
}

func TestProgressingStepBoundDoesNotRelaxLiveness(t *testing.T) {
	got := Fuzz(progressingStepFactory(false), FuzzConfig{MaxSteps: 1, Liveness: true, LivenessAfter: 0, LivenessSteps: 1, Progress: map[string][]json.RawMessage{"stepBudgetFirst": {json.RawMessage(`true`)}}})
	if got.Finding == nil || got.Finding.Rule != "liveness" {
		t.Fatalf("strict all-good bound was weakened: %s", got)
	}
}

func TestStepContextTreatsAbsentCountersAsZero(t *testing.T) {
	context := func(counters map[string]int) string {
		return stepContext(wf.RunStatus{Instances: []wf.InstanceStatus{{ID: "root", State: "work", Counters: counters}}}, "root")
	}
	if context(nil) != context(map[string]int{"round": 0}) {
		t.Fatal("recording a zero counter must not count as progress")
	}
	if context(nil) == context(map[string]int{"round": 1}) {
		t.Fatal("advancing an explicit counter must change context")
	}
}

func TestStepBudgetDefersUnusedEffectResults(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("pending-effect")
		a := wf.NewAgent[stepBudgetFirst, bool](d)
		b := wf.NewAgent[stepBudgetNext, bool](d)
		c := wf.NewCommand[sequenceCommand](d, "check")
		end := wf.NewTerminal[stepBudgetDone](d, wf.Success)
		a.Spec(wf.PromptText("first"), wf.Retry[stepBudgetFirst](wf.Const(2), a.To(end, "failed")))
		b.Spec(wf.PromptText("next"), wf.Retry[stepBudgetNext](wf.Const(2), b.To(end, "failed")))
		a.Then(a.To(b, "next"))
		b.Then(b.To(c, "check"))
		c.Then(c.To(end, "done"))
		return New(d)
	}
	cfg := FuzzConfig{MaxSteps: 1, FailPercent: -1, SchemaPercent: -1, EffectResults: []FuzzEffectSequence{{State: "sequenceCommand", Adapter: "shell", Op: "run", Results: []json.RawMessage{json.RawMessage(`{"Exit":0}`)}}}}
	got := Fuzz(factory, cfg)
	if got.Finding != nil || got.End != "budget" {
		t.Fatalf("pending effect sequence became a finding: %s", got)
	}
	cfg.MaxSteps = 8
	if done := Fuzz(factory, cfg); done.Finding != nil || done.End != "success" {
		t.Fatalf("larger budget must consume the effect: %s", done)
	}
}

func TestProgressingNestedStepBoundIsBudget(t *testing.T) {
	factory := func() *Sim {
		child := progressingStepFactory(false)().def
		d := wf.New("parent")
		sub := wf.NewSub[stepBudgetFirst](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		end := wf.NewTerminal[stepBudgetDone](d, wf.Success)
		sub.Exits(wf.OnExit[stepBudgetDone](sub.To(end, "finished")))
		return New(d)
	}
	got := Fuzz(factory, FuzzConfig{MaxSteps: 1, FailPercent: -1, SchemaPercent: -1})
	if got.Finding != nil || got.End != "budget" {
		t.Fatalf("nested progressing work misclassified: %s", got)
	}
}

func TestStepBudgetPreservesCompletedChildStopChecks(t *testing.T) {
	factory := func() *Sim {
		child := wf.New("silent-child")
		wf.NewTerminal[sequenceDone](child, wf.Stopped)
		d := wf.New("pending-after-child")
		sub := wf.NewSub[sequenceAgent](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		a := wf.NewAgent[stepBudgetFirst, bool](d)
		b := wf.NewAgent[stepBudgetNext, bool](d)
		end := wf.NewTerminal[stepBudgetDone](d, wf.Success)
		sub.Exits(wf.OnExit[sequenceDone](sub.To(a, "continue")))
		a.Spec(wf.PromptText("first"), wf.Retry[stepBudgetFirst](wf.Const(2), a.To(end, "failed")))
		b.Spec(wf.PromptText("next"), wf.Retry[stepBudgetNext](wf.Const(2), b.To(end, "failed")))
		a.Then(a.To(b, "next"))
		b.Then(b.To(end, "done"))
		return New(d)
	}
	got := Fuzz(factory, FuzzConfig{MaxSteps: 1, FailPercent: -1, SchemaPercent: -1})
	if got.Finding == nil || got.Finding.Rule != "ends-silent" {
		t.Fatalf("budget cut hid completed child violation: %s", got)
	}
}
