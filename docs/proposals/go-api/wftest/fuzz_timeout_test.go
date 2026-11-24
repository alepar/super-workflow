package wftest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func timedGateFactory() *wftest.Sim {
	d := wf.New("timed")
	gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
	done := wf.NewTerminal[fuzzDone](d, wf.Success)
	expired := wf.NewTerminal[fuzzSpin](d, wf.Success)
	gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "approved").Ends(wf.Success, "the answer selects this outcome"))).Timeout(7*time.Minute, gate.To(expired, "expired"))
	return wftest.New(d, wftest.WithClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }))
}

func TestFuzzTimeoutReplaysDeclaredGateDeadline(t *testing.T) {
	result := wftest.Fuzz(timedGateFactory, wftest.FuzzConfig{Seed: 2, MaxSteps: 8, Prefix: []wftest.FuzzAction{{State: "fuzzStop", Kind: wftest.FuzzTimeout}}})
	if result.Finding != nil || result.Status.Terminal != "fuzzSpin" {
		t.Fatalf("timeout did not take declared edge: %s", result)
	}
	if len(result.Script) != 1 || result.Script[0].Kind != wftest.FuzzTimeout {
		t.Fatalf("missing timeout action: %+v", result.Script)
	}
	timed := false
	for _, record := range result.Trace.Journal() {
		if record.Kind != wf.JournalAutoAnswered {
			continue
		}
		var data wf.AutoAnsweredData
		if json.Unmarshal(record.Data, &data) == nil && data.Answer == "timeout" {
			timed = true
			if !record.At.Equal(time.Date(2026, 1, 1, 0, 7, 0, 0, time.UTC)) {
				t.Fatalf("timeout at %v, want declared seven-minute deadline", record.At)
			}
		}
	}
	if !timed {
		t.Fatal("engine did not journal timeout")
	}
	replay := result.Replay(timedGateFactory)
	if replay.Finding != nil || replay.Status.Terminal != result.Status.Terminal || !replay.ReplayEqual {
		t.Fatalf("timeout replay differs: %s", replay)
	}
}

func TestFuzzTimeoutSwitchAndExplicitAnswerPrecedence(t *testing.T) {
	found := false
	for seed := int64(0); seed < 32; seed++ {
		cfg := wftest.FuzzConfig{Seed: seed, MaxSteps: 8, OutcomeSwitches: true}
		result := wftest.Fuzz(timedGateFactory, cfg)
		if !result.Switches["fuzzStop/timeout"] {
			continue
		}
		found = true
		if result.Finding != nil || result.Status.Terminal != "fuzzSpin" {
			t.Fatalf("enabled timeout ignored: %s", result)
		}
		cfg.GateAnswers = map[string]string{"fuzzStop": "yes"}
		explicit := wftest.Fuzz(timedGateFactory, cfg)
		if explicit.Finding != nil || explicit.Status.Terminal != "fuzzDone" {
			t.Fatalf("timeout overrode answer: %s", explicit)
		}
		cfg.Liveness = true
		cfg.LivenessAfter = 0
		good := wftest.Fuzz(timedGateFactory, cfg)
		if good.Finding != nil || good.End != "human gate" || len(good.Script) != 0 {
			t.Fatalf("all-good did not stop at human gate: %s", good)
		}
		break
	}
	if !found {
		t.Fatal("no gate timeout switch sampled")
	}
}

func TestFuzzTimeoutRejectsGateWithoutDeadline(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("untimed")
		gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "approved").Ends(wf.Success, "the answer selects this outcome")))
		return wftest.New(d)
	}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{Prefix: []wftest.FuzzAction{{State: "fuzzStop", Kind: wftest.FuzzTimeout}}})
	if result.Finding == nil || result.Finding.Rule != "script-mismatch" {
		t.Fatalf("undeclared timeout accepted: %s", result)
	}
}

func TestFuzzTimeoutTargetRecoversWithProgress(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("timeout-repair")
		gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		repair := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "approved").Ends(wf.Success, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(repair, "repair expired gate"))
		repair.Spec(wf.PromptText("repair"), wf.Retry[fuzzAgent](wf.Const(1), repair.To(done, "exhausted")))
		repair.Then(repair.To(done, "repaired"))
		return wftest.New(d)
	}
	cfg := wftest.FuzzConfig{MaxSteps: 8, Fault: &wftest.FuzzFault{State: "fuzzStop", Class: "timeout"}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}
	result := wftest.Fuzz(factory, cfg)
	if result.Finding != nil || result.Status.Terminal != "fuzzDone" {
		t.Fatalf("timeout recovery failed: %s", result)
	}
	if len(result.Script) != 2 || result.Script[0].Kind != wftest.FuzzTimeout || result.Script[1].Kind != wftest.FuzzSubmit {
		t.Fatalf("missing timeout then progress: %+v", result.Script)
	}
	replay := result.Replay(factory)
	if replay.Finding != nil || replay.Status.Terminal != result.Status.Terminal {
		t.Fatalf("timeout recovery replay failed: %s", replay)
	}
}

