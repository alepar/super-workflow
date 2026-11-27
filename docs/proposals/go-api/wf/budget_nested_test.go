package wf_test

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A budget stop below the top level (sw-zpeo): an inner Map whose join
// tolerates one lost seat reaches its success terminal, and so does the
// outer Map and the run. The stop must still show on the outer item.

type (
	bnOuter struct{}
	bnInner struct{}
	bnVote  struct{}
	bnYes   struct{}
	bnMet   struct{}
	bnShort struct{}
	bnAll   struct{}
	bnSome  struct{}
)

type bnBallot struct{ OK bool }

func bnDef() *wf.Def {
	sd := wf.New("bn-seat")
	v := wf.NewAgent[bnVote, bnBallot](sd)
	yes := wf.NewTerminal[bnYes](sd, wf.Success)
	v.Spec(wf.PromptText("vote as seat {{item}}", wf.ChildInput()), wf.Retry[bnVote](wf.Const(1), v.To(yes, "gave up")))
	toYes := v.To(yes, "voted")
	v.Route(func(bnBallot, wf.Ctx) wf.Edge[bnVote] { return toYes }, toYes)
	wf.Yield(yes, func(wf.Ctx) bool { return true })

	cd := wf.New("bn-candidate")
	inner := wf.NewMap[bnInner, string, string, bool](cd, wf.FromValues("s1", "s2"), func(s string) string { return s }, wf.Typed[string, bool](sd))
	met := wf.NewTerminal[bnMet](cd, wf.Success)
	short := wf.NewTerminal[bnShort](cd, wf.Stopped)
	inner.Parallel(2).Join(wf.Quorum(1), inner.To(met, "a seat voted"), inner.To(short, "no seat voted"))
	wf.Yield(met, func(wf.Ctx) bool { return true })
	wf.Yield(short, func(wf.Ctx) bool { return false })

	d := wf.New("bn-panels")
	outer := wf.NewMap[bnOuter, string, string, bool](d, wf.FromValues("c1"), func(s string) string { return s }, wf.Typed[string, bool](cd))
	all := wf.NewTerminal[bnAll](d, wf.Success)
	some := wf.NewTerminal[bnSome](d, wf.Stopped)
	outer.Join(wf.AllOK(), outer.To(all, "every panel met"), outer.To(some, "a panel fell short"))
	return d
}

func TestNestedBudgetStopVisibleOnOuterItem(t *testing.T) {
	d := bnDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %v, want 1: the second seat is stopped on the budget", childIDs(ins))
	}
	h.submit(run, ins[0], `{"OK":true}`)
	answerAll(h, run, okOut)
	if got := len(h.kind(run, wf.JournalBudgetStopped)); got != 1 {
		t.Fatalf("budget_stopped = %d, want 1", got)
	}
	statusOf(t, h.status(run), wf.RunEnded, "bnAll")
	var out wf.MapResult[string, bool]
	if err := json.Unmarshal(acceptedOf(h, run, "bnOuter"), &out); err != nil {
		t.Fatal(err)
	}
	it := out.Items["c1"]
	if it.Terminal != "bnMet" || it.BudgetStopped || !it.BudgetStoppedBelow {
		t.Fatalf("outer item = %+v, want bnMet, not itself stopped, with a stop below it", it)
	}
	var panel wf.MapResult[string, bool]
	for _, r := range h.kind(run, wf.JournalAccepted) {
		var ad wf.AcceptedData
		if r.State == "bnInner" && json.Unmarshal(r.Data, &ad) == nil {
			if err := json.Unmarshal(ad.Output, &panel); err != nil {
				t.Fatal(err)
			}
		}
	}
	var stopped int
	for _, seat := range panel.Items {
		if seat.BudgetStopped {
			stopped++
		}
		if seat.BudgetStoppedBelow {
			t.Fatalf("seat %+v: a leaf has nothing below it", seat)
		}
	}
	if stopped != 1 {
		t.Fatalf("inner items = %+v, want one stopped seat", panel.Items)
	}
}
