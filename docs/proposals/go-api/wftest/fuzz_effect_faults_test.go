package wftest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func effectSwitchRun(seed int64) *fuzzRun {
	f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{}, schemas: map[string]wf.StateView{"child/Work": {Kind: "command"}, "other/Work": {Kind: "compute"}}}
	f.setupPhases(FuzzConfig{Seed: seed, OutcomeSwitches: true, MaxSteps: 10})
	// These tests exercise returned faults; crash/restart is covered separately.
	delete(f.result.Switches, "child/Work/effect-uncertain")
	return f
}

func effectOutcome(raw json.RawMessage, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, wf.ErrUnavailable) {
		return "unavailable"
	}
	if err != nil {
		return "adverse"
	}
	var r wf.RunResult
	_ = json.Unmarshal(raw, &r)
	if r.Exit == 124 {
		return "timeout"
	}
	if r.Exit != 0 {
		return "adverse"
	}
	return "good"
}

func TestEffectSwitchesExerciseEveryEnabledClass(t *testing.T) {
	for _, effect := range []wf.ExternalEffect{{Adapter: "git", Op: "merge"}, {Adapter: "shell", Op: "run"}, {Adapter: "shell", Op: "run", Args: map[string]any{"Timeout": "1s"}}} {
		t.Run(fmt.Sprintf("%s.%s/%v", effect.Adapter, effect.Op, effect.Args), func(t *testing.T) {
			f := effectSwitchRun(9)
			for _, key := range []string{"effect-unavailable", "effect-timeout", "effect-adverse"} {
				f.result.Switches["child/Work/"+key] = true
			}
			seen := map[string]int{}
			for i := 0; i < 6; i++ {
				raw, err := f.sim.world.ApplyAt("", wf.EffectContext{Instance: "child#2", State: "child/Work"}, effect)
				seen[effectOutcome(raw, err)]++
			}
			if !reflect.DeepEqual(seen, map[string]int{"unavailable": 2, "timeout": 2, "adverse": 2}) {
				t.Fatalf("enabled faults starved: %v", seen)
			}
			f.sim.world.setFuzzGood(true)
			for i := 0; i < 6; i++ {
				raw, err := f.sim.world.ApplyAt("", wf.EffectContext{State: "child/Work"}, effect)
				if got := effectOutcome(raw, err); got != "good" {
					t.Fatalf("all-good injected %s", got)
				}
			}
		})
	}
}

func TestEffectScheduleIsDeterministicAndIndependentAcrossContexts(t *testing.T) {
	run := func(interleave bool) []string {
		f := effectSwitchRun(17)
		for key := range f.result.Switches {
			f.result.Switches[key] = true
		}
		var out []string
		for i := 0; i < 9; i++ {
			raw, err := f.sim.world.ApplyAt("", wf.EffectContext{State: "child/Work"}, wf.ExternalEffect{Adapter: "git", Op: "merge"})
			out = append(out, effectOutcome(raw, err))
			if interleave {
				_, _ = f.sim.world.ApplyAt("", wf.EffectContext{State: "other/Work"}, wf.ExternalEffect{Adapter: "git", Op: "merge"})
			}
		}
		return out
	}
	if a, b := run(false), run(true); !reflect.DeepEqual(a, b) {
		t.Fatalf("another context shifted seeded schedule: %v vs %v", a, b)
	}
}

func TestGateTimeoutSwitchOnlyForDeclaredTimeout(t *testing.T) {
	f := effectSwitchRun(2)
	f.schemas = map[string]wf.StateView{"Timed": {Kind: "gate", Edges: []wf.EdgeView{{Timeout: time.Second}}}, "Untimed": {Kind: "gate"}, "Command": {Kind: "command", Edges: []wf.EdgeView{{Timeout: time.Second}}}}
	f.setupPhases(FuzzConfig{Seed: 2, OutcomeSwitches: true, MaxSteps: 10})
	if _, ok := f.result.Switches["Timed/timeout"]; !ok {
		t.Fatal("timed gate has no timeout switch")
	}
	for _, key := range []string{"Untimed/timeout", "Command/timeout"} {
		if _, ok := f.result.Switches[key]; ok {
			t.Fatalf("undeclared gate timeout switch: %s", key)
		}
	}
}

type faultCommand struct{}
type faultPass struct{}
type faultFail struct{}
type faultSub struct{}