func TestFuzzTimeoutOneStepRecoveryDrivesTimeoutEdge(t *testing.T) {
	for _, success := range []bool{false, true} {
		factory := func() *wftest.Sim {
			d := wf.New("timeout-bound")
			gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.PolicyAnswerable)
			done := wf.NewTerminal[fuzzDone](d, wf.Success)
			outcome := wf.Stopped
			if success {
				outcome = wf.Success
			}
			timed := wf.NewTerminal[fuzzSpin](d, outcome)
			gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "approved").Ends(wf.Success, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(timed, "expired"))
			return wftest.New(d)
		}
		result := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 1, LivenessSteps: 1, Fault: &wftest.FuzzFault{State: "fuzzStop", Class: "timeout"}})
		if result.Status.Terminal != "fuzzSpin" {
			t.Fatalf("success=%v timeout edge was not driven: terminal=%q finding=%v", success, result.Status.Terminal, result.Finding)
		}
		traversed := false
		for _, record := range result.Trace.Journal() {
			if record.Kind != wf.JournalAutoAnswered {
				continue
			}
			var data wf.AutoAnsweredData
			if json.Unmarshal(record.Data, &data) == nil && data.Answer == "timeout" {
				traversed = true
			}
		}
		if !traversed {
			t.Fatalf("success=%v timeout not journaled", success)
		}
		if success && result.Finding != nil {
			t.Fatalf("successful timeout failed: %s", result)
		}
		if !success && (result.Finding == nil || result.Finding.Rule != "liveness") {
			t.Fatalf("stopped timeout passed: %s", result)
		}
	}
}

func TestFuzzReservedGateDeclaredTimeoutIsIntentionalBoundary(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		for _, marked := range []bool{false, true} {
			factory := func() *wftest.Sim {
				d := wf.New("reserved-timeout")
				reservation := wf.PolicyAnswerable
				if reserved {
					reservation = wf.Reserved
				}
				gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), reservation)
				kept := wf.NewTerminal[fuzzDone](d, wf.Stopped)
				edge := gate.To(kept, "keep after timeout")
				if marked {
					edge = edge.Ends("human decision window elapsed; retain work")
				}
				gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(kept, "keep").Ends(wf.Stopped, "the answer selects this outcome"))).Timeout(time.Minute, edge)
				return wftest.New(d)
			}
			got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 1, LivenessSteps: 1, Fault: &wftest.FuzzFault{State: "fuzzStop", Class: "timeout"}})
			allowed := reserved
			if allowed && got.Finding != nil {
				t.Fatalf("intentional reserved timeout failed: %s", got)
			}
			if !allowed && (got.Finding == nil || got.Finding.Rule != "liveness") {
				t.Fatalf("reserved=%v marked=%v unexpected pass: %s", reserved, marked, got)
			}
			if got.Status.Terminal != "fuzzDone" || !got.FaultInjected {
				t.Fatalf("timeout was not actually driven: %s", got)
			}
			replay := got.Replay(factory)
			if replay.Status.Terminal != got.Status.Terminal || (replay.Finding == nil) != allowed {
				t.Fatalf("timeout replay changed verdict: %s", replay)
			}
		}
	}
}

func TestCheckCoversFailureOnlyGateTimeoutWithAdversePrelude(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("failure-gate")
		command := wf.NewCommand[fuzzSpin](d, "check")
		gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "recover?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		kept := wf.NewTerminal[fuzzAgent](d, wf.Stopped)
		good, bad := command.To(done, "green"), command.To(gate, "needs recovery")
		command.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[fuzzSpin] {
			if result.Exit != 0 {
				return bad
			}
			return good
		}, good, bad)
		d.OnFailure(wf.Unavailable, gate)
		d.OnFailure(wf.Blocked, gate)
		d.OnFailure(wf.Uncertain, gate)
		gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "recovered").Ends(wf.Success, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(kept, "retain after timeout").Ends("human decision window elapsed"))
		return wftest.New(d)
	}
	report := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 16, MaxSteps: 8}})
	for _, target := range report.UnreachedFaults {
		if strings.Contains(target, "fuzzStop timeout") {
			t.Fatalf("failure-only gate timeout not reached: %v", report.Problems)
		}
	}
	covered := false
	for key, count := range report.Coverage.EdgesReached {
		if strings.Contains(key, "/fuzzStop/") && strings.Contains(key, "edge[1]") && count > 0 {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("timeout edge not actually covered: %v", report.Coverage.EdgeProblems())
	}
	for _, finding := range report.Findings {
		if finding.Finding.Rule == "liveness" {
			t.Fatalf("reserved timeout produced false liveness finding: %s", finding.Finding)
		}
	}
}

