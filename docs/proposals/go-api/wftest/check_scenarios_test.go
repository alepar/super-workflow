package wftest

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCheckScenariosConsumeStructuredResultsAndInheritProgress(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("scenario")
		start := wf.NewAgent[corpusStart, bool](d)
		work := wf.NewCommand[repeatedWork](d, "build")
		done := wf.NewTerminal[repeatedDone](d, wf.Success)
		stop := wf.NewTerminal[repeatedStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		start.Spec(wf.PromptText("start"), wf.Retry[corpusStart](wf.Const(1), start.To(stop, "invalid")))
		start.Then(start.To(work, "build"))
		good, bad := work.To(done, "built"), work.To(stop, "structured failure").Ends("build failure")
		work.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[repeatedWork] {
			if r.Stdout == `{"failed":true}` {
				return bad
			}
			return good
		}, good, bad)
		return New(d)
	}
	value, _ := json.Marshal(wf.RunResult{Exit: 1, Stdout: `{"failed":true}`})
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 8, Progress: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}}},
		Scenarios: map[string]FuzzConfig{"structured": {EffectResults: []FuzzEffectSequence{{State: "repeatedWork", Adapter: "shell", Op: "run", Results: []json.RawMessage{value}}}}},
	}
	coverage := NewFuzzCoverage(factory, nil)
	var results []*FuzzResult
	n := checkScenarios(factory, cfg, func(r *FuzzResult) { results = append(results, r); coverage.Add(r) })
	if n != 1 || results[0].Finding != nil || coverage.EdgesReached["scenario/repeatedWork/edge[1]"] == 0 {
		t.Fatalf("scenario missing: n=%d results=%v edges=%v", n, results, coverage.EdgesReached)
	}
	if !reflect.DeepEqual(results[0].config.Progress, cfg.Progress) || cfg.Scenarios["structured"].MaxSteps != 0 {
		t.Fatal("defaults not inherited or caller configuration mutated")
	}
	if replay := results[0].Replay(factory); replay.Finding != nil || replay.Status.Terminal != "repeatedStop" {
		t.Fatal(replay)
	}
}

func TestCheckScenariosUseStableNameOrder(t *testing.T) {
	factory := func() *Sim { d := wf.New("done"); wf.NewTerminal[repeatedDone](d, wf.Success); return New(d) }
	var seeds []int64
	checkScenarios(factory, CheckConfig{Scenarios: map[string]FuzzConfig{"z": {Seed: 2}, "a": {Seed: 1}}}, func(r *FuzzResult) { seeds = append(seeds, r.Seed) })
	if !reflect.DeepEqual(seeds, []int64{1, 2}) {
		t.Fatal(seeds)
	}
}

func TestCheckScenariosRequireCuratedAgentOutputs(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("missing-corpus")
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedDone](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.RetryPolicy[corpusStart]{})
		a.Then(a.To(end, "done"))
		return New(d)
	}
	checkScenarios(factory, CheckConfig{Scenarios: map[string]FuzzConfig{"missing": {}}}, func(r *FuzzResult) {
		if r.Finding == nil || r.Finding.Rule != "fixture-gap" {
			t.Fatalf("scenario invented output instead of requiring corpus: %s", r)
		}
	})
}
