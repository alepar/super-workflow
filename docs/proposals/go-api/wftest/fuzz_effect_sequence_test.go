package wftest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func sequenceRun(samples ...json.RawMessage) (*fuzzRun, func()) {
	f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{}, schemas: map[string]wf.StateView{"child/Work": {Kind: "command"}, "other/Work": {Kind: "command"}}}
	f.setupPhases(FuzzConfig{})
	finish := f.setupEffectSequences(FuzzConfig{EffectResults: []FuzzEffectSequence{{State: "child/Work", Adapter: "shell", Op: "run", Results: samples}}})
	return f, finish
}

func TestEffectSequenceStateOrderFallbackAndGoodPhase(t *testing.T) {
	f, finish := sequenceRun(json.RawMessage(`{"Exit":2}`), json.RawMessage(`{"Exit":3}`))
	w := f.sim.world
	calls := 0
	OnApply(w, func(wf.ExternalEffect) { calls++ })
	effect := wf.ExternalEffect{Adapter: "shell", Op: "run"}
	var exits []int
	for _, state := range []string{"other/Work", "child/Work", "child/Work", "child/Work"} {
		raw, err := w.ApplyAt("", wf.EffectContext{State: state}, effect)
		if err != nil {
			t.Fatal(err)
		}
		var result wf.RunResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		exits = append(exits, result.Exit)
	}
	if !reflect.DeepEqual(exits, []int{0, 2, 3, 0}) || calls != 4 || len(w.applied) != 4 {
		t.Fatalf("exits=%v calls=%d applied=%d", exits, calls, len(w.applied))
	}
	finish()
	if f.result.Finding != nil {
		t.Fatal(f.result.Finding)
	}
	f, finish = sequenceRun(json.RawMessage(`{"Exit":2}`))
	f.sim.world.setFuzzGood(true)
	raw, _ := f.sim.world.ApplyAt("", wf.EffectContext{State: "child/Work"}, effect)
	if effectOutcome(raw, nil) != "good" {
		t.Fatalf("good phase: %s", raw)
	}
	finish()
	if f.result.Finding == nil || f.result.Finding.Rule != "fixture-gap" {
		t.Fatal("ignored sample counted as consumed")
	}
}

func TestEffectSequenceFaultPrecedence(t *testing.T) {
	f, finish := sequenceRun(json.RawMessage(`{"Exit":3}`))
	w := f.sim.world
	w.fuzzFault = func(wf.EffectContext, wf.ExternalEffect) string { return "effect-adverse" }
	effect := wf.ExternalEffect{Adapter: "shell", Op: "run"}
	_, _ = w.ApplyAt("", wf.EffectContext{State: "child/Work"}, effect)
	w.fuzzFault = nil
	raw, _ := w.ApplyAt("", wf.EffectContext{State: "child/Work"}, effect)
	var result wf.RunResult
	_ = json.Unmarshal(raw, &result)
	if result.Exit != 3 {
		t.Fatalf("fault consumed sample: %s", raw)
	}
	finish()
	if f.result.Finding != nil {
		t.Fatal(f.result.Finding)
	}
}

func TestEffectSequenceValidation(t *testing.T) {
	valid := FuzzEffectSequence{State: "child/Work", Adapter: "shell", Op: "run", Results: []json.RawMessage{json.RawMessage(`{}`)}}
	for _, mode := range []string{"json", "empty", "state", "op", "duplicate", "unused"} {
		t.Run(mode, func(t *testing.T) {
			seq := valid
			switch mode {
			case "json":
				seq.Results = []json.RawMessage{json.RawMessage(`{`)}
			case "empty":
				seq.Results = nil
			case "state":
				seq.State = "missing"
			case "op":
				seq.Op = "missing"
			}
			config := FuzzConfig{EffectResults: []FuzzEffectSequence{seq}}
			if mode == "duplicate" {
				config.EffectResults = append(config.EffectResults, seq)
			}
			f := &fuzzRun{sim: New(wf.New("root")), result: &FuzzResult{}, schemas: map[string]wf.StateView{"child/Work": {Kind: "command"}}}
			finish := f.setupEffectSequences(config)
			finish()
			if f.result.Finding == nil || f.result.Finding.Rule != "fixture-gap" || !strings.Contains(f.result.Finding.Why, seq.State) {
				t.Fatalf("missing target-specific gap: %+v", f.result.Finding)
			}
		})
	}
}

type sequenceCommand struct{}
type sequenceDone struct{}
type sequenceAgent struct{}

