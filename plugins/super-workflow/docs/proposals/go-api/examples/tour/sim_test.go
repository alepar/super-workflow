package tour_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tour"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func runTour(t *testing.T, world *wftest.World, steps ...wftest.Step) *wftest.Trace {
	t.Helper()
	return wftest.New(tour.Definition(), wftest.WithTB(t), wftest.WithWorld(world), wftest.WithParams(map[string]string{"goal": "Add arithmetic", "suite": "go test ./..."})).Run(steps...)
}

func tourPlan(tasks ...tour.Task) wftest.Step {
	return wftest.SubmitAt("Plan", tour.PlanOut{Tasks: tasks}).WithArtifacts(map[string]string{"plan.md": "Plan and acceptance criteria"})
}
func tourReview(verdict tour.Verdict) wftest.Step {
	return wftest.SubmitAt("Review", tour.ReviewOut{Rows: []tour.ReviewRow{{ID: "goal", Met: verdict == tour.OK, Evidence: "plan.md:1 goal coverage"}, {ID: "independent", Met: true, Evidence: "plan.md:2 independent tasks"}}})
}
func tourPatch() wftest.Step {
	return wftest.SubmitAt("Implement", tour.Patch{Summary: "Implemented and tested"})
}

func TestTourBuildsEveryTaskBeforeHumanDecision(t *testing.T) {
	for _, tc := range []struct{ answer, terminal string }{{"ship", "Shipped"}, {"hold", "Stopped"}} {
		t.Run(tc.answer, func(t *testing.T) {
			tr := runTour(t, wftest.NewWorld(),
				tourPlan(tour.Task{ID: "add", Title: "Addition"}, tour.Task{ID: "mul", Title: "Multiplication"}),
				tourReview(tour.OK), tourPatch(), tourPatch(), wftest.AnswerAt("Ship", tc.answer))
			if tr.Terminal() != tc.terminal {
				t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
			}
		})
	}
}

func TestTourRevisesPlanAndRepairsFailedSuite(t *testing.T) {
	world := wftest.NewWorld()
	wftest.ExitOnce(world, 1, "sh", "-c")
	tr := runTour(t, world,
		tourPlan(tour.Task{ID: "add", Title: "Addition"}), tourReview(tour.Revise),
		tourPlan(tour.Task{ID: "add", Title: "Revised addition"}), tourReview(tour.OK),
		tourPatch(), tourPatch(), wftest.AnswerAt("Ship", "ship"))
	if tr.Terminal() != "Shipped" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
	wftest.AssertRoute(t, tr, "Plan", "Review", "Plan", "Review", "Build", "tour-task/Implement", "tour-task/Test", "tour-task/Implement", "tour-task/Test", "tour-task/TaskDone", "Ship", "Shipped")
}

func TestTourStopsWhenTaskExhaustsRepairVisits(t *testing.T) {
	world := wftest.NewWorld()
	wftest.Exit(world, 1, "sh", "-c")
	tr := runTour(t, world, tourPlan(tour.Task{ID: "add", Title: "Addition"}), tourReview(tour.OK), tourPatch(), tourPatch(), tourPatch())
	if tr.Terminal() != "Stopped" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
}
