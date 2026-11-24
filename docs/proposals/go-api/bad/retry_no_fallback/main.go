//go:build badexample

// Negative example: a retry policy without its exhausted-attempts fallback.
//
// want: not enough arguments in call to wf.Retry
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Out struct{ OK bool }

type (
	A    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-retry")
	a := wf.NewAgent[A, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ok := a.To(done, "ok")
	// BAD: Retry requires the edge taken when attempts are exhausted.
	a.Spec(wf.PromptText("p"), wf.Retry[A](wf.Const(3)))
	a.Route(func(Out, wf.Ctx) wf.Edge[A] { return ok }, ok)
	wf.Main(d)
}
