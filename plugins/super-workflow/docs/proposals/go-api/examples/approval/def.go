// Package approval is a human approval gate with a timeout and a pre-committed
// plan (lane D P8, VISION E12): the plan is written and committed before the
// gate opens, the gate presents exactly that artifact, and a human who never
// answers stops the run instead of leaving it parked forever. The gate is
// Reserved: no autonomy policy may answer it.
package approval

import (
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types.
type (
	Draft    struct{}
	Approve  struct{}
	Execute  struct{}
	Done     struct{}
	Rejected struct{}
	Expired  struct{}
	Failed   struct{}
)

// Answer is the approval gate's answer set.
type Answer string

// The answers.
const (
	Approved Answer = "approve"
	Declined Answer = "decline"
)

// PlanDoc is the plan the human approves.
type PlanDoc struct{ Steps []string }

// PlanState is the closed set of drafter results.
type PlanState string

// The plan states.
const (
	PlanReady PlanState = "ready"
	PlanEmpty PlanState = "empty"
)

// PlanOut is the drafter's output.
type PlanOut struct {
	State PlanState
	Path  string
}

// Outcome is the executor agent's output.
type Outcome struct{ Summary string }

// Definition builds the approval flow.
func Definition() *wf.Def {
	d := wf.New("approval")
	answers := wf.NewEnum(d, "answer", Approved, Declined)
	planStates := wf.NewEnum(d, "plan-state", PlanReady, PlanEmpty)

	draft := wf.NewAgent[Draft, PlanOut](d)
	// The plan is committed to the repository, so its path is the product's.
	plan := wf.NewRepoArtifact[PlanDoc](draft, "plan", "{run}/plan.md")
	approve := wf.NewGate[Approve, Answer](d, "Run the committed plan?", answers, wf.Reserved)
	execute := wf.NewAgent[Execute, Outcome](d)
	wf.Consumes(execute, plan)
	done := wf.NewTerminal[Done](d, wf.Success)
	rejected := wf.NewTerminal[Rejected](d, wf.Stopped)
	expired := wf.NewTerminal[Expired](d, wf.Stopped)
	failed := wf.NewTerminal[Failed](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)

	draft.Spec(wf.PromptText("Write the plan to {{run}}/plan.md."),
		wf.Retry[Draft](wf.Const(2), draft.To(failed, "no plan")))
	// Committing the plan is the edge's effect, keyed by the plan path, so a
	// replayed edge does not commit twice.
	planned := draft.To(approve, "plan written").With(
		git.Commit(wf.KeyOf("approval", "plan"), "{worktree}", "plan: awaiting approval", plan.Path()))
	draft.RouteCases(wf.Cases[Draft, PlanOut](func(p PlanOut) PlanState { return p.State }, planStates,
		wf.On(PlanReady, planned),
		wf.On(PlanEmpty, draft.To(failed, "empty plan").Ends("There is no plan to present for approval")),
	))

	approve.Presents(plan).Answers(
		wf.On(Approved, approve.To(execute, "approved")),
		wf.On(Declined, approve.To(rejected, "declined").Ends(wf.Stopped, "the person declined the plan")),
	).Timeout(48*time.Hour, approve.To(expired, "no answer"))

	execute.Spec(wf.PromptText("Execute the approved plan exactly."),
		wf.Retry[Execute](wf.Const(2), execute.To(failed, "execution failed")))
	executeNext := execute.To(done, "executed")
	execute.Route(func(Outcome, wf.Ctx) wf.Edge[Execute] { return executeNext }, executeNext)
	return d
}
