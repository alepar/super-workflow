package wftest

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type uncertainFirst struct{}
type uncertainWork struct{}
type uncertainRepair struct{}
type uncertainDone struct{}
type uncertainOutput struct{}

func uncertainFactory(command bool, modes ...string) func() *Sim {
	return func() *Sim {
		mode := ""
		if len(modes) > 0 {
			mode = modes[0]
		}
		d := wf.New("uncertain")
		first := wf.NewCommand[uncertainFirst](d, "prepare")
		first.Key("prepare")
		repair := wf.NewCommand[uncertainRepair](d, "repair")
		repair.Key("repair")
		done := wf.NewTerminal[uncertainDone](d, wf.Success)
		d.OnFailure(wf.Uncertain, repair)
		repaired := repair.To(done, "repaired")
		repair.Route(func(wf.RunResult, wf.Ctx) wf.Edge[uncertainRepair] { return repaired }, repaired)
		if command {
			work := wf.NewCommand[uncertainWork](d, "work")
			switch mode {
			case "idempotent":
				work.Idempotent()
			case "unkeyed":
			case "detached":
				work.Key("work").Detached(time.Minute)
			default:
				work.Key("work")
			}
			ready := first.To(work, "ready")
			first.Route(func(wf.RunResult, wf.Ctx) wf.Edge[uncertainFirst] { return ready }, ready)
			finish := work.To(done, "finished")
			work.Route(func(wf.RunResult, wf.Ctx) wf.Edge[uncertainWork] { return finish }, finish)
		} else {
			work := wf.NewCompute[uncertainWork, uncertainOutput](d, func(wf.Ctx) (uncertainOutput, error) { return uncertainOutput{}, nil })
			ready := first.To(work, "ready")
			first.Route(func(wf.RunResult, wf.Ctx) wf.Edge[uncertainFirst] { return ready }, ready)
			key := wf.KeyOf("work")
			if mode == "non-idempotent" {
				key = wf.NonIdempotent("cannot query")
			}
			finish := work.To(done, "finished").With(wf.ExternalEffect{Adapter: "shell", Op: "run", Key: key, Args: map[string]any{"argv": []string{"work"}}})
			work.Route(func(uncertainOutput, wf.Ctx) wf.Edge[uncertainWork] { return finish }, finish)
		}
		if mode == "detached" {
			return New(d, WithEffectRunner(NewFakeRunner()))
		}
		return New(d)
	}
}

func TestFuzzUncertainRestartsAndRecoversDefaultRoute(t *testing.T) {
	for _, command := range []bool{false, true} {
		factory := uncertainFactory(command)
		result := Fuzz(factory, FuzzConfig{MaxSteps: 10, Fault: &FuzzFault{State: "uncertainWork", Class: "effect-uncertain", Adapter: "shell", Op: "run"}})
		if !result.FaultInjected || result.Finding != nil || result.Status.Terminal != "uncertainDone" {
			t.Fatalf("command=%v: %s", command, result)
		}
		var uncertain []string
		var results []string
		for _, record := range result.Trace.Journal() {
			switch record.Kind {
			case wf.JournalRefused:
				var data wf.RefusedData
				if err := json.Unmarshal(record.Data, &data); err != nil {
					t.Fatal(err)
				}
				if record.Class == wf.Uncertain {
					uncertain = append(uncertain, record.State)
				}
			case wf.JournalEffectResult:
				results = append(results, record.State)
			}
		}
		if !reflect.DeepEqual(uncertain, []string{"uncertainWork"}) {
			t.Fatalf("wrong uncertain states: %v\n%s", uncertain, result)
		}
		if !reflect.DeepEqual(results, []string{"uncertainFirst", "uncertainRepair"}) {
			t.Fatalf("crashed effect got a result or contaminated sibling: %v", results)
		}
		replay := result.Replay(factory)
		if !replay.FaultInjected || replay.Finding != nil || replay.Status.Terminal != result.Status.Terminal || !reflect.DeepEqual(replay.Trace.Journal(), result.Trace.Journal()) {
			t.Fatalf("replay differs: %s", replay)
		}
	}
}

