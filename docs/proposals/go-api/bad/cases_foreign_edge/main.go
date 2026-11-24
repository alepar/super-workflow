//go:build badexample

// Negative example: a Cases table of state A holding state B's edge.
//
// want: (value of struct type wf.CaseTable[B, Out]) as wf.CaseTable[A, Out] value in argument to a.RouteCases
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Out struct{ V string }

type (
	A    struct{}
	B    struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-cases-edge")
	a := wf.NewAgent[A, Out](d)
	b := wf.NewAgent[B, Out](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	enum := wf.NewEnum(d, "v", "x")
	ab := a.To(b, "ok")
	bEdge := b.To(done, "ok")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ab))
	b.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), bEdge))
	// BAD: bEdge belongs to B, but A's table can only hold Edge[A].
	a.RouteCases(wf.Cases(func(o Out) string { return o.V }, enum, wf.On("x", bEdge)))
	b.Route(func(Out, wf.Ctx) wf.Edge[B] { return bEdge }, bEdge)
	wf.Main(d)
}
