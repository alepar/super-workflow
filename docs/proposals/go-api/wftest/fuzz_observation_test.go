package wftest

import (
	"encoding/json"
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"reflect"
	"testing"
)

type observationFirst struct{}
type observationLater struct{}
type observationDone struct{}

func TestObservationFaultTargetsLaterOwnerAndReplays(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("observation-fault")
		obs := wf.Observe[bool]("test", "read", nil)
		first := wf.NewCompute[observationFirst, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, obs) }).Observes(obs)
		later := wf.NewCompute[observationLater, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, obs) }).Observes(obs)
		end := wf.NewTerminal[observationDone](d, wf.Success)
		later.Retry(wf.Retry[observationLater](wf.Const(2), later.To(end, "stop")))
		first.Then(first.To(later, "next"))
		later.Then(later.To(end, "done"))
		d.OnFailure(wf.Unavailable, later)
		d.OnFailure(wf.Blocked, later)
		w := NewWorld()
		Set(w, obs, true)
		return New(d, WithWorld(w))
	}
	for _, class := range []string{"observation-unavailable", "observation-error"} {
		result := Fuzz(factory, FuzzConfig{Seed: 3, MaxSteps: 8, Fault: &FuzzFault{State: "observationLater", Adapter: "test", Op: "read", Class: class}})
		if !result.FaultInjected || result.Finding != nil || result.Status.Outcome != wf.Success {
			t.Fatalf("fault: %s", result)
		}
		faults := 0
		for _, r := range result.Trace.Journal() {
			if r.Kind == wf.JournalObserved {
				var d wf.ObservedData
				_ = json.Unmarshal(r.Data, &d)
				if d.Error != "" {
					faults++
					if r.State != "observationLater" {
						t.Fatalf("wrong owner: %+v", r)
					}
				}
			}
		}
		if faults != 1 {
			t.Fatalf("fault count %d", faults)
		}
		replay := result.Replay(factory)
		if !replay.FaultInjected || replay.Finding != nil || !reflect.DeepEqual(result.Trace.Journal(), replay.Trace.Journal()) {
			t.Fatalf("replay: %s", replay)
		}
	}
}

func TestObservationFaultKeepsUnknownReadAndLegacyBehavior(t *testing.T) {
	w := NewWorld()
	obs := wf.Observe[bool]("test", "read", nil)
	Set(w, obs, true)
	w.fuzzObservation = func(at wf.EffectContext, d wf.ObservationDesc) string {
		if at.State == "target" && d.Op == "read" {
			return "observation-unavailable"
		}
		return ""
	}
	if _, err := w.ReadAt("", wf.EffectContext{State: "target"}, obs.Desc()); !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("fault: %v", err)
	}
	if v, err := w.Read("", obs.Desc()); err != nil || string(v) != "true" {
		t.Fatalf("legacy read: %s %v", v, err)
	}
	if _, err := w.ReadAt("", wf.EffectContext{State: "target"}, wf.ObservationDesc{Adapter: "test", Op: "unknown"}); err == nil || errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("unknown op fallback: %v", err)
	}
}

func TestObservationFaultSurvivesValidateUntilDone(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("observed-check")
		obs := wf.Observe[bool]("test", "read", nil)
		agent := wf.NewAgent[observationLater, bool](d)
		end := wf.NewTerminal[observationDone](d, wf.Success)
		agent.Spec(wf.PromptText("review"), wf.Retry[observationLater](wf.Const(2), agent.To(end, "stop")).OnClass(wf.Unavailable, agent.To(agent, "retry observation")), wf.Checkf("observation", func(_ bool, c wf.Ctx) error { _, err := wf.Observed(c, obs); return err }).Observes(obs))
		agent.Then(agent.To(end, "done"))
		w := NewWorld()
		Set(w, obs, true)
		return New(d, WithWorld(w))
	}
	for _, class := range []string{"observation-unavailable", "observation-error"} {
		result := Fuzz(factory, FuzzConfig{Seed: 1, MaxSteps: 8, Valid: map[string][]json.RawMessage{"observationLater": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"observationLater": {json.RawMessage(`true`)}}, Fault: &FuzzFault{State: "observationLater", Adapter: "test", Op: "read", Class: class}})
		if !result.FaultInjected || result.Finding != nil || result.Status.Outcome != wf.Success {
			t.Fatalf("fault: %s", result)
		}
		faults := 0
		for _, r := range result.Trace.Journal() {
			if r.Kind == wf.JournalObserved {
				var d wf.ObservedData
				_ = json.Unmarshal(r.Data, &d)
				if d.Error != "" {
					faults++
					if d.Purpose != "check" {
						t.Fatalf("fault not consumed by check: %+v", d)
					}
				}
			}
		}
		if faults != 1 {
			t.Fatalf("fault disappeared between validate/done: %d", faults)
		}
		replay := result.Replay(factory)
		if !replay.FaultInjected || replay.Finding != nil || !reflect.DeepEqual(observationJournal(result), observationJournal(replay)) {
			t.Fatalf("replay: %s", replay)
		}
	}
}

