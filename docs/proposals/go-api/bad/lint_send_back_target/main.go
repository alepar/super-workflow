//go:build badexample

// Negative example (lint): a send-back targets a state that did not produce
// the work: here a compute, which has no agent to repair it.
//
// want: send-back-target
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Verdict struct {
	OK       bool
	Problems []string
}

type (
	Prepare struct{}
	Review  struct{}
	Done    struct{}
	Stop    struct{}
)

func def() *wf.Def {
	d := wf.New("bad-send-back-target")
	prepare := wf.NewCompute[Prepare, string](d, func(wf.Ctx) (string, error) { return "x", nil })
	review := wf.NewAgent[Review, Verdict](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	stop := wf.NewTerminal[Stop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	prepare.Then(prepare.To(review, "prepared"))
	review.Spec(wf.PromptText("review {{x}}", wf.As("x", wf.Out(prepare))), wf.Retry[Review](wf.Const(1), review.To(stop, "gave up")))
	ok := review.To(done, "ok")
	// BAD: only an agent can repair what it wrote.
	back := review.SendBack(prepare, "redo", func(v Verdict) []string { return v.Problems }, wf.Const(2),
		review.To(stop, "not fixed").Ends(wf.Stopped, "the work was not fixed"))
	review.Route(func(v Verdict, _ wf.Ctx) wf.Edge[Review] {
		if v.OK {
			return ok
		}
		return back
	}, ok, back)
	return d
}

func main() { wf.Main(def()) }
