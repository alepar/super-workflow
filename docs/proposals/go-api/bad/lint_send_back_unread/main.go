//go:build badexample

// Negative example (lint): a send-back's producer never binds wf.Returned(),
// so the reasons would never reach it.
//
// want: send-back-unread
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Verdict struct {
	OK       bool
	Problems []string
}

type (
	Draft  struct{}
	Review struct{}
	Done   struct{}
	Stop   struct{}
)

func def() *wf.Def {
	d := wf.New("bad-send-back-unread")
	draft := wf.NewAgent[Draft, string](d)
	review := wf.NewAgent[Review, Verdict](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	stop := wf.NewTerminal[Stop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	// BAD: the producer's prompt does not bind wf.Returned().
	draft.Spec(wf.PromptText("draft"), wf.Retry[Draft](wf.Const(1), draft.To(stop, "gave up")))
	draft.Then(draft.To(review, "drafted"))
	review.Spec(wf.PromptText("review {{x}}", wf.As("x", wf.Out(draft))), wf.Retry[Review](wf.Const(1), review.To(stop, "review gave up")))
	ok := review.To(done, "ok")
	back := review.SendBack(draft, "revise", func(v Verdict) []string { return v.Problems }, wf.Const(2),
		review.To(stop, "not fixed").Ends(wf.Stopped, "the draft was not fixed"))
	review.Route(func(v Verdict, _ wf.Ctx) wf.Edge[Review] {
		if v.OK {
			return ok
		}
		return back
	}, ok, back)
	return d
}

func main() { wf.Main(def()) }
