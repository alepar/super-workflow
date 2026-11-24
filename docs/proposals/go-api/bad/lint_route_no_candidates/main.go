//go:build badexample

// Negative example (lint): a free-form Route declares no candidate edges.
//
// want: route-no-candidates
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	A    struct{}
	Done struct{}
)

func def() *wf.Def {
	d := wf.New("bad-route-no-candidates")
	a := wf.NewAgent[A, string](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	toDone := a.To(done, "done")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), toDone))
	// BAD: the closure picks an edge, but no candidate is declared, so the
	// walk, render, hash and lint cannot see where it may go.
	a.Route(func(string, wf.Ctx) wf.Edge[A] { return toDone })
	return d
}

func main() { wf.Main(def()) }
