//go:build badexample

// Negative example: a state's Route is given another state's edge as a candidate.
//
// want: as wf.Edge[A] value in argument to a.Route
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Out struct{ OK bool }

type (
	A    struct{}
	B    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-route-foreign-candidate")
	a := wf.NewAgent[A, Out](d)
	b := wf.NewAgent[B, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ab := a.To(b, "ok")
	bDone := b.To(done, "ok")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ab))
	b.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), bDone))
	// BAD: bDone is an edge of B; A's candidates must be edges of A.
	a.Route(func(Out, wf.Ctx) wf.Edge[A] { return ab }, ab, bDone)
	b.Route(func(Out, wf.Ctx) wf.Edge[B] { return bDone }, bDone)
	wf.Main(d)
}
