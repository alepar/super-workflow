//go:build badexample

// Negative example (lint): a child terminal has no parent edge.
//
// want: sub-exit-unmapped
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
	bad := wf.NewTerminal[Fail](d, wf.Stopped)
	wf.Yield(ok, func(wf.Ctx) Res { return Res{} })
	wf.Yield(bad, func(wf.Ctx) Res { return Res{} })
	return wf.Typed[Item, Res](d)
}

func def() *wf.Def {
	d := wf.New("bad-sub-exit")
	nest := wf.NewSub[Nest](d, child(), func(wf.Ctx) Item { return Item{} })
	end := wf.NewTerminal[Final](d, wf.Success)
	// BAD: the child can end in Fail, which has no OnExit mapping.
	nest.Exits(wf.OnExit[Kid](nest.To(end, "kid ok")))
	return d
}

func main() { wf.Main(def()) }