func TestFuzzReservedTimeoutDoesNotExcuseBrokenRepair(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("broken-timeout-repair")
		gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		repair := wf.NewCompute[fuzzSpin, bool](d, func(wf.Ctx) (bool, error) { return false, nil })
		stop := wf.NewTerminal[fuzzDone](d, wf.Stopped)
		gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(stop, "keep").Ends(wf.Stopped, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(repair, "repair").Ends("repair elapsed decision"))
		repair.Then(repair.To(stop, "broken repair"))
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 2, Fault: &wftest.FuzzFault{State: "fuzzStop", Class: "timeout"}})
	if got.Finding == nil || got.Finding.Rule != "liveness" {
		t.Fatalf("upstream reserved gate hid broken repair: %s", got)
	}
}

func TestCheckTriesProgressBeforeAdverseTimeoutPrelude(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("happy-gate")
		command := wf.NewCommand[fuzzSpin](d, "check")
		gate := wf.NewGate[fuzzStop, fuzzVerdict](d, "approve?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		kept := wf.NewTerminal[fuzzAgent](d, wf.Stopped)
		good, bad := command.To(gate, "ready"), command.To(kept, "command failed").Ends("retain failed checkout")
		command.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[fuzzSpin] {
			if result.Exit != 0 {
				return bad
			}
			return good
		}, good, bad)
		d.OnFailure(wf.Unavailable, kept)
		d.OnFailure(wf.Blocked, kept)
		gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(done, "approved").Ends(wf.Success, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(kept, "retain after timeout").Ends("human decision window elapsed"))
		return wftest.New(d)
	}
	report := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, MaxSteps: 8}})
	for _, target := range report.UnreachedFaults {
		if strings.Contains(target, "fuzzStop timeout") {
			t.Fatalf("happy-path gate hidden by adverse prelude: %v", report.Problems)
		}
	}
	if report.Coverage.EdgesReached["happy-gate/fuzzStop/edge[1]"] == 0 {
		t.Fatalf("happy-path timeout not traversed: %v", report.Coverage.EdgeProblems())
	}
}

func TestFuzzNestedReservedTimeoutProvesChildSettlement(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		factory := func() *wftest.Sim {
			child := wf.New("timeout-child")
			gate := wf.NewGate[fuzzStop, fuzzVerdict](child, "continue?", wf.NewEnum(child, "answer", fuzzVerdict("yes")), wf.Reserved)
			kept := wf.NewTerminal[fuzzDone](child, wf.Stopped)
			gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(kept, "keep").Ends(wf.Stopped, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(kept, "expired"))
			wf.Yield(kept, func(wf.Ctx) bool { return false })
			parent := wf.New("timeout-parent")
			if mapped {
				fan := wf.NewMap[fuzzFan, int, int, bool](parent, wf.FromValues(1), func(i int) int { return i }, wf.Typed[int, bool](child))
				stopped := wf.NewTerminal[fuzzSpin](parent, wf.Stopped)
				fan.Join(wf.AllOK(), fan.To(stopped, "all settled"), fan.To(stopped, "kept child"))
			} else {
				sub := wf.NewSub[fuzzFan](parent, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
				stopped := wf.NewTerminal[fuzzSpin](parent, wf.Stopped)
				sub.Exits(wf.OnExit[fuzzDone](sub.To(stopped, "kept child")))
			}
			return wftest.New(parent)
		}
		got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, Fault: &wftest.FuzzFault{State: "timeout-child/fuzzStop", Class: "timeout"}})
		if got.Finding != nil || !got.FaultInjected || got.Status.Terminal != "fuzzSpin" {
			t.Fatalf("mapped=%v nested reserved timeout failed: %s", mapped, got)
		}
		settled := false
		for _, record := range got.Trace.Journal() {
			if record.Kind != wf.JournalChildSettled {
				continue
			}
			var data wf.ChildSettledData
			if json.Unmarshal(record.Data, &data) == nil && data.Terminal == "fuzzDone" && data.Outcome == wf.Stopped && data.Class == wf.Refused && !data.Cancelled && !data.Running {
				settled = true
			}
		}
		if !settled {
			t.Fatalf("mapped=%v no genuine stopped terminal child settlement", mapped)
		}
		replay := got.Replay(factory)
		if replay.Finding != nil || replay.Status.Terminal != got.Status.Terminal {
			t.Fatalf("mapped=%v replay failed: %s", mapped, replay)
		}
	}
}

