//go:build badexample

// Negative example: a field handle names a field the options struct lacks.
//
// want: o.Autonomouz undefined
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Opts struct{ Autonomous bool }

type (
	Ask  struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-option-field-misspelled")
	opts := wf.Options[Opts](d)
	answers := wf.NewEnum(d, "answer", "go")
	ask := wf.NewGate[Ask](d, "Go?", answers, wf.PolicyAnswerable)
	done := wf.NewTerminal[Done](d, wf.Success)
	ask.Answers(wf.On("go", ask.To(done, "go")))
	// BAD: Autonomouz is not a field of Opts.
	auto := wf.Field(opts, func(o *Opts) *bool { return &o.Autonomouz })
	d.SetPolicy(wf.Policy(wf.PolicyAnswer(ask, "go", auto.Is(true))))
	wf.Main(d)
}
