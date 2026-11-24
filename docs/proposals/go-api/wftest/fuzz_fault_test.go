package wftest_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"strings"
	"testing"
)

// The adversarial command must actually run before recovery starts; selecting
// an all-good checkpoint before it would hide the failure. Both repair to
// success and the declared stopped terminal are valid fault outcomes.
func TestFaultRecoveryDoesNotEraseTheFault(t *testing.T) {
	factory := func(repair bool) func() *wftest.Sim {
		return func() *wftest.Sim {
			d := wf.New("fault-recovery")
			c := wf.NewCommand[fuzzSpin](d, "check")
			done := wf.NewTerminal[fuzzDone](d, wf.Success)
			stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
			good, bad := c.To(done, "green"), c.To(stop, "red").Ends("retain failed command for inspection")
			if repair {
				bad = c.To(c, "retry")
			}
			c.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[fuzzSpin] {
				if r.Exit != 0 {
					return bad
				}
				return good
			}, good, bad)
			return wftest.New(d)
		}
	}
	for _, repair := range []bool{false, true} {
		cfg := wftest.FuzzConfig{MaxSteps: 8, Fault: &wftest.FuzzFault{State: "fuzzSpin", Adapter: "shell", Op: "run", Class: "effect-adverse"}}
		got := wftest.Fuzz(factory(repair), cfg)
		if !got.FaultInjected {
			t.Fatalf("fault never exercised: %s", got)
		}
		if repair {
			if got.Finding != nil || got.Status.Outcome != wf.Success {
				t.Fatalf("repair failed: %s", got)
			}
		} else if got.Finding != nil || got.Status.Phase != wf.RunEnded || got.Status.Terminal != "fuzzStop" || got.Status.Outcome != wf.Stopped {
			t.Fatalf("declared fault stop rejected: %s", got)
		}
		faultRecorded := false
		rootEndRecorded := false
		for _, record := range got.Trace.Journal() {
			if record.Kind == wf.JournalEffectResult && record.State == "fuzzSpin" {
				var data wf.EffectResultData
				_ = json.Unmarshal(record.Data, &data)
				var result wf.RunResult
				_ = json.Unmarshal(data.Result, &result)
				faultRecorded = faultRecorded || result.Exit == 1
			}
			if record.Kind == wf.JournalRunEnded && record.Instance == "" && record.State == got.Status.Terminal {
				rootEndRecorded = true
			}
		}
		if !faultRecorded || !rootEndRecorded {
			t.Fatalf("fault/end proof missing: %s", got)
		}
		again := got.Replay(factory(repair))
		if again.FaultInjected != got.FaultInjected || again.Status.Outcome != got.Status.Outcome || (again.Finding == nil) != (got.Finding == nil) {
			t.Fatalf("fault replay diverged: %s", again)
		}
	}
}

func TestCheckReportsUnresolvedEffectAsHumanBoundary(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("adverse")
		c := wf.NewCommand[fuzzSpin](d, "check")
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		good, bad := c.To(done, "green"), c.To(stop, "red").Ends("retain failed checkout")
		c.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[fuzzSpin] {
			if r.Exit != 0 {
				return bad
			}
			return good
		}, good, bad)
		d.OnFailure(wf.Unavailable, stop)
		d.OnFailure(wf.Blocked, stop)
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 1, MaxSteps: 8}})
	if len(got.RealProblems) != 0 || got.Coverage.Ends["human gate"] == 0 || !strings.Contains(strings.Join(got.Advisories, "\n"), "no Uncertain route: uncertain effect at fuzzSpin ends in manual verification") {
		t.Fatalf("unresolved effect missing human boundary advisory: %s", got)
	}
}

func TestCheckUnreachedFaultIsFailure(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("unreached-fault")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		d.OnFailure(wf.Blocked, done)
		ordinary := a.To(done, "ordinary")
		alternate := a.To(done, "alternate").With(wf.ExternalEffect{Adapter: "test", Op: "write", Key: wf.KeyOf("write")})
		a.Spec(wf.PromptText("choose"), wf.Retry[fuzzAgent](wf.Const(1), ordinary))
		a.Route(func(v bool, _ wf.Ctx) wf.Edge[fuzzAgent] {
			if v {
				return ordinary
			}
			return alternate
		}, ordinary, alternate)
		return wftest.New(d)
	}
	got := wftest.Check(factory, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 8, MaxSteps: 8, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`), json.RawMessage(`false`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}})
	if len(got.UnreachedFaults) == 0 {
		t.Fatalf("expected fault not on progress path: %s", got)
	}
	if !strings.Contains(strings.Join(got.Problems, "\n"), "fault not reached") {
		t.Fatalf("unreached fault was advisory: %v", got.Problems)
	}
}
