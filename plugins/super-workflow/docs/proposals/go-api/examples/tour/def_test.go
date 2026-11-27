package tour

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"slices"
	"testing"
)

func TestShipPresentsPlanArtifact(t *testing.T) {
	for _, state := range wf.Inspect(Definition()).States {
		if state.Name == "Ship" && slices.Contains(state.Presents, "plan.md") {
			return
		}
	}
	t.Fatal("Ship must present the allocated plan artifact")
}

func TestReviewRequiresEvidenceAndDerivesVerdict(t *testing.T) {
	valid := ReviewOut{Rows: []ReviewRow{{ID: "goal", Met: true, Evidence: "plan.md:1 covers the goal"}, {ID: "independent", Met: true, Evidence: "plan.md:2 disjoint tasks"}}}
	if err := checkReview(valid, nil); err != nil || reviewVerdict(valid) != OK {
		t.Fatalf("valid review: %v", err)
	}
	valid.Rows[1].Met = false
	if err := checkReview(valid, nil); err != nil || reviewVerdict(valid) != Revise {
		t.Fatalf("negative evidence must route to revision: %v", err)
	}
	for _, bad := range []ReviewOut{
		{},
		{Rows: []ReviewRow{{ID: "goal", Met: true, Evidence: "plan.md:1"}}},
		{Rows: []ReviewRow{{ID: "goal", Evidence: "plan.md:1"}, {ID: "goal", Evidence: "plan.md:2"}}},
		{Rows: []ReviewRow{{ID: "goal", Evidence: "plan.md:1"}, {ID: "unknown", Evidence: "plan.md:2"}}},
		{Rows: []ReviewRow{{ID: "goal", Evidence: " "}, {ID: "independent", Evidence: "plan.md:2"}}},
	} {
		if err := checkReview(bad, nil); err == nil {
			t.Fatalf("accepted incomplete review: %+v", bad)
		}
	}
}

func TestLints(t *testing.T) {
	d := Definition()
	for name, ps := range map[string]any{"lint": d.Lint(), "sound": d.LintSound(), "checks": d.LintChecks(), "reach": d.LintReachability(), "exec": d.LintExec()} {
		t.Logf("%s: %v", name, ps)
	}
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
}
