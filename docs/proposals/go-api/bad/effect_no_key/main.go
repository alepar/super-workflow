//go:build badexample

// Negative example: an external effect without its idempotency key.
//
// want: not enough arguments in call to bd.Create
package main

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type Out struct{ OK bool }

type (
	A    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-effect-key")
	a := wf.NewAgent[A, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	// BAD: bd.Create takes the idempotency key as its first parameter.
	ok := a.To(done, "ok").With(bd.Create(bd.Issue{Title: "x"}))
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ok))
	a.Route(func(Out, wf.Ctx) wf.Edge[A] { return ok }, ok)
	wf.Main(d)
}
