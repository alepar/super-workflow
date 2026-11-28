// Package checker supplies development-only fixtures for the five-minute tour.
package checker

import (
	"encoding/json"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// Scenario returns a fresh simulated world; no suite runs in the caller's checkout.
func Scenario() (func() *wftest.Sim, wftest.CalibrationConfig) {
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
	valid := map[string][]json.RawMessage{
		"Plan":                {raw(tour.PlanOut{Tasks: []tour.Task{{ID: "add", Title: "Add addition"}, {ID: "mul", Title: "Add multiplication"}}})},
		"Review":              {raw(review(true)), raw(review(false))},
		"tour-task/Implement": {raw(tour.Patch{Summary: "Implemented and tested"})},
	}
	valid["Plan"] = append(valid["Plan"], raw(tour.PlanOut{Tasks: []tour.Task{{ID: "dup", Title: "First"}, {ID: "dup", Title: "Second"}}}))
	progress := map[string][]json.RawMessage{
		"Plan": {valid["Plan"][0]}, "Review": {valid["Review"][0]}, "tour-task/Implement": valid["tour-task/Implement"],
	}
	factory := func() *wftest.Sim {
		world := wftest.NewWorld()
		wftest.Exit(world, 0, "sh", "-c", "go test ./...")
		return wftest.New(tour.Definition(), wftest.WithWorld(world), wftest.WithParams(map[string]string{"goal": "Add arithmetic", "suite": "go test ./..."}))
	}
	return factory, wftest.CalibrationConfig{Valid: valid, Progress: progress, Artifacts: map[string]map[string]string{"Plan": {"plan.md": "Plan and acceptance criteria"}}, Seeds: 64, MaxSteps: 96}
}

// Registration keeps simulator dependencies out of the shipped workflow runner.
func Registration() wf.Registration {
	factory, cfg := Scenario()
	return wf.Registration{Def: tour.Definition(), Check: wftest.CheckCommand(factory, wftest.CheckConfig{CalibrationConfig: cfg, ChildProtocol: true})}
}

func review(met bool) tour.ReviewOut {
	return tour.ReviewOut{Rows: []tour.ReviewRow{
		{ID: "goal", Met: met, Evidence: "plan.md:1 goal coverage"},
		{ID: "independent", Met: true, Evidence: "plan.md:2 independent tasks"},
	}}
}