func TestEffectSequenceReplayAndUnusedSampleGap(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("sequence")
		c := wf.NewCommand[sequenceCommand](d, "check")
		done := wf.NewTerminal[sequenceDone](d, wf.Success)
		again, end := c.To(c, "again"), c.To(done, "done")
		c.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[sequenceCommand] {
			if result.Exit != 0 {
				return again
			}
			return end
		}, again, end)
		return New(d)
	}
	cfg := FuzzConfig{MaxSteps: 10, EffectResults: []FuzzEffectSequence{{State: "sequenceCommand", Adapter: "shell", Op: "run", Results: []json.RawMessage{json.RawMessage(`{"Exit":2}`), json.RawMessage(`{"Exit":3}`)}}}}
	result := Fuzz(factory, cfg)
	if result.Finding != nil || result.Status.Outcome != wf.Success {
		t.Fatalf("sequence run: %s", result)
	}
	replay := result.Replay(factory)
	if replay.Finding != nil || replay.Status.Outcome != wf.Success {
		t.Fatalf("sequence replay: %s", replay)
	}
	commandExits := func(r *FuzzResult) []int {
		var exits []int
		for _, record := range r.Trace.Journal() {
			if record.Kind == wf.JournalEffectResult {
				var data wf.EffectResultData
				if err := json.Unmarshal(record.Data, &data); err != nil {
					t.Fatal(err)
				}
				var outcome wf.RunResult
				if err := json.Unmarshal(data.Result, &outcome); err != nil {
					t.Fatal(err)
				}
				exits = append(exits, outcome.Exit)
			}
		}
		return exits
	}
	if got := commandExits(result); !reflect.DeepEqual(got, []int{2, 3, 0}) {
		t.Fatalf("result order: %v", got)
	}
	if got := commandExits(replay); !reflect.DeepEqual(got, []int{2, 3, 0}) {
		t.Fatalf("replay order: %v", got)
	}
	cfg.EffectResults[0].Results = []json.RawMessage{json.RawMessage(`{"Exit":0}`), json.RawMessage(`{"Exit":3}`)}
	unused := Fuzz(factory, cfg)
	if unused.Finding == nil || unused.Finding.Rule != "fixture-gap" || !strings.Contains(unused.Finding.Why, "1 unconsumed") {
		t.Fatalf("unused result accepted: %s", unused)
	}
}

func TestEffectSequenceHealthyStoppedStillFailsLiveness(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("healthy-stop")
		c := wf.NewCommand[sequenceCommand](d, "check")
		stop := wf.NewTerminal[sequenceDone](d, wf.Stopped)
		a := wf.NewAgent[declaredAgent, bool](d)
		c.Then(c.To(a, "decide"))
		a.Spec(wf.PromptText("decide"), wf.Retry[declaredAgent](wf.Const(1), a.To(stop, "no answer")))
		b := wf.NewAgent[sequenceAgent, bool](d)
		a.Then(a.To(b, "continue"))
		b.Spec(wf.PromptText("decide"), wf.Retry[sequenceAgent](wf.Const(1), b.To(stop, "no answer")))
		b.Then(b.To(stop, "stop").Ends("intentional stop"))
		return New(d)
	}
	result := Fuzz(factory, FuzzConfig{MaxSteps: 5, Liveness: true, LivenessAfter: 1, Progress: map[string][]json.RawMessage{"declaredAgent": {json.RawMessage(`true`)}, "sequenceAgent": {json.RawMessage(`true`)}}, EffectResults: []FuzzEffectSequence{{State: "sequenceCommand", Adapter: "shell", Op: "run", Results: []json.RawMessage{json.RawMessage(`{"Exit":0}`)}}}})
	if result.adversarialInjected || result.Finding == nil || result.Finding.Rule != "liveness" {
		t.Fatalf("healthy data excused stop: %s", result)
	}
}

func TestEffectSequenceDetachedPollDoesNotConsume(t *testing.T) {
	f, finish := sequenceRun(json.RawMessage(`{"Exit":2}`), json.RawMessage(`{"Exit":3}`))
	runner := newFuzzEffectRunner(NewFakeRunner(), f.sim.world)
	req := wf.DetachedRequest{Context: wf.EffectContext{State: "child/Work"}, ResultPath: "one"}
	p, err := runner.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		report, err := runner.Status(p, req)
		if err != nil || report.Result.Exit != 2 {
			t.Fatalf("poll: %+v %v", report, err)
		}
	}
	raw, err := f.sim.world.ApplyAt("", req.Context, wf.ExternalEffect{Adapter: "shell", Op: "run"})
	var outcome wf.RunResult
	_ = json.Unmarshal(raw, &outcome)
	if err != nil || outcome.Exit != 3 {
		t.Fatalf("shared inline sequence: %s %v", raw, err)
	}
	finish()
	if f.result.Finding != nil {
		t.Fatal(f.result.Finding)
	}
}
