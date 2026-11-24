//go:build badexample

// Negative example: an output check typed for a different output than the
// agent's.
//
// want: (value of struct type wf.Check[Other]) as wf.Check[Out] value in argument to a.Spec
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	Out   struct{ OK bool }
	Other struct{ N int }
)

type (
	A    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-check-type")
	a := wf.NewAgent[A, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ok := a.To(done, "ok")
	// BAD: the agent produces Out; the check reads Other.
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ok),
		wf.Checkf("x", func(o Other, _ wf.Ctx) error { return nil }))
	a.Route(func(Out, wf.Ctx) wf.Edge[A] { return ok }, ok)
	wf.Main(d)
}
