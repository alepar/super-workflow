//go:build badexample

// Negative example: a state's route returns another state's edge.
//
// want: cannot use bDone (variable of struct type wf.Edge[B]) as wf.Edge[A]
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Out struct{ OK bool }

type (
	A    struct{}
	B    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-foreign-edge")
	a := wf.NewAgent[A, Out](d)
	b := wf.NewAgent[B, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ab := a.To(b, "ok")
	bDone := b.To(done, "ok")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ab))
	b.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), bDone))
	// BAD: A's route returns an edge declared from B.
	a.Route(func(Out, wf.Ctx) wf.Edge[A] { return bDone }, bDone)
	b.Route(func(Out, wf.Ctx) wf.Edge[B] { return bDone }, bDone)
	wf.Main(d)
}
