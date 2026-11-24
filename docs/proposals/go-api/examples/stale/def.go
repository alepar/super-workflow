// Package stale is VISION E16: a stale observation, and a definition change
// across a resume. The review's check observes git.Head, so when the head moves
// between the instruction and the submission the submission is classified
// stale and the run goes back to waiting instead of accepting a verdict about
// code that no longer exists, up to three review visits. The definition is at
// Version 2; a Migration maps
// the states of version 1 onto it so an older run can resume.
package stale

import (
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Verdict is the review's output.
type Verdict struct{ Approve bool }

// Marker types, one per state.
type (
	AwaitHead struct{}
	Review    struct{}
	Reviewed  struct{}
	TimedOut  struct{}
	GaveUp    struct{}
)

// The pinned hashes of the two versions as the executor recorded them.
const (
	hashV1 = "sha256:v1-pin"
	hashV2 = "sha256:v2-pin"
)

// Definition returns the stale definition.
func Definition() *wf.Def {
	d := wf.New("stale").Version(2)
	d.Migrations(wf.Migration{FromHash: hashV1, ToHash: hashV2, States: map[string]string{"WaitHead": "AwaitHead"}})
	head := git.Head(".")

	wait := wf.NewWait[AwaitHead](d, head, "head moved past the reviewed commit", func(h string) bool { return h != "" })
	review := wf.NewAgent[Review, Verdict](d)
	reviewed := wf.NewTerminal[Reviewed](d, wf.Success)
	timedOut := wf.NewTerminal[TimedOut](d, wf.Stopped)
	gaveUp := wf.NewTerminal[GaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)

	wait.Ready(wait.To(review, "head moved")).
		Poll(30*time.Second).
		Timeout(24*time.Hour, wait.To(timedOut, "no new commit in a day"))
	review.Spec(wf.PromptText("Review the commits up to the observed head."),
		wf.Retry(wf.Const(2), review.To(gaveUp, "review failed twice")).
			OnClass(wf.Stale, review.To(wait, "head moved under the review: observe again")),
		wf.Checkf("head-unchanged", func(Verdict, wf.Ctx) error { return nil }).Observes(head))
	ok := review.To(reviewed, "reviewed")
	review.Route(func(Verdict, wf.Ctx) wf.Edge[Review] { return ok }, ok)
	// Bound head churn across visits, independently of per-visit retries.
	wf.MaxVisits(wf.Const(3), review.To(gaveUp, "head kept moving across three reviews"))
	return d
}
