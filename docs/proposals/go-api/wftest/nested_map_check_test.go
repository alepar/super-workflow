package wftest_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// The nested Map fixture of the engine guarantee (sw-fcsg): a panel per
// candidate. The outer Map runs one child per candidate; each child runs an
// inner Map with one child per seat, and each seat votes once. A reject vote
// is a seat that succeeded with false; only a failure stops a seat.
type (
	nmCand  struct{}
	nmSeat  struct{}
	nmVote  struct{}
	nmYes   struct{}
	nmNay   struct{}
	nmNo    struct{}
	nmMet   struct{}
	nmShort struct{}
	nmAll   struct{}
	nmSome  struct{}
)

type nmBallot struct{ OK bool }

func nestedPanelsDef() *wf.Def {
	sd := wf.New("nm-seat")
	v := wf.NewAgent[nmVote, nmBallot](sd)
	yes := wf.NewTerminal[nmYes](sd, wf.Success)
	nay := wf.NewTerminal[nmNay](sd, wf.Success)
	no := wf.NewTerminal[nmNo](sd, wf.Stopped)
	sd.OnFailure(wf.Blocked, no)
	v.Spec(wf.PromptText("vote as seat {{item}}", wf.ChildInput()), wf.Retry[nmVote](wf.Const(1), v.To(no, "gave up")))
	toYes, toNo := v.To(yes, "yes"), v.To(nay, "no")
	v.Route(func(b nmBallot, _ wf.Ctx) wf.Edge[nmVote] {
		if b.OK {
			return toYes
		}
		return toNo
	}, toYes, toNo)
	wf.Yield(yes, func(wf.Ctx) bool { return true })
	wf.Yield(nay, func(wf.Ctx) bool { return false })
	wf.Yield(no, func(wf.Ctx) bool { return false })

	cd := wf.New("nm-candidate")
	inner := wf.NewMap[nmSeat, int, string, bool](cd, wf.FromValues(1, 2, 3), func(i int) string { return fmt.Sprintf("s%d", i) }, wf.Typed[int, bool](sd))
	met := wf.NewTerminal[nmMet](cd, wf.Success)
	short := wf.NewTerminal[nmShort](cd, wf.Stopped)
	inner.Parallel(2).Join(wf.AllOK(), inner.To(met, "panel agreed"), inner.To(short, "panel split"))
	wf.Yield(met, func(wf.Ctx) bool { return true })
	wf.Yield(short, func(wf.Ctx) bool { return false })

	d := wf.New("nm-panels")
	outer := wf.NewMap[nmCand, string, string, bool](d, wf.FromValues("c1", "c2"), func(s string) string { return s }, wf.Typed[string, bool](cd))
	all := wf.NewTerminal[nmAll](d, wf.Success)
	some := wf.NewTerminal[nmSome](d, wf.Stopped)
	outer.Parallel(2).Join(wf.AllOK(), outer.To(all, "every panel agreed"), outer.To(some, "a panel split").Ends("a split panel is the reported result"))
	return d
}

// Check passes the nested definition: every state and edge at both levels is
// covered across its mixed and liveness seeds, with no property violation.
// Crash recovery of the same shape is swept append by append in wf's
// TestNestedMapCrashSweep.
func TestCheckNestedMap(t *testing.T) {
	ballots := []json.RawMessage{json.RawMessage(`{"OK":true}`), json.RawMessage(`{"OK":false}`)}
	got := wftest.Check(func() *wftest.Sim { return wftest.New(nestedPanelsDef()) }, wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{
		Valid:    map[string][]json.RawMessage{"nmVote": ballots},
		Progress: map[string][]json.RawMessage{"nmVote": ballots},
	}})
	for _, p := range got.Problems {
		t.Error(p)
	}
	if got.MixedSeeds == 0 || got.LivenessSeeds == 0 || got.Coverage == nil || len(got.Coverage.EdgeProblems()) != 0 {
		t.Fatalf("want mixed and liveness seeds and every edge covered: %s", got)
	}
}
