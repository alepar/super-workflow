//go:build badexample

// Negative example (lint): a terminal of a typed child does not Yield the
// child's result.
//
// want: sub-result-type
package main

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

type (
	Item  struct{}
	Res   struct{}
	Kid   struct{}
	Fail  struct{}
	Nest  struct{}
	Final struct{}
)

func child() wf.Child[Item, Res] {
	d := wf.New("kid")
	ok := wf.NewTerminal[Kid](d, wf.Success)
	wf.NewTerminal[Fail](d, wf.Stopped)
	wf.Yield(ok, func(wf.Ctx) Res { return Res{} })
	// BAD: Fail is a terminal of a typed child but never Yields a Res.
	return wf.Typed[Item, Res](d)
}

func def() *wf.Def {
	d := wf.New("bad-sub-unyielded")
	nest := wf.NewSub[Nest](d, child(), func(wf.Ctx) Item { return Item{} })
	end := wf.NewTerminal[Final](d, wf.Success)
	nest.Exits(wf.OnExit[Kid](nest.To(end, "kid ok")), wf.OnExit[Fail](nest.To(end, "kid failed")))
	return d
}

func main() { wf.Main(def()) }
