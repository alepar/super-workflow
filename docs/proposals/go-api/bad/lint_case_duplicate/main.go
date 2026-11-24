//go:build badexample

// Negative example (lint): a Cases table maps the same value twice.
//
// want: case-duplicate
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	A    struct{}
	Done struct{}
)

func def() *wf.Def {
	d := wf.New("bad-case-duplicate")
	verdicts := wf.NewEnum(d, "verdict", "approved", "changes_requested")
	a := wf.NewAgent[A, string](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	ok := a.To(done, "approved")
	again := a.To(done, "changes requested")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), ok))
	// BAD: "approved" is mapped twice; the second row can never be taken.
	a.RouteCases(wf.Cases(func(s string) string { return s }, verdicts,
		wf.On("approved", ok), wf.On("approved", again), wf.On("changes_requested", again)))
	return d
}

func main() { wf.Main(def()) }
