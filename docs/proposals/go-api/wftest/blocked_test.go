package wftest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	blkAsk  struct{}
	blkMake struct{}
	blkOK   struct{}
	blkExit int
	blkOut  struct{ OK bool }
)

// TestCapCoordinatorReportsBlocked: the scripted coordinator hears the blocked
// event of a halted command step and says so when a later script step finds
// nothing to act on.
func TestCapCoordinatorReportsBlocked(t *testing.T) {
	d := wf.New("blocked")
	exits := wf.NewEnum(d, "blk-exit", blkExit(0))
	ask := wf.NewAgent[blkAsk, blkOut](d)
	cmd := wf.NewCommand[blkMake](d, "make")
	ok := wf.NewTerminal[blkOK](d, wf.Success)
	ask.Spec(wf.PromptText("Go."), wf.RetryPolicy[blkAsk]{})
	toCmd := ask.To(cmd, "build")
	ask.Route(func(blkOut, wf.Ctx) wf.Edge[blkAsk] { return toCmd }, toCmd)
	cmd.RouteCases(wf.Cases[blkMake, wf.RunResult](wf.ExitAs[blkExit], exits, wf.On(blkExit(0), cmd.To(ok, "passes"))))

	w := wftest.NewWorld()
	wftest.FailApply(w, "shell", "run", "exec: permission denied")
	rec := &recordingTB{TB: t}
	wftest.New(d, wftest.Cap(1), wftest.WithTB(rec), wftest.WithWorld(w)).Run(
		wftest.Submit(ask, blkOut{OK: true}),
		wftest.SubmitAt("Nowhere", blkOut{}),
	)
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "the run blocked at blkMake") {
		t.Fatalf("messages = %q, want one naming the blocked state blkMake", rec.msgs)
	}
}

type (
	naMake struct{}
	naOK   struct{}
	naStop struct{}
	naExit int
)

// notAppliedDef is a keyed command routed on exit 0; route adds an
// OnClass(Blocked) edge to naStop.
func notAppliedDef(route bool) *wf.Def {
	d := wf.New("not-applied")
	exits := wf.NewEnum(d, "na-exit", naExit(0))
	cmd := wf.NewCommand[naMake](d, "make").Key("build")
	ok := wf.NewTerminal[naOK](d, wf.Success)
	stop := wf.NewTerminal[naStop](d, wf.Stopped)
	cmd.RouteCases(wf.Cases[naMake, wf.RunResult](wf.ExitAs[naExit], exits, wf.On(naExit(0), cmd.To(ok, "passes"))))
	r := wf.Retry[naMake](wf.Const(1), cmd.To(stop, "gave up"))
	if route {
		r = r.OnClass(wf.Blocked, cmd.To(stop, "not applied"))
	}
	cmd.Retry(r)
	return d
}

// settlements are the effect_settled payloads of the trace, in order.
func settlements(t *testing.T, tr *wftest.Trace) []wf.EffectSettledData {
	t.Helper()
	var out []wf.EffectSettledData
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalEffectSettled {
			var s wf.EffectSettledData
			if err := json.Unmarshal(r.Data, &s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
	}
	return out
}

// G4: a keyed command settled not applied is re-issued once with nothing
// spent; the second not-applied settlement takes the Blocked route, and with
// no Blocked route the declared exhausted edge (C3: the run never waits).
func TestNotAppliedReissuesOnceThenBlocked(t *testing.T) {
	script := []wftest.Step{
		wftest.CrashAfterIntent("shell", "run"), wftest.Resume(),
		wftest.Settle(false), // re-issued once, nothing spent
		wftest.CrashAfterIntent("shell", "run"), wftest.Resume(),
		wftest.Settle(false), // Blocked
	}
	for _, route := range []bool{true, false} {
		t.Run(fmt.Sprintf("route=%v", route), func(t *testing.T) {
			w := wftest.NewWorld()
			wftest.EffectUnknown(w, "shell", "run", wf.KeyOf("build"))
			tr := wftest.New(notAppliedDef(route), wftest.WithWorld(w)).Run(script...)
			s := settlements(t, tr)
			if len(s) != 2 || !s[0].Reissued || s[0].Refused == nil || s[0].Refused.Spent || s[1].Reissued || s[1].Refused == nil || s[1].Refused.Spent {
				t.Fatalf("settlements = %+v, want a free re-issue then a Blocked decision, nothing spent", s)
			}
			ended := false
			for _, r := range tr.Journal() {
				ended = ended || r.Kind == wf.JournalRunEnded
			}
			if s[1].Refused.Plan == nil || s[1].Refused.Halt || !ended {
				t.Fatalf("settlement %+v ended=%v, want a route to naStop (Blocked or exhausted)", s[1], ended)
			}
		})
	}
}
