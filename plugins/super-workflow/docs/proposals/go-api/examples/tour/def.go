// Package tour is the five-minute tour: plan, review, build in parallel,
// ask a person. Every line of behaviour is a declared state or edge.
package tour

import (
	"errors"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// 1. States are types. A typo is a compile error, not a run-time surprise.
type (
	Plan    struct{} // agent: break the goal into tasks
	Review  struct{} // agent, fresh context: is the plan good?
	Build   struct{} // map: one child workflow per task, in parallel
	Ship    struct{} // gate: a person decides
	Shipped struct{}
	Stopped struct{}

	Implement  struct{} // child workflow: write the code
	Test       struct{} // child workflow: run the suite
	TaskDone   struct{}
	TaskGaveUp struct{}
)

// 2. Agent results are typed JSON. The engine decodes and validates them.
type Task struct{ ID, Title string }
type PlanOut struct{ Tasks []Task }

// ReviewRow names a declared criterion and cites evidence in plan.md.
type ReviewRow struct {
	ID       string
	Met      bool
	Evidence string
}
type ReviewOut struct{ Rows []ReviewRow }
type TaskResult struct{ Summary string }
type Patch struct{ Summary string }

type Verdict string // routing values derived from validated review evidence
const (
	OK     Verdict = "ok"
	Revise Verdict = "revise"
)

type Answer string

const (
	Go   Answer = "ship"
	Hold Answer = "hold"
)

func Definition() *wf.Def {
	d := wf.New("tour")
	goal := d.Param("goal") // run parameters, given on the command line
	d.Param("suite")
	verdicts := wf.NewEnum(d, "verdict", OK, Revise)
	answers := wf.NewEnum(d, "answer", Go, Hold)
	revisions := d.Counter("revisions")

	plan := wf.NewAgent[Plan, PlanOut](d)
	planDoc := wf.NewArtifact[string](plan, "plan.md")
	review := wf.NewAgent[Review, ReviewOut](d)
	// Map type arguments: state, item, comparable key, child result.
	build := wf.NewMap[Build, Task, string, TaskResult](d, // items come from Plan's output
		wf.FromOutput(plan, func(p PlanOut) []Task { return p.Tasks }),
		func(t Task) string { return t.ID }, taskWorkflow())
	ship := wf.NewGate[Ship, Answer](d, "All tasks built and tested. Ship it?", answers, wf.Reserved)
	shipped := wf.NewTerminal[Shipped](d, wf.Success)
	stopped := wf.NewTerminal[Stopped](d, wf.Stopped)

	// 3. One default for operational failures, instead of an edge on every state.
	d.OnFailures(stopped, wf.Blocked, wf.Unavailable, wf.Uncertain)

	// 4. An agent state: a prompt with typed inputs, a retry budget, and a check
	//    that carries its own examples (the checker runs them).
	plan.Spec(
		wf.PromptText("Split this goal into small independent tasks: {{goal}}. Previous review evidence: {{notes}}. Write the plan and acceptance criteria to the allocated plan.md artifact; do not return its path.",
			goal, wf.As("notes", wf.Out(review))),
		wf.Attempts(2, plan.To(stopped, "no usable plan")).OnClass(wf.Unavailable,
			plan.To(stopped, "default unavailable route").Unreachable("inline prompt; the task-count check returns only ordinary refusals")),
		wf.Checkf("at least one task", func(p PlanOut, _ wf.Ctx) error {
			if len(p.Tasks) == 0 {
				return errors.New("a plan needs at least one task")
			}
			return nil
		}).Accepts(PlanOut{Tasks: []Task{{ID: "add", Title: "Add()"}}}).
			Refuses(PlanOut{}, "a plan needs at least one task"))
	plan.ThenTo(review, "planned")

	// 5. Routing on an enum: every value must have an edge, or lint fails.
	review.Spec(wf.PromptText("Review this plan for the goal {{goal}}: {{plan}}. Read {{document}}. Return one evidence row for each criterion: goal (tasks cover the goal), independent (tasks can be implemented independently). Each row has id, met, and evidence citing plan.md; explain missing coverage in false rows. Do not choose a verdict.", goal, wf.As("plan", wf.Out(plan)), wf.As("document", planDoc)),
		wf.Attempts(2, review.To(build, "reviewer unusable: build the plan as it is")).OnClass(wf.Unavailable,
			review.To(stopped, "default unavailable route").Unreachable("inline prompt and evidence check have no adapter observations or external effects; artifact input errors are blocked")),
		wf.Checkf("complete review evidence", checkReview).
			Accepts(ReviewOut{Rows: []ReviewRow{{ID: "goal", Met: true, Evidence: "plan.md:1 covers goal"}, {ID: "independent", Met: true, Evidence: "plan.md:2 disjoint tasks"}}}).
			Refuses(ReviewOut{}, "review needs goal and independent evidence rows"))
	wf.Consumes(review, planDoc)
	review.RouteCases(wf.Cases(reviewVerdict, verdicts,
		wf.On(OK, review.To(build, "approved")),
		wf.On(Revise, review.To(plan, "revise").With(revisions.Inc()))))
	// 6. Every loop has a declared bound, and says where it goes when it hits it.
	wf.MaxVisits(wf.Const(3), plan.To(build, "revision cap: build the latest plan"))

	build.OnClass(wf.Blocked, build.To(stopped, "default blocked route").
		Unreachable("every entry follows an accepted Plan; the fixed mapper returns its tasks and duplicate keys deduplicate"))

	// 7. A join decides what the parallel results mean.
	build.Join(wf.AllOK(),
		build.To(ship, "every task built"),
		build.To(stopped, "a task gave up").Ends("partial work is kept on its branches"))

	// 8. A gate is the only place a person is asked; it has a timeout and a default.
	ship.Presents(planDoc).Answers(
		wf.On(Go, ship.To(shipped, "person approved").Ends(wf.Success, "the person approved shipping")),
		wf.On(Hold, ship.To(stopped, "person held it").Ends(wf.Stopped, "the person withheld shipping approval")),
	).Timeout(24*time.Hour, ship.To(stopped, "nobody answered"))
	return d
}

// The child workflow, run once per task: implement, test, repair, capped.
func taskWorkflow() wf.Child[Task, TaskResult] {
	d := wf.New("tour-task")
	implement := wf.NewAgent[Implement, Patch](d)
	// 9. A command state: the engine runs it and routes on the result. No agent
	//    is asked whether the tests passed.
	test := shell.Command[Test](d, shell.Run("sh", "-c", "{suite}", "tour-suite")).Key("tour", "{run}", "{id}", "test").Idempotent()
	done := wf.NewTerminal[TaskDone](d, wf.Success)
	gaveUp := wf.NewTerminal[TaskGaveUp](d, wf.Stopped)
	d.OnFailures(gaveUp, wf.Blocked, wf.Unavailable, wf.Uncertain)

	implement.Spec(wf.PromptText("Implement and commit this task: {{item}}. Last test output: {{test}}",
		wf.ChildInput(), wf.As("test", wf.Out(test))),
		wf.Attempts(2, implement.To(gaveUp, "no usable patch")))
	implement.ThenTo(test, "implemented")
	wf.MaxVisits(wf.Const(3), implement.To(gaveUp, "repair cap"))

	test.Retry(wf.Attempts(2, test.To(gaveUp, "suite would not run")).OnClass(wf.Blocked,
		test.To(gaveUp, "default blocked route").Unreachable("execution errors use retry classes; the router always selects an unguarded declared edge")))
	green, red := test.To(done, "tests pass"), test.To(implement, "tests fail: repair")
	test.RouteExit(green, red)

	wf.Yield(done, func(c wf.Ctx) TaskResult { return TaskResult{Summary: wf.OutputOf(c, implement).Summary} })
	wf.Yield(gaveUp, func(wf.Ctx) TaskResult { return TaskResult{} })
	return wf.Typed[Task, TaskResult](d)
}

// checkReview validates completeness; the engine derives the verdict from Met.
func checkReview(r ReviewOut, _ wf.Ctx) error {
	if len(r.Rows) != 2 {
		return errors.New("review needs goal and independent evidence rows")
	}
	seen := map[string]bool{}
	for _, row := range r.Rows {
		if (row.ID != "goal" && row.ID != "independent") || seen[row.ID] || strings.TrimSpace(row.Evidence) == "" {
			return errors.New("review needs unique goal and independent rows with nonempty evidence")
		}
		seen[row.ID] = true
	}
	return nil
}

func reviewVerdict(r ReviewOut) Verdict {
	for _, row := range r.Rows {
		if !row.Met {
			return Revise
		}
	}
	return OK
}
