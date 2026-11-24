package wftest

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type declaredCompute struct{}
type declaredAgent struct{}
type declaredStop struct{}

func TestLivenessAcceptsDeclaredStoppedTerminal(t *testing.T) {
	for _, mode := range []string{"precondition", "retry-exhausted"} {
		t.Run(mode, func(t *testing.T) {
			factory := func() *Sim {
				d := wf.New("declared-stop")

				obs := wf.Observe[bool]("test", "read", nil)
				c := wf.NewCompute[declaredCompute, bool](d, func(ctx wf.Ctx) (bool, error) { return wf.Observed(ctx, obs) }).Observes(obs)
				stop := wf.NewTerminal[declaredStop](d, wf.Stopped)
				retry := wf.Retry[declaredCompute](wf.Const(1), c.To(stop, "cannot complete"))
				if mode == "precondition" {
					retry = retry.OnClass(wf.Refused, c.To(stop, "precondition not met"))
				}
				c.Retry(retry)
				c.Then(c.To(stop, "declined"))
				w := NewWorld()
				Set(w, obs, true)

				return New(d, WithWorld(w))
			}
			cfg := FuzzConfig{MaxSteps: 5, Fault: &FuzzFault{State: "declaredCompute", Adapter: "test", Op: "read", Class: "observation-error"}}
			result := Fuzz(factory, cfg)
			if result.Finding != nil || result.Status.Phase != wf.RunEnded || result.Status.Terminal != "declaredStop" || result.Status.Outcome != wf.Stopped {
				t.Fatalf("declared stop rejected: %s", result)
			}
			if replay := result.Replay(factory); replay.Finding != nil || replay.Status.Terminal != "declaredStop" {
				t.Fatalf("replay: %s", replay)
			}
		})
	}
}

func TestDeclaredTerminalDoesNotHideNamedHaltOrLoop(t *testing.T) {
	for _, loop := range []bool{false, true} {
		factory := func() *Sim {
			d := wf.New("not-terminal")
			c := wf.NewCompute[declaredCompute, bool](d, func(wf.Ctx) (bool, error) {
				if !loop {
					return false, errors.New("named halt")
				}
				return true, nil
			})
			_ = wf.NewTerminal[declaredStop](d, wf.Stopped)
			c.Then(c.To(c, "again"))
			return New(d)
		}
		result := Fuzz(factory, FuzzConfig{MaxSteps: 3, Liveness: true})
		if result.Finding == nil {
			t.Fatalf("nonterminal accepted: %s", result)
		}
	}
}

func TestAllGoodDeclaredStopStillFailsLiveness(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("all-good-stop")
		a := wf.NewAgent[declaredAgent, bool](d)
		stop := wf.NewTerminal[declaredStop](d, wf.Stopped)
		a.Spec(wf.PromptText("decide"), wf.Retry[declaredAgent](wf.Const(1), a.To(stop, "no answer")))
		a.Then(a.To(stop, "declined").Ends("intentional declined result"))
		return New(d)
	}
	result := Fuzz(factory, FuzzConfig{MaxSteps: 5, Liveness: true, Progress: map[string][]json.RawMessage{"declaredAgent": {json.RawMessage(`true`)}}})
	if result.Finding == nil || result.Finding.Rule != "liveness" {
		t.Fatalf("no-fault stop passed: %s", result)
	}
}

func TestDeclaredStoppedRequiresRootEndProofAndActualFault(t *testing.T) {
	d := wf.New("proof")
	_ = wf.NewTerminal[declaredStop](d, wf.Stopped)
	raw, _ := json.Marshal(wf.RunEndedData{Terminal: "declaredStop", Outcome: wf.Stopped})
	valid := func() *fuzzRun {
		return &fuzzRun{sim: New(d), result: &FuzzResult{adversarialInjected: true, Status: wf.RunStatus{Run: "r", Phase: wf.RunEnded, Terminal: "declaredStop", Outcome: wf.Stopped}, Trace: &Trace{journal: []wf.JournalRecord{{Run: "r", State: "declaredStop", Kind: wf.JournalRunEnded, Data: raw}}}}}
	}
	if !valid().declaredStoppedAfterFault() {
		t.Fatal("valid declared root end rejected")
	}
	for _, change := range []func(*fuzzRun){
		func(f *fuzzRun) {
			f.result.adversarialInjected = false
			f.result.Switches = map[string]bool{"unused/effect-error": true}
		},
		func(f *fuzzRun) { f.result.Trace.journal = nil },
		func(f *fuzzRun) { f.result.Trace.journal[0].Instance = "child" },
		func(f *fuzzRun) { f.result.Trace.journal[0].Run = "other" },
		func(f *fuzzRun) { f.result.Trace.journal[0].State = "other" },
		func(f *fuzzRun) {
			f.result.Trace.journal[0].Data = json.RawMessage(`{"Terminal":"other","Outcome":"stopped"}`)
		},
		func(f *fuzzRun) { f.result.Status.Phase = wf.RunBlocked },
		func(f *fuzzRun) { f.result.Status.Terminal = "undeclared" },
	} {
		f := valid()
		change(f)
		if f.declaredStoppedAfterFault() {
			t.Fatalf("unproven end accepted: %+v", f.result)
		}
	}
}

func TestFaultMarkerExcludesDryValidationAndEnabledSwitches(t *testing.T) {
	cfg := FuzzConfig{MaxSteps: 4, Fault: &FuzzFault{State: "target", Adapter: "test", Op: "read", Class: "observation-error"}}
	f := &fuzzRun{sim: New(wf.New("marker")), result: &FuzzResult{config: cfg}, schemas: map[string]wf.StateView{}}
	f.setupPhases(cfg)
	if f.result.adversarialInjected {
		t.Fatal("configuration counted as injection")
	}
	w := f.sim.world
	at := wf.EffectContext{State: "target", Purpose: "check"}
	d := wf.ObservationDesc{Adapter: "test", Op: "read"}
	w.observationValidation(true, true)
	_, _ = w.ReadAt("", at, d)
	if f.result.adversarialInjected {
		t.Fatal("dry validation counted as actual fault")
	}
	w.observationValidation(false, false)
	_, _ = w.ReadAt("", at, d)
	if !f.result.adversarialInjected {
		t.Fatal("consumed fault not counted")
	}
}