func TestObservationFaultValidationDoesNotStartRecovery(t *testing.T) {
	cfg := FuzzConfig{MaxSteps: 8, Fault: &FuzzFault{State: "later", Adapter: "test", Op: "read", Class: "observation-error"}}
	f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{config: cfg}, schemas: map[string]wf.StateView{}}
	f.setupPhases(cfg)
	w := f.sim.world
	at := wf.EffectContext{State: "later", Purpose: "check"}
	d := wf.ObservationDesc{Adapter: "test", Op: "read"}
	w.observationValidation(true, true)
	_, err := w.ReadAt("", at, d)
	if err == nil || f.result.FaultInjected || f.good {
		t.Fatalf("validation started recovery: %v %+v", err, f.result)
	}
	w.observationValidation(false, false)
	_, err = w.ReadAt("", at, d)
	if err == nil || !f.result.FaultInjected || !f.good {
		t.Fatalf("actual read did not retain fault and recover: %v %+v", err, f.result)
	}
}

func observationJournal(result *FuzzResult) []wf.JournalRecord {
	var out []wf.JournalRecord
	for _, r := range result.Trace.Journal() {
		if r.Kind == wf.JournalObserved {
			out = append(out, r)
		}
	}
	return out
}

func TestObservationFaultCommandDoneWhenAndDiscovery(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("command-observation")
		obs := wf.Observe[bool]("test", "done", nil)
		command := wf.NewCommand[observationLater](d, "check").Idempotent()
		end := wf.NewTerminal[observationDone](d, wf.Success)
		command.Then(command.To(end, "done"))
		command.Retry(wf.Retry[observationLater](wf.Const(2), command.To(end, "stop")))
		wf.DoneWhen(command, obs, "already complete")
		d.OnFailure(wf.Unavailable, command)
		w := NewWorld()
		Set(w, obs, false)
		kinds := wf.Inspect(d).States[0].ObservationKinds()
		if !reflect.DeepEqual(kinds, []wf.EffectKind{{Adapter: "test", Op: "done"}}) {
			t.Fatalf("donewhen discovery: %+v", kinds)
		}
		return New(d, WithWorld(w))
	}
	for _, class := range []string{"observation-unavailable", "observation-error"} {
		result := Fuzz(factory, FuzzConfig{Seed: 1, MaxSteps: 8, Fault: &FuzzFault{State: "observationLater", Adapter: "test", Op: "done", Class: class}})
		if !result.FaultInjected || result.Finding != nil || result.Status.Outcome != wf.Success {
			t.Fatalf("donewhen fault: %s", result)
		}
		found := false
		for _, r := range observationJournal(result) {
			var d wf.ObservedData
			_ = json.Unmarshal(r.Data, &d)
			if d.Error != "" {
				found = true
				if d.Purpose != "done_when" {
					t.Fatalf("purpose: %+v", d)
				}
			}
		}
		if !found {
			t.Fatal("missing DoneWhen fault")
		}
	}
}

