package tour_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour/checker"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"testing"
)

func TestReservedGateDelivery(t *testing.T) {
	factory, _ := checker.Scenario()
	r := wftest.CheckReservedGateDelivery(factory, "Ship", "hold", tourPlan(tour.Task{ID: "one", Title: "One task"}), tourReview(tour.OK), wftest.SubmitAt("tour-task/Implement", tour.Patch{Summary: "done"}))
	t.Log(r)
	if r.PositiveControls != 8 || len(r.FixtureGaps) > 0 || len(r.Problems) > 0 {
		t.Fatal(r)
	}
}
