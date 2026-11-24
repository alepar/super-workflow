package wftest_test

import (
	"encoding/json"
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"reflect"
	"strings"
	"testing"
)

func TestLivenessReportsRefusedProgressPremise(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("live")
		a := wf.NewAgent[fuzzAgent, bool](d)
		end := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "refusal").Ends("no repair")), wf.Checkf("broken", func(bool, wf.Ctx) error { return errors.New("false refusal") }))
		a.Then(a.To(end, "done").Ends("intentional stop"))
		return wftest.New(d)
	}
	result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 0, MaxSteps: 8, Liveness: true, LivenessAfter: 0, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
	if result.Finding == nil || result.Finding.Rule != "progress-context" {
		t.Fatalf("refused progress hid invalid all-good premise: %s", result)
	}
	again := result.Replay(factory)
	if again.Finding == nil || again.Finding.Rule != "progress-context" {
		t.Fatalf("progress-context replay lost failure: %s", again)
	}
}

func TestLivenessNeedsKnownGoodFixture(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{Liveness: true, LivenessAfter: 0, MaxSteps: 8})
	if got.Finding == nil || got.Finding.Rule != "no-progress-example" {
		t.Fatalf("missing good fixture should not fake progress: %s", got)
	}
}

func TestOutcomeSwitchesAreSeededAndPrinted(t *testing.T) {
	cfg := wftest.FuzzConfig{Seed: 42, MaxSteps: 8, OutcomeSwitches: true}
	a := wftest.Fuzz(fuzzSimpleFactory(false), cfg)
	b := wftest.Fuzz(fuzzSimpleFactory(false), cfg)
	if len(a.Switches) == 0 || !reflect.DeepEqual(a.Switches, b.Switches) || !reflect.DeepEqual(a.Script, b.Script) || !strings.Contains(a.String(), "switches:") {
		t.Fatalf("switches missing or non-deterministic: %s\n%s", a, b)
	}
}

func TestLivenessCheckpointComesFromLivePrefix(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("early-end")
		a := wf.NewAgent[fuzzAgent, bool](d)
		done := wf.NewTerminal[fuzzDone](d, wf.Success)
		stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(stop, "exhausted")))
		a.Then(a.To(done, "accepted"))
		return wftest.New(d)
	}
	for seed := int64(0); seed < 16; seed++ {
		got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 64, Liveness: true, LivenessAfter: -1, OutcomeSwitches: true, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
		if got.Finding != nil {
			t.Fatalf("recovery should precede terminal hostile prefix: %s", got)
		}
		if got.Status.Outcome != wf.Success {
			t.Fatalf("all-good recovery failed: %s", got)
		}
	}
}

func TestLivenessUsesProgressRatherThanValidNegativeReview(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("progress")
		a := wf.NewAgent[fuzzAgent, bool](d)
		yes := wf.NewTerminal[fuzzDone](d, wf.Success)
		no := wf.NewTerminal[fuzzStop](d, wf.Stopped)
		pass, fail := a.To(yes, "pass"), a.To(no, "negative review").Ends("valid negative review")
		a.Spec(wf.PromptText("review"), wf.Retry[fuzzAgent](wf.Const(1), fail))
		a.Route(func(out bool, _ wf.Ctx) wf.Edge[fuzzAgent] {
			if out {
				return pass
			}
			return fail
		}, pass, fail)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{Liveness: true, MaxSteps: 8, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`false`)}}, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
	if got.Finding != nil || got.Status.Outcome != wf.Success {
		t.Fatalf("accepted negative replaced progress: %s", got)
	}
}

func TestLivenessAcceptsSuccessOnLastAllowedStep(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{Liveness: true, LivenessAfter: 0, LivenessSteps: 1, Progress: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
	if got.Finding != nil || got.Status.Outcome != wf.Success {
		t.Fatalf("success at exact bound was rejected: %s", got)
	}
}
