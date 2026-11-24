package feature_test

import (
	"fmt"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// Calibrate the reference checks in the same scenario as the end-to-end tests.
// Observation overrides in declared examples remain visible in the report.
func TestFeatureCheckCalibration(t *testing.T) {
	factory, cfg := checker.Scenario()
	result := wftest.Calibrate(factory, cfg)
	t.Log(result)
	if result.States != 8 {
		t.Errorf("calibrated %d states, want all eight", result.States)
	}
	for _, problem := range result.Problems {
		t.Error(problem)
	}
}

func TestFeatureCheckerCoversStructuredFailuresAndCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("long scenario (about two CPU-minutes); the pin gate runs it without -short")
	}
	factory, cfg := checker.Suite()
	cfg.ChildProtocol, cfg.Seeds = false, 1
	report := wftest.Check(factory, cfg)
	for _, finding := range report.Findings {
		t.Error(finding)
	}
	want := map[string]bool{
		"goal test did not build: correct the test":                               false,
		"goal test still does not build: public API may be missing or unexported": false,
		"goal-test authoring cap":                                                 false,
		"base conflict resolution cap":                                            false,
	}
	for _, state := range wf.Inspect(feature.Definition()).States {
		for i, edge := range state.Edges {
			if _, ok := want[edge.Label]; ok && report.Coverage != nil {
				want[edge.Label] = report.Coverage.EdgesReached[fmt.Sprintf("%s/edge[%d]", state.Path, i)] > 0
			}
		}
	}
	for label, reached := range want {
		if !reached {
			t.Errorf("missing %s: %s", label, report)
		}
	}
}