func TestDetachedFaultsUseNestedStateAndProduceCommandResults(t *testing.T) {
	for _, class := range []string{"effect-unavailable", "effect-timeout", "effect-adverse", "good"} {
		t.Run(class, func(t *testing.T) {
			child := wf.New("nested")
			command := wf.NewCommand[faultCommand](child, "check")
			pass := wf.NewTerminal[faultPass](child, wf.Success)
			fail := wf.NewTerminal[faultFail](child, wf.Stopped)
			command.Key("check").Idempotent().Detached(time.Second)
			yes, no := command.To(pass, "pass"), command.To(fail, "fail")
			command.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[faultCommand] {
				if result.Exit == 0 {
					return yes
				}
				return no
			}, yes, no)
			child.OnFailure(wf.Unavailable, fail)
			wf.Yield(pass, func(wf.Ctx) bool { return true })
			wf.Yield(fail, func(wf.Ctx) bool { return false })
			root := wf.New("root")
			sub := wf.NewSub[faultSub](root, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
			done := wf.NewTerminal[faultPass](root, wf.Success)
			stop := wf.NewTerminal[faultFail](root, wf.Stopped)
			sub.Exits(wf.OnExit[faultPass](sub.To(done, "pass")), wf.OnExit[faultFail](sub.To(stop, "fail")))
			original := NewFakeRunner()
			original.SetStatus(0, wf.DetachedPresent, wf.RunResult{Exit: 0})
			ticks := 0
			clock := WithClock(func() time.Time {
				ticks++
				return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(ticks) * time.Hour)
			})
			sim := New(root, WithTB(t), WithEffectRunner(original), clock)
			f := &fuzzRun{sim: sim, result: &FuzzResult{}, schemas: map[string]wf.StateView{"nested/faultCommand": {Kind: "command"}}}
			f.setupPhases(FuzzConfig{OutcomeSwitches: true, MaxSteps: 10})
			for key := range f.result.Switches {
				f.result.Switches[key] = false
			}
			if class != "good" {
				f.result.Switches["nested/faultCommand/"+class] = true
			}
			trace := sim.Run(Resume(), Resume())
			wantTerminal := "faultFail"
			if class == "good" {
				wantTerminal = "faultPass"
			}
			if trace.Terminal() != wantTerminal {
				t.Fatalf("%s terminal %q, trace %s", class, trace.Terminal(), trace)
			}
			found := false
			for _, record := range trace.Journal() {
				if record.Kind != wf.JournalEffectResult {
					continue
				}
				found = true
				var data wf.EffectResultData
				if err := json.Unmarshal(record.Data, &data); err != nil {
					t.Fatal(err)
				}
				if class == "effect-timeout" && data.Outcome != "timed out" {
					t.Fatalf("timeout did not exercise engine expiry: %+v", data)
				}
				if class == "effect-unavailable" {
					if data.Outcome != "unavailable" || data.OK {
						t.Fatalf("unavailable result %+v", data)
					}
				} else {
					var result wf.RunResult
					if err := json.Unmarshal(data.Result, &result); err != nil {
						t.Fatal(err)
					}
					want := map[string]int{"effect-timeout": 124, "effect-adverse": 1, "good": 0}[class]
					if result.Exit != want {
						t.Fatalf("%s exit %d, want %d", class, result.Exit, want)
					}
				}
			}
			if !found {
				t.Fatal("no journaled effect result")
			}
		})
	}
}

func TestDetachedFaultRecoveryReplaysAndDisablesLaterFaults(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("detached-recovery")
		command := wf.NewCommand[faultCommand](d, "check")
		done := wf.NewTerminal[faultPass](d, wf.Success)
		command.Key("check").Idempotent().Detached(time.Second)
		pass, retry := command.To(done, "pass"), command.To(command, "retry")
		command.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[faultCommand] {
			if result.Exit != 0 {
				return retry
			}
			return pass
		}, pass, retry)
		d.OnFailure(wf.Unavailable, command)
		runner := NewFakeRunner()
		runner.SetStatus(0, wf.DetachedPresent, wf.RunResult{Exit: 0})
		return New(d, WithEffectRunner(runner))
	}
	for _, class := range []string{"effect-unavailable", "effect-timeout", "effect-adverse"} {
		t.Run(class, func(t *testing.T) {
			result := Fuzz(factory, FuzzConfig{Seed: 23, MaxSteps: 12, Fault: &FuzzFault{State: "faultCommand", Adapter: "shell", Op: "run", Class: class}})
			if !result.FaultInjected || result.Finding != nil || result.Status.Outcome != wf.Success {
				t.Fatalf("fault recovery due=%+v status=%s: %s", result.Status.Due, result.Status.Phase, result)
			}
			again := result.Replay(factory)
			if !again.FaultInjected || again.Finding != nil || again.Status.Outcome != wf.Success {
				t.Fatalf("fault replay: %s", again)
			}
			outcomes := func(r *FuzzResult) []string {
				var out []string
				for _, record := range r.Trace.Journal() {
					if record.Kind == wf.JournalEffectResult {
						out = append(out, string(record.Data))
					}
				}
				return out
			}
			if !reflect.DeepEqual(outcomes(result), outcomes(again)) {
				t.Fatalf("effect replay differs: %v vs %v", outcomes(result), outcomes(again))
			}
		})
	}
}

func TestEffectRecoveryRetainsFaultThenMakesFollowingCallsGood(t *testing.T) {
	f := effectSwitchRun(3)
	for key := range f.result.Switches {
		f.result.Switches[key] = true
	}
	calls := 0
	f.sim.world.fuzzRecover = func(at wf.EffectContext, class string) bool { calls++; return true }
	for i := 0; i < 4; i++ {
		raw, err := f.sim.world.ApplyAt("", wf.EffectContext{State: "child/Work"}, wf.ExternalEffect{Adapter: "git", Op: "merge"})
		outcome := effectOutcome(raw, err)
		if i == 0 && outcome == "good" {
			t.Fatal("recovery erased the injected fault")
		}
		if i > 0 && outcome != "good" {
			t.Fatalf("following effect still faulty: %s", outcome)
		}
	}
	if calls != 1 {
		t.Fatalf("recovery callbacks %d, want one", calls)
	}
}
