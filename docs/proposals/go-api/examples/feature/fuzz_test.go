package feature_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// The reference workflow driven end to end by seeded hostile agents. The
// known-good outputs are the scenario fixtures above, never written to pass a
// check; the driver still mixes in schema-only outputs and failures.
func TestFuzzFeature(t *testing.T) {
	// Odd seeds fail the first integration suite and goal check so repair
	// paths depend on command results rather than agent-selected commands.
	factoryFor := func(red bool) func() *wftest.Sim {
		return func() *wftest.Sim {
			w := world(t)
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
			if red {
				failGoalCheck(w, true)
				wftest.ExitOnce(w, 1, "sh", "-c", "go test ./...", "feature-suite")
			}
			return wftest.New(feature.Definition(), wftest.WithWorld(w), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": "Add addition", "suite": "go test ./..."}))
		}
	}
	factory := factoryFor(false)
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	valid := map[string][]json.RawMessage{
		"Design":                        {raw(plan())},
		"DesignReview":                  {raw(rows("g1", goal, true)), raw(rows("g1", goal, false))},
		"Implement":                     {raw(feature.Patch{Summary: goal, Head: strings.Repeat("a", 40)})},
		"Review":                        {raw(rows("c1", criterion, true)), raw(rows("c1", criterion, false))},
		"GoalCheck":                     {raw(feature.GoalTest{})},
		"Fix":                           {raw(feature.Patch{Summary: "repair", Head: strings.Repeat("a", 40)})},
		"ResolveBaseMerge":              {raw(feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)})},
		"feature-task/ResolveTaskMerge": {raw(feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)})},
	}

	coverage := wftest.NewFuzzCoverage(factory, nil)
	for seed := int64(0); seed < 64; seed++ {
		factory := factoryFor(seed%2 == 1)
		result := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 96, Valid: valid, Artifacts: map[string]map[string]string{"Design": {"spec.md": goal}, "GoalCheck": {"goal_test.go": blackBox}}})
		if result.Finding != nil {
			t.Fatal(result.Minimize(factory).String())
		}
		coverage.Add(result)
	}
	t.Log(coverage)
	for _, problem := range coverage.Problems() {
		t.Error(problem)
	}
}
