//go:build badexample

// Negative example: a field handle is compared with a value of the wrong type.
//
// want: cannot use "yes" (untyped string constant) as bool value
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Opts struct{ Autonomous bool }

type (
	Ask  struct{}
	Done struct{}
)

func main() {
	d := wf.New("bad-option-field-wrong-type")
	opts := wf.Options[Opts](d)
	answers := wf.NewEnum(d, "answer", "go")
	ask := wf.NewGate[Ask](d, "Go?", answers, wf.PolicyAnswerable)
	done := wf.NewTerminal[Done](d, wf.Success)
	ask.Answers(wf.On("go", ask.To(done, "go")))
	// BAD: Autonomous is a bool; the value does not match the handle's type.
	auto := wf.Field(opts, func(o *Opts) *bool { return &o.Autonomous })
	d.SetPolicy(wf.Policy(wf.PolicyAnswer(ask, "go", auto.Is("yes"))))
	wf.Main(d)
}
