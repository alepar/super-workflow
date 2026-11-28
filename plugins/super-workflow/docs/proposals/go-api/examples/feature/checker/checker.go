// Package checker holds development-only simulator fixtures for the reference workflow.
package checker

import (
	"encoding/json"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// Scenario uses the addition scenario from the reference's end-to-end
// tests. Its world is simulated; no commands run against the caller's checkout.
func Scenario() (func() *wftest.Sim, wftest.CalibrationConfig) {
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
	valid := map[string][]json.RawMessage{
		"Design":                        {raw(checkerPlan())},
		"DesignReview":                  {raw(checkerRows("g1", true)), raw(checkerRows("g1", false))},
		"feature-task/Implement":        {raw(feature.Patch{Summary: checkerGoal, Head: strings.Repeat("a", 40)})},
		"feature-task/Review":           {raw(checkerRows("c1", true)), raw(checkerRows("c1", false))},
		"GoalCheck":                     {raw(feature.GoalTest{})},
		"Fix":                           {raw(feature.Patch{Summary: "repair", Head: strings.Repeat("a", 40)})},
		"ResolveBaseMerge":              {raw(feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)})},
		"feature-task/ResolveTaskMerge": {raw(feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)})},
	}
	// Negative reviews are valid submissions, but do not advance an all-good
	// recovery. Designate positive reviews separately from the broader corpus.
	progress := map[string][]json.RawMessage{
		"Design":                        valid["Design"],
		"DesignReview":                  {raw(checkerRows("g1", true))},
		"feature-task/Implement":        valid["feature-task/Implement"],
		"feature-task/Review":           {raw(checkerRows("c1", true))},
		"GoalCheck":                     valid["GoalCheck"],
		"Fix":                           valid["Fix"],
		"ResolveBaseMerge":              valid["ResolveBaseMerge"],
		"feature-task/ResolveTaskMerge": valid["feature-task/ResolveTaskMerge"],
	}
	factory := func() *wftest.Sim {
		w := checkerWorld()
		failed := map[bool]bool{}
		wftest.FailApplyIf(w, func(e wf.ExternalEffect) string {
			if e.Adapter == "git" && e.Op == "merge" {
				base := e.Args["into"] != nil
				if !failed[base] {
					failed[base] = true
					return "fixture merge conflict; aborted"
				}
			}
			return ""
		})
		wftest.ExitOnce(w, 1, "sh", "-c")
		wftest.Exit(w, 0, "sh", "-c", "go test ./...")
		wftest.Exit(w, 0, "sh", "-c", "files=$(gofmt -l .) && test -z \"$files\" && go vet ./...")
		return wftest.New(feature.Definition(), wftest.WithWorld(w), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": checkerGoal, "suite": "go test ./..."}))
	}

	return factory, wftest.CalibrationConfig{Valid: valid, Progress: progress, Seeds: 64, MaxSteps: 96, Artifacts: map[string]map[string]string{"Design": {"spec.md": checkerGoal}, "GoalCheck": {"goal_test.go": checkerBlackBox}}}
}

const checkerGoal = "Add addition"
const checkerCriterion = "2+3 is 5"
const checkerBlackBox = `package acceptance_test
import ("testing";calc "fixture")
func TestAPI(t *testing.T) {if calc.Add(2,3)!=5 {t.Fatal("sum")}}
`
const checkerGoalEvents = "{\"Action\":\"run\",\"Test\":\"TestAPI\"}\n{\"Action\":\"pass\",\"Test\":\"TestAPI\"}\n"

func checkerPlan() feature.Plan {
	return feature.Plan{Tasks: []feature.Task{{ID: "add", Title: checkerGoal, Files: []string{"calc.go"}, Acceptance: []string{checkerCriterion}}}}
}
func checkerRows(id string, met bool) feature.ReviewResult {
	return feature.ReviewResult{Rows: []feature.ReviewRow{{ID: id, Met: met, Evidence: "TestAPI: calc.go:3 and go test: passed", Tasks: []string{"add"}}}}
}
func checkerWorld() *wftest.World {
	w := wftest.NewWorld()
	wftest.Stdout(w, checkerGoalEvents, "sh", "-c")
	wftest.OnRead(w, func(d wf.ObservationDesc) (json.RawMessage, bool) {
		var v any
		switch d.Adapter + "." + d.Op {
		case "git.head":
			v = strings.Repeat("a", 40)
		case "git.status":
			v = map[string]any{"Clean": true}
		case "shell.run":
			b, _ := json.Marshal(d.Args)
			if strings.Contains(string(b), "ls-files") {
				v = wf.RunResult{}
			} else if strings.Contains(string(b), "list") {
				v = wf.RunResult{Stdout: "fixture\n"}
			} else if strings.Contains(string(b), "printf") {
				v = wf.RunResult{Stdout: "run\n"}
			} else {
				v = wf.RunResult{Stdout: "main\n"}
			}
		default:
			return nil, false
		}
		b, _ := json.Marshal(v)
		return b, true
	})
	return w
}

// Suite adds structured command results and coordinated approval/repair paths
// to the ordinary calibration and generated fault coverage.
func Suite() (func() *wftest.Sim, wftest.CheckConfig) {
	factory, cfg := Scenario()
	raw := func(v wf.RunResult) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
	failed := raw(wf.RunResult{Exit: 1, Stdout: "{\"Action\":\"build-fail\"}\n"})
	passed := raw(wf.RunResult{Stdout: checkerGoalEvents})
	results := func(values ...json.RawMessage) []wftest.FuzzEffectSequence {
		return []wftest.FuzzEffectSequence{{State: "RunGoalCheck", Adapter: "shell", Op: "run", Results: values}}
	}
	return factory, wftest.CheckConfig{ChildProtocol: true, CalibrationConfig: cfg, Scenarios: map[string]wftest.FuzzConfig{
		// Two failed authoring attempts per product version spend both repairs.
		// A final author correction passes; the ordinary base merge conflict then
		// requires another verification epoch, exceeding the authoring visit cap.
		"build failures and authoring cap": {EffectResults: results(failed, failed, failed, failed, failed, passed), GateAnswers: map[string]string{"Approve": "merge"}},
		"base merge resolution cap":        {EffectResults: results(passed, passed, passed), GateAnswers: map[string]string{"Approve": "merge", "Recovery": "keep"}, PreludeFault: &wftest.FuzzFault{State: "BaseMerge", Adapter: "git", Op: "merge", Class: "effect-error"}, RepeatPrelude: true},
	}}
}

// Registration opts the development-only checker binary into simulation.
func Registration() wf.Registration {
	registration := feature.Registration()
	factory, cfg := Suite()
	registration.Check = wftest.CheckCommand(factory, cfg)
	return registration
}