func TestFuzzUncertainReconcilesWithoutAdapterCheck(t *testing.T) {
	for _, test := range []struct {
		command bool
		mode    string
		repair  bool
	}{{true, "idempotent", false}, {true, "unkeyed", true}, {false, "non-idempotent", true}} {
		t.Run(test.mode, func(t *testing.T) {
			result := Fuzz(uncertainFactory(test.command, test.mode), FuzzConfig{MaxSteps: 10, Fault: &FuzzFault{State: "uncertainWork", Class: "effect-uncertain", Adapter: "shell", Op: "run"}})
			if !result.FaultInjected || result.Finding != nil || result.Status.Terminal != "uncertainDone" {
				t.Fatal(result)
			}
			repaired := false
			for _, record := range result.Trace.Journal() {
				if record.State == "uncertainRepair" {
					repaired = true
				}
			}
			if repaired != test.repair {
				t.Fatalf("repair=%t, want %t: %s", repaired, test.repair, result)
			}
		})
	}
}

func TestFuzzUncertainDetachedReportsFixtureGap(t *testing.T) {
	result := Fuzz(uncertainFactory(true, "detached"), FuzzConfig{MaxSteps: 10, Fault: &FuzzFault{State: "uncertainWork", Class: "effect-uncertain", Adapter: "shell", Op: "run"}})
	if result.FaultInjected || result.Finding == nil || result.Finding.Rule != "fixture-gap" {
		t.Fatal(result)
	}
}

func TestFuzzUncertainStartsRecoveryOnlyWhenReconciled(t *testing.T) {
	f := &fuzzRun{sim: New(wf.New("phase")), result: &FuzzResult{config: FuzzConfig{Fault: &FuzzFault{State: "Work", Class: "effect-uncertain", Adapter: "test", Op: "write"}}}, schemas: map[string]wf.StateView{}}
	f.setupPhases(f.result.config)
	effect := wf.ExternalEffect{Adapter: "test", Op: "write", Key: wf.KeyOf("work")}
	runner := &runner{}
	if !runner.guard(func() { _, _ = f.sim.world.ApplyAt("", wf.EffectContext{State: "Work"}, effect) }) {
		t.Fatal("expected crash")
	}
	if f.good || f.result.FaultInjected {
		t.Fatal("recovery began before restart reconciled intent")
	}
	// Another key must not consume or inherit the selected invocation's answer.
	other := effect
	other.Key = wf.KeyOf("other")
	if status, _ := f.sim.world.Check("", other); status != wf.EffectNotDone {
		t.Fatalf("unrelated check: %s", status)
	}
	if f.good {
		t.Fatal("unrelated check started recovery")
	}
	if status, _ := f.sim.world.Check("", effect); status != wf.EffectUnknown {
		t.Fatalf("selected check: %s", status)
	}
	if !f.good || !f.result.FaultInjected {
		t.Fatal("reconciliation did not start recovery")
	}
}

type uncertainAgent struct{}

func TestFuzzUncertainAfterSubmitReplaysHumanBoundary(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("uncertain-submit")
		agent := wf.NewAgent[uncertainAgent, bool](d)
		work := wf.NewCommand[uncertainWork](d, "work")
		work.Key("work")
		done := wf.NewTerminal[uncertainDone](d, wf.Success)
		fail := agent.To(done, "exhausted")
		agent.Spec(wf.PromptText("Proceed."), wf.Retry[uncertainAgent](wf.Const(2), fail))
		ready := agent.To(work, "ready")
		agent.Route(func(bool, wf.Ctx) wf.Edge[uncertainAgent] { return ready }, ready)
		finish := work.To(done, "finished")
		work.Route(func(wf.RunResult, wf.Ctx) wf.Edge[uncertainWork] { return finish }, finish)
		return New(d)
	}
	result := Fuzz(factory, FuzzConfig{MaxSteps: 10, Progress: map[string][]json.RawMessage{"uncertainAgent": {json.RawMessage(`true`)}}, Fault: &FuzzFault{State: "uncertainWork", Class: "effect-uncertain", Adapter: "shell", Op: "run"}})
	if !result.FaultInjected || result.Finding != nil || result.End != "human gate" || len(result.Script) != 1 {
		t.Fatal(result)
	}
	minimized := result.Minimize(factory)
	replay := minimized.Replay(factory)
	if !minimized.FaultInjected || len(minimized.Script) != 1 || !replay.FaultInjected || replay.Finding != nil || replay.End != "human gate" || !reflect.DeepEqual(result.Advisories, replay.Advisories) {
		t.Fatalf("lost crash on reduction: %s", replay)
	}
}