func TestFuzzNestedTimeoutEndsTargetedLivenessAtChildSettlement(t *testing.T) {
	for _, parentPanics := range []bool{false, true} {
		factory := func() *wftest.Sim {
			child := wf.New("timeout-child")
			gate := wf.NewGate[fuzzStop, fuzzVerdict](child, "continue?", wf.NewEnum(child, "answer", fuzzVerdict("yes")), wf.Reserved)
			kept := wf.NewTerminal[fuzzDone](child, wf.Stopped)
			gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(kept, "keep").Ends(wf.Stopped, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(kept, "expired"))
			wf.Yield(kept, func(wf.Ctx) bool { return false })
			parent := wf.New("timeout-parent")
			sub := wf.NewSub[fuzzFan](parent, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
			repair := wf.NewCompute[fuzzSpin, bool](parent, func(wf.Ctx) (bool, error) {
				if parentPanics {
					panic("parent report failed")
				}
				return false, nil
			})
			stopped := wf.NewTerminal[fuzzAgent](parent, wf.Stopped)
			sub.Exits(wf.OnExit[fuzzDone](sub.To(repair, "report child outcome")))
			repair.Then(repair.To(stopped, "report complete"))
			return wftest.New(parent)
		}
		got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, Fault: &wftest.FuzzFault{State: "timeout-child/fuzzStop", Class: "timeout"}})
		if !parentPanics && got.Finding != nil {
			t.Fatalf("parent terminal invalidated completed child boundary: %s", got)
		}
		if parentPanics && (got.Finding == nil || got.Finding.Rule != "definition-panic") {
			t.Fatalf("independent parent fault was hidden: %s", got)
		}
	}
}

func TestFuzzSettledChildBoundarySuppressesOnlyLaterLivenessFindings(t *testing.T) {
	for _, suffix := range []string{"wait", "step-bound", "journal-bound"} {
		t.Run(suffix, func(t *testing.T) {
			factory := func() *wftest.Sim {
				child := wf.New("timeout-child")
				gate := wf.NewGate[fuzzStop, fuzzVerdict](child, "continue?", wf.NewEnum(child, "answer", fuzzVerdict("yes")), wf.Reserved)
				kept := wf.NewTerminal[fuzzDone](child, wf.Stopped)
				gate.Answers(wf.On(fuzzVerdict("yes"), gate.To(kept, "keep").Ends(wf.Stopped, "the answer selects this outcome"))).Timeout(time.Minute, gate.To(kept, "expired"))
				wf.Yield(kept, func(wf.Ctx) bool { return false })
				parent := wf.New("timeout-parent")
				sub := wf.NewSub[fuzzFan](parent, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
				world := wftest.NewWorld()
				switch suffix {
				case "wait":
					wait := wf.NewWait[fuzzSpin](parent, git.Head("."), "head exists", func(head string) bool { return head != "" })
					done := wf.NewTerminal[fuzzAgent](parent, wf.Success)
					sub.Exits(wf.OnExit[fuzzDone](sub.To(wait, "wait after child")))
					wait.Ready(wait.To(done, "ready"))
					wftest.Set(world, git.Head("."), "")
				case "step-bound":
					next := wf.NewAgent[fuzzSpin, bool](parent)
					done := wf.NewTerminal[fuzzAgent](parent, wf.Success)
					sub.Exits(wf.OnExit[fuzzDone](sub.To(next, "later work")))
					next.Spec(wf.PromptText("later"), wf.Retry[fuzzSpin](wf.Const(1), next.To(done, "exhausted")))
					next.Then(next.To(next, "another step"))
				case "journal-bound":
					next := wf.NewCompute[fuzzSpin, bool](parent, func(wf.Ctx) (bool, error) { return true, nil })
					sub.Exits(wf.OnExit[fuzzDone](sub.To(next, "later work")))
					next.Then(next.To(next, "another step"))
				}
				return wftest.New(parent, wftest.WithWorld(world))
			}
			bound := 1
			if suffix == "wait" {
				bound = 8
			}
			result := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, LivenessSteps: bound, Fault: &wftest.FuzzFault{State: "timeout-child/fuzzStop", Class: "timeout"}, Progress: map[string][]json.RawMessage{"fuzzSpin": {json.RawMessage(`true`)}}})
			if result.Finding != nil && result.Finding.Rule == "liveness" {
				t.Fatalf("completed child boundary reopened: %s", result.Finding)
			}
			settled := false
			for _, record := range result.Trace.Journal() {
				if record.Kind == wf.JournalChildSettled {
					settled = true
				}
			}
			if !result.FaultInjected || !settled {
				t.Fatalf("missing actual child timeout settlement: %s", result)
			}
			// Independent suffix properties (for example stall-unnamed at the
			// bound) remain findings; only this gate's liveness is satisfied.
		})
	}
}
