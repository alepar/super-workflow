// Package dispatch is VISION E14: every agent instruction is "spawn a
// subagent", on any harness. The definition states the hints that subagent is
// spawned with (wf.ModelTier, wf.Effort, wf.AgentType). The fan-out sets no
// Parallel limit: how many items run at once is the coordinator's own take
// (the n it passes to wf next), not the definition's.
package dispatch

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Item is one unit of work.
type Item struct{ Name string }

// ItemResult is a child's typed result.
type ItemResult struct{ Reviewed bool }

// Marker types, one per state.
type (
	Review       struct{}
	ReviewDone   struct{}
	ReviewFailed struct{}
	Fan          struct{}
	Merged       struct{}
	Partial      struct{}
)

func itemChild() wf.Child[Item, ItemResult] {
	d := wf.New("dispatch-item")
	review := wf.NewAgent[Review, ItemResult](d)
	done := wf.NewTerminal[ReviewDone](d, wf.Success)
	failed := wf.NewTerminal[ReviewFailed](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)
	review.Spec(wf.PromptText("Review {{item}}.", wf.ChildInput()),
		wf.Retry(wf.Const(1), review.To(failed, "review dead")))
	review.Dispatch(wf.ModelTier(wf.TierStandard), wf.Effort("low"), wf.AgentType("reviewer"))
	ok := review.To(done, "reviewed")
	review.Route(func(ItemResult, wf.Ctx) wf.Edge[Review] { return ok }, ok)
	res := func(wf.Ctx) ItemResult { return ItemResult{} }
	wf.Yield(done, res)
	wf.Yield(failed, res)
	return wf.Typed[Item, ItemResult](d)
}

// Definition returns the dispatch definition.
func Definition() *wf.Def {
	d := wf.New("dispatch").Version(1)
	fan := wf.NewMap[Fan, Item, string, ItemResult](d,
		wf.FromObservation(fsx.ReadJSON[[]Item]("work/items.json", "/items")),
		func(i Item) string { return i.Name }, itemChild())
	merged := wf.NewTerminal[Merged](d, wf.Success)
	partial := wf.NewTerminal[Partial](d, wf.Stopped)

	fan.Join(wf.AllSettled(), fan.To(merged, "all items settled"), fan.To(partial, "an item died").Ends("The review batch cannot complete with a failed item"))
	return d
}
