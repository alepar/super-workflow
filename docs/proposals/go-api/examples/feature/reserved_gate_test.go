package feature_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"strings"
	"testing"
)

func TestReservedGateDelivery(t *testing.T) {
	for _, gate := range []string{"Approve", "Recovery", "feature-task/Recovery"} {
		t.Run(gate, func(t *testing.T) {
			factory := func() *wftest.Sim {
				w := world(t)
				if gate == "feature-task/Recovery" {
					wftest.FailApply(w, "git", "merge", "merge conflict; aborted")
				}
				return wftest.New(feature.Definition(), wftest.WithWorld(w), wftest.WithParams(map[string]string{"goal-check-command": "go test -count=1 -json", "goal": goal, "suite": "go test ./..."}))
			}
			steps := happy()
			if gate == "feature-task/Recovery" {
				steps = happy()[:4]
				for i := 0; i < 2; i++ {
					steps = append(steps, wftest.SubmitAt("ResolveTaskMerge", feature.Patch{Summary: "resolved", Head: strings.Repeat("a", 40)}), review(true))
				}
			}
			if gate == "Recovery" {
				steps = []wftest.Step{wftest.Fail[feature.Design, feature.Plan](nil, wf.FailBlocked, "needs a person")}
			}
			r := wftest.CheckReservedGateDelivery(factory, gate, "keep", steps...)
			t.Log(r)
			if r.PositiveControls != 8 || len(r.FixtureGaps) > 0 || len(r.Problems) > 0 {
				t.Fatal(r)
			}
		})
	}
}
