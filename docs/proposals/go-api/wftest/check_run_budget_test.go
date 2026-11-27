package wftest_test

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	rbFirst  struct{}
	rbSecond struct{}
	rbDone   struct{}
)

type rbOut struct{ V string }

// A run the budget stops itself ends the drive with End "run budget":
// stopped, not ended, and no finding.
func TestFuzzRunBudgetStopEndsTheDrive(t *testing.T) {
	d := wf.New("rb-chain")
	a := wf.NewAgent[rbFirst, rbOut](d)
	b := wf.NewAgent[rbSecond, rbOut](d)
	done := wf.NewTerminal[rbDone](d, wf.Success)
	a.Spec(wf.PromptText("First."), wf.Retry[rbFirst](wf.Const(1), a.To(done, "gave up")))
	b.Spec(wf.PromptText("Second."), wf.Retry[rbSecond](wf.Const(1), b.To(done, "gave up")))
	toB, toDone := a.To(b, "next"), b.To(done, "done")
	a.Route(func(rbOut, wf.Ctx) wf.Edge[rbFirst] { return toB }, toB)
	b.Route(func(rbOut, wf.Ctx) wf.Edge[rbSecond] { return toDone }, toDone)
	got := wftest.Fuzz(func() *wftest.Sim { return wftest.New(d) }, wftest.FuzzConfig{MaxSteps: 20, FailPercent: -1, SchemaPercent: -1, RunBudget: wf.RunBudget{Turns: 1}})
	if got.Finding != nil || got.End != "run budget" || got.Status.Phase == wf.RunEnded || !got.Status.Stopped {
		t.Fatalf("result end=%q finding=%v phase=%s stopped=%v, want a run-budget stop", got.End, got.Finding, got.Status.Phase, got.Status.Stopped)
	}
}

// Check simulates the registration's default budget being spent at several
// turns of the nested definition, and the definition passes; with no budget
// it makes no such runs.
func TestCheckSimulatesTheDefaultBudget(t *testing.T) {
	ballots := []json.RawMessage{json.RawMessage(`{"OK":true}`), json.RawMessage(`{"OK":false}`)}
	cfg := wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{
		Valid:    map[string][]json.RawMessage{"nmVote": ballots},
		Progress: map[string][]json.RawMessage{"nmVote": ballots},
	}}
	factory := func() *wftest.Sim { return wftest.New(nestedPanelsDef()) }
	if got := wftest.Check(factory, cfg); got.RunBudgetSeeds != 0 {
		t.Fatalf("run-budget runs without a budget: %d", got.RunBudgetSeeds)
	}
	cfg.Budget = func(any) wf.RunBudget { return wf.RunBudget{Turns: 200, Tokens: 1_000_000} }
	got := wftest.Check(factory, cfg)
	for _, p := range got.Problems {
		t.Error(p)
	}
	// The reference run and a stop at its first, middle and last turns.
	if got.RunBudgetSeeds != 4 {
		t.Fatalf("run-budget runs = %d, want 4\n%s", got.RunBudgetSeeds, got)
	}
}
