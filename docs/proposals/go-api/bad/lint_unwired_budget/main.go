//go:build badexample

// Negative example (lint): a budget reads a counter that no edge increments.
//
// want: budget-unwired
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	A    struct{}
	Done struct{}
)

func def() *wf.Def {
	d := wf.New("bad-unwired-budget")
	rounds := d.Counter("rounds")
	enum := wf.NewEnum(d, "v", "ok")
	a := wf.NewAgent[A, string](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ok := a.To(done, "ok")
	// BAD: nothing ever calls rounds.Inc(), so this cap can never change.
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1).Plus(wf.Count(rounds)), ok))
	a.RouteCases(wf.Cases(func(s string) string { return s }, enum, wf.On("ok", ok)))
	return d
}

func main() { wf.Main(def()) }
