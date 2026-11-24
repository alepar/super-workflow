package wftest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type gatePathPrepare struct{}
type gatePathChild struct{}
type gatePathWork struct{}
type gatePathReview struct{}
type gatePathDone struct{}

func gatePreludeFactory() *Sim {
	child := wf.New("child")
	work := wf.NewCommand[gatePathWork](child, "check")
	review := wf.NewGate[gatePathReview, string](child, "recover?", wf.NewEnum(child, "answer", "continue"), wf.Reserved)
	done := wf.NewTerminal[gatePathDone](child, wf.Success)
	good, bad := work.To(done, "passed"), work.To(review, "failed")
	work.Route(func(result wf.RunResult, _ wf.Ctx) wf.Edge[gatePathWork] {
		if result.Exit != 0 {
			return bad
		}
		return good
	}, good, bad)
	review.Answers(wf.On("continue", review.To(done, "approved"))).Timeout(time.Minute, review.To(done, "expired"))
	wf.Yield(done, func(wf.Ctx) bool { return true })
	root := wf.New("gate-path")
	prepare := wf.NewCommand[gatePathPrepare](root, "prepare")
	sub := wf.NewSub[gatePathChild](root, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
	finished := wf.NewTerminal[gatePathDone](root, wf.Success)
	next := prepare.To(sub, "prepared")
	prepare.Route(func(wf.RunResult, wf.Ctx) wf.Edge[gatePathPrepare] { return next }, next)
	sub.Exits(wf.OnExit[gatePathDone](sub.To(finished, "finished")))
	return New(root)
}

func TestGatePreludeAnswersFailureOnlyChildGate(t *testing.T) {
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 12}}
	var results []*FuzzResult
	runs, answered := checkGatePreludePaths(gatePreludeFactory, cfg, "child/gatePathReview", "continue", []FuzzFault{
		{State: "child/gatePathReview", Class: "timeout"},
		{State: "child/gatePathWork", Class: "effect-adverse", Adapter: "shell", Op: "run"},
	}, func(result *FuzzResult) { results = append(results, result) })
	if runs != 1 || !answered || len(results) != 1 {
		t.Fatalf("runs=%d answered=%t results=%d", runs, answered, len(results))
	}
	result := results[0]
	if result.Finding != nil || !result.PreludeFaultInjected || result.Status.Outcome != wf.Success {
		t.Fatal(result)
	}
	var exits []int
	for _, record := range result.Trace.Journal() {
		if record.Kind != wf.JournalEffectResult {
			continue
		}
		var effect wf.EffectResultData
		if err := json.Unmarshal(record.Data, &effect); err != nil {
			t.Fatal(err)
		}
		var command wf.RunResult
		if err := json.Unmarshal(effect.Result, &command); err != nil {
			t.Fatal(err)
		}
		exits = append(exits, command.Exit)
	}
	if len(exits) != 2 || exits[0] != 0 || exits[1] != 1 {
		t.Fatalf("parent inherited child fault: exits=%v", exits)
	}
	if len(result.Script) != 1 || result.Script[0].Kind != FuzzAnswer || string(result.Script[0].Output) != `"continue"` {
		t.Fatalf("gate was reached but not answered: %v", result.Script)
	}
}

func TestGatePreludeDoesNotClaimAnotherGateAnswer(t *testing.T) {
	var results []*FuzzResult
	runs, answered := checkGatePreludePaths(gatePreludeFactory, CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 12}}, "elsewhere/gatePathReview", "continue", []FuzzFault{
		{State: "child/gatePathWork", Class: "effect-adverse", Adapter: "shell", Op: "run"},
	}, func(result *FuzzResult) { results = append(results, result) })
	if runs != 1 || answered || len(results) != 1 || len(results[0].Script) != 1 || results[0].Script[0].State != "child/gatePathReview" {
		t.Fatalf("runs=%d answered=%t results=%v", runs, answered, results)
	}
}
