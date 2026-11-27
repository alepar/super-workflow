// Package fanout is dynamic fan-out with per-item verification and a join
// (lane D P3): a Map over the ready tasks of an epic, each item running a small
// agent-plus-check child, joined with AllSettled so one failed item does not
// hide the others.
package fanout

import (
	"errors"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types (parent, then child).
type (
	Fan     struct{}
	Merged  struct{}
	Partial struct{}

	Work     struct{}
	ItemDone struct{}
	ItemFail struct{}
)

// ItemResult is the child's typed result for one task.
type ItemResult struct {
	ID string
	OK bool
}

// WorkOut is the per-item agent's output.
type WorkOut struct {
	Summary string
	Tested  bool
}

func itemChild() wf.Child[bd.Issue, ItemResult] {
	d := wf.New("fanout-item")
	work := wf.NewAgent[Work, WorkOut](d)
	done := wf.NewTerminal[ItemDone](d, wf.Success)
	fail := wf.NewTerminal[ItemFail](d, wf.Stopped)
	d.OnFailure(wf.Blocked, fail)
	work.Spec(wf.PromptText("Implement the task {{item}} and run its tests.", wf.ChildInput()),
		wf.Retry[Work](wf.Const(2), work.To(fail, "tests still failing")),
		wf.Checkf("tests were run", func(o WorkOut, _ wf.Ctx) error {
			if !o.Tested {
				return errNotTested
			}
			return nil
		}))
	workNext := work.To(done, "verified")
	work.Route(func(WorkOut, wf.Ctx) wf.Edge[Work] { return workNext }, workNext)
	wf.Yield(done, func(wf.Ctx) ItemResult { return ItemResult{OK: true} })
	wf.Yield(fail, func(wf.Ctx) ItemResult { return ItemResult{} })
	return wf.Typed[bd.Issue, ItemResult](d)
}

var errNotTested = errors.New("the item's tests were not run")

// Definition builds the fan-out.
func Definition() *wf.Def {
	d := wf.New("fanout")
	fan := wf.NewMap[Fan, bd.Issue, string, ItemResult](d,
		wf.FromObservation(bd.Ready("{epic}")),
		func(i bd.Issue) string { return i.ID },
		itemChild())
	merged := wf.NewTerminal[Merged](d, wf.Success)
	partial := wf.NewTerminal[Partial](d, wf.Stopped)

	fan.Join(wf.AllSettled(),
		fan.To(merged, "all items settled"),
		fan.To(partial, "some items failed").Ends("Preserve partial results for follow-up after item failure"))
	return d
}