func TestObservationPreludeSnapshotDoesNotRecoverPrimary(t *testing.T) {
	cfg := FuzzConfig{MaxSteps: 8, PreludeFault: &FuzzFault{State: "later", Adapter: "test", Op: "prepare", Class: "observation-error"}, Fault: &FuzzFault{State: "later", Adapter: "test", Op: "read", Class: "observation-error"}}
	f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{config: cfg}, schemas: map[string]wf.StateView{}}
	f.setupPhases(cfg)
	w := f.sim.world
	at := wf.EffectContext{State: "later", Purpose: "check"}
	prelude := wf.ObservationDesc{Adapter: "test", Op: "prepare"}
	primary := wf.ObservationDesc{Adapter: "test", Op: "read"}
	w.observationValidation(true, true)
	_, _ = w.ReadAt("", at, prelude)
	_, _ = w.ReadAt("", at, primary)
	w.observationValidation(false, false)
	if _, err := w.ReadAt("", at, prelude); err == nil || f.good || f.result.FaultInjected {
		t.Fatalf("prelude recovered primary: %v %+v", err, f.result)
	}
	if _, err := w.ReadAt("", at, primary); err == nil || !f.good || !f.result.FaultInjected {
		t.Fatalf("primary recovery: %v %+v", err, f.result)
	}
}

func TestSharedCheckAndInputObservationKeepsReadSequence(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("shared-observation")
		obs := wf.Observe[bool]("test", "read", nil)
		agent := wf.NewAgent[observationLater, bool](d).Observes(obs)
		end := wf.NewTerminal[observationDone](d, wf.Success)
		agent.Spec(wf.PromptText("review"), wf.Retry[observationLater](wf.Const(2), agent.To(end, "stop")).OnClass(wf.Unavailable, agent.To(agent, "retry")), wf.Checkf("observation", func(_ bool, c wf.Ctx) error { _, err := wf.Observed(c, obs); return err }).Observes(obs))
		agent.Then(agent.To(end, "done"))
		w := NewWorld()
		Set(w, obs, true)
		return New(d, WithWorld(w))
	}
	for _, class := range []string{"observation-error", "observation-unavailable"} {
		cfg := FuzzConfig{Seed: 3, MaxSteps: 8, Valid: map[string][]json.RawMessage{"observationLater": {json.RawMessage(`true`)}}, Progress: map[string][]json.RawMessage{"observationLater": {json.RawMessage(`true`)}}, Fault: &FuzzFault{State: "observationLater", Adapter: "test", Op: "read", Class: class}}
		result := Fuzz(factory, cfg)
		if result.Finding != nil || !result.FaultInjected || result.Status.Outcome != wf.Success {
			t.Fatalf("shared observation parity: %s", result)
		}
		var checks []wf.ObservedData
		for _, r := range observationJournal(result) {
			var d wf.ObservedData
			_ = json.Unmarshal(r.Data, &d)
			if d.Purpose == "check" {
				checks = append(checks, d)
			}
		}
		if len(checks) != 2 || checks[0].Error == "" || checks[1].Error != "" || string(checks[1].Value) != "true" {
			t.Fatalf("fault sequence: %+v", checks)
		}
		replay := result.Replay(factory)
		if replay.Finding != nil || !reflect.DeepEqual(observationJournal(result), observationJournal(replay)) {
			t.Fatalf("shared observation replay: %s", replay)
		}
	}
}

func TestObservationSnapshotRetainsHealthyOccurrences(t *testing.T) {
	w := NewWorld()
	obs := wf.Observe[bool]("test", "read", nil)
	Set(w, obs, true)
	calls := 0
	w.fuzzObservation = func(wf.EffectContext, wf.ObservationDesc) string {
		calls++
		if calls == 2 {
			return "observation-error"
		}
		return ""
	}
	at := wf.EffectContext{State: "target", Purpose: "check"}
	read := func() []bool {
		var failed []bool
		for i := 0; i < 3; i++ {
			_, err := w.ReadAt("", at, obs.Desc())
			failed = append(failed, err != nil)
		}
		return failed
	}
	w.observationValidation(true, true)
	validated := read()
	if !reflect.DeepEqual(validated, []bool{false, true, false}) || calls != 3 {
		t.Fatalf("validation read occurrences: %v calls=%d", validated, calls)
	}
	w.observationValidation(false, false)
	submitted := read()
	if !reflect.DeepEqual(submitted, validated) || calls != 3 {
		t.Fatalf("done sequence: %v vs %v calls=%d", submitted, validated, calls)
	}
	if w.observationFaultPending() {
		t.Fatal("consumed snapshot retained pending faults")
	}
}
