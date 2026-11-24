package approval_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"testing"
)

func TestReservedGateDelivery(t *testing.T) {
	factory := func() *wftest.Sim {
		return wftest.New(approval.Definition(), wftest.WithParams(map[string]string{"worktree": t.TempDir()}))
	}
	r := wftest.CheckReservedGateDelivery(factory, "Approve", "decline", wftest.SubmitAt("Draft", approval.PlanOut{State: approval.PlanReady, Path: "plan.md"}))
	t.Log(r)
	if r.PositiveControls != 8 || len(r.FixtureGaps) > 0 || len(r.Problems) > 0 {
		t.Fatal(r)
	}
}
