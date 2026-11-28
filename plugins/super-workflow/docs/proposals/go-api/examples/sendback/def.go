// Package sendback returns a document to the agent that wrote it (sw-3op4):
// a reviewer sends the draft back with its findings, at most twice, and the
// drafter repairs its own file in place; once the returns are spent the run
// asks a person. Nothing but the drafter edits the draft.
package sendback

import (
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types.
type (
	Draft    struct{}
	Review   struct{}
	Human    struct{}
	Accepted struct{}
	Dropped  struct{}
)

// Summary is the drafter's output; the document itself is the draft.md
// artifact.
type Summary struct{ Summary string }

// Verdict is the reviewer's output: its findings are the reasons a return
// carries.
type Verdict struct {
	OK       bool
	Findings []string
}

// Decision is the person's answer once the returns are spent.
type Decision string

// The decisions.
const (
	Keep Decision = "keep"
	Drop Decision = "drop"
)

// Definition builds the flow.
func Definition() *wf.Def {
	d := wf.New("sendback")
	decisions := wf.NewEnum(d, "decision", Keep, Drop)
	draft := wf.NewAgent[Draft, Summary](d)
	review := wf.NewAgent[Review, Verdict](d)
	human := wf.NewGate[Human, Decision](d, "The reviewer still has findings after two returns; keep the draft?", decisions, wf.Reserved)
	accepted := wf.NewTerminal[Accepted](d, wf.Success)
	dropped := wf.NewTerminal[Dropped](d, wf.Stopped)
	d.OnFailure(wf.Blocked, human)
	doc := wf.NewArtifact[string](draft, "draft.md")

	// The prompt says what a return means; the engine only supplies it.
	draft.Spec(wf.PromptText("Write the document to draft.md and summarize it. "+
		"If the block below is not empty, the reviewer returned your draft: fix each reason in draft.md, which already holds your prior version.\n{{returned}}",
		wf.As("returned", wf.Returned())),
		wf.Retry[Draft](wf.Const(2), draft.To(human, "drafter gave up")))
	draft.Then(draft.To(review, "drafted"))

	review.Spec(wf.PromptText("Review the document at [DOC]. Approve it, or list each finding that must be fixed.",
		wf.Bound("DOC", wf.ArtifactPath(doc))),
		wf.Retry[Review](wf.Const(2), review.To(human, "reviewer gave up")))
	wf.Consumes(review, doc)
	approved := review.To(accepted, "approved")
	back := review.SendBack(draft, "revise", func(v Verdict) []string { return v.Findings },
		wf.Const(2), review.To(human, "findings remain after two returns"))
	review.Route(func(v Verdict, _ wf.Ctx) wf.Edge[Review] {
		if v.OK {
			return approved
		}
		return back
	}, approved, back)

	human.Answers(
		wf.On(Keep, human.To(accepted, "person keeps").Ends(wf.Success, "the person keeps the draft")),
		wf.On(Drop, human.To(dropped, "person drops").Ends(wf.Stopped, "the person drops the draft")),
	).Timeout(24*time.Hour, human.To(dropped, "no answer"))
	return d
}
