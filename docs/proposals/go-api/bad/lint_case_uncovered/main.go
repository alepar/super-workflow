//go:build badexample

// Negative example (lint): a Cases table misses a value of its Enum.
//
// want: case-uncovered
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	A    struct{}
	Done struct{}
)

func def() *wf.Def {
	d := wf.New("bad-case-uncovered")
	verdicts := wf.NewEnum(d, "verdict", "approved", "changes_requested")
	a := wf.NewAgent[A, string](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ok := a.To(done, "approved")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ok))
	// BAD: "changes_requested" has no case and there is no Otherwise.
	a.RouteCases(wf.Cases(func(s string) string { return s }, verdicts, wf.On("approved", ok)))
	return d
}

func main() { wf.Main(def()) }
