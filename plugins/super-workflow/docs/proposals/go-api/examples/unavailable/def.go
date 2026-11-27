// Package unavailable is VISION E15: tool unavailable versus semantic failure.
// An agent whose tool is down (outcome class unavailable) takes its own edge
// and files a blocker bead; running out of attempts on a result that keeps
// failing its checks (the semantic route) ends elsewhere. The two are never
// the same terminal.
package unavailable

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TriageOut is the agent's output.
type TriageOut struct{ Summary string }

// Marker types, one per state.
type (
	Triage       struct{}
	TriageDone   struct{}
	ToolBlocked  struct{}
	NotConverged struct{}
)

// Definition returns the unavailable definition.
func Definition() *wf.Def {
	d := wf.New("unavailable").Version(1)

	triage := wf.NewAgent[Triage, TriageOut](d)
	done := wf.NewTerminal[TriageDone](d, wf.Success)
	blocked := wf.NewTerminal[ToolBlocked](d, wf.Stopped)
	failed := wf.NewTerminal[NotConverged](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)

	blocker := bd.Issue{
		Title:       "triage tool unavailable",
		Description: "The triage tool could not be reached; the run stopped without a semantic verdict.",
		Type:        "task",
		Labels:      []string{"blocker"},
	}
	triage.Spec(wf.PromptText("Triage the report with the triage tool."),
		wf.Retry(wf.Const(3), triage.To(failed, "three semantic failures")).
			OnClass(wf.Unavailable, triage.To(blocked, "tool unavailable: file a blocker").
				With(bd.Create(bd.IssueKey("blocker", blocker.Title), blocker))),
		wf.Checkf("verdict-present", func(TriageOut, wf.Ctx) error { return nil }))
	ok := triage.To(done, "triaged")
	triage.Route(func(TriageOut, wf.Ctx) wf.Edge[Triage] { return ok }, ok)
	return d
}
