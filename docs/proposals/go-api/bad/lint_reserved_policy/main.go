//go:build badexample

// Negative example (lint): an autonomy-policy rule answers a reserved gate.
//
// want: reserved-gate-policy
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type Opts struct{ Autonomous bool }

type (
	A    struct{}
	Ask  struct{}
	Done struct{}
)

func def() *wf.Def {
	d := wf.New("bad-reserved-policy")
	opts := wf.Options[Opts](d)
	answers := wf.NewEnum(d, "answer", "go")
	a := wf.NewAgent[A, string](d)
	ask := wf.NewGate[Ask](d, "Ship it?", answers, wf.Reserved)
	done := wf.NewTerminal[Done](d, wf.Success)
	toAsk := a.To(ask, "ask")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), toAsk))
	a.Route(func(string, wf.Ctx) wf.Edge[A] { return toAsk }, toAsk)
	ask.Answers(wf.On("go", ask.To(done, "go")))
	// BAD: a reserved gate always waits for a human; it cannot carry a policy.
	d.SetPolicy(wf.Policy(wf.PolicyAnswer(ask, "go", wf.Field(opts, func(o *Opts) *bool { return &o.Autonomous }).Is(true))))
	return d
}

func main() { wf.Main(def()) }
