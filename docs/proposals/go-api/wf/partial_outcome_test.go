package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// an earlier revision: a run that hands off not ready ends partial, not success.

type (
	poHand struct{}
	poDone struct{}
	poSub  struct{}
	poFin  struct{}
	poStop struct{}
)

type poOut struct{ Ready bool }

// poDef hands off once and ends at a Success terminal that ends partial when
// the hand-off was not ready.
func poDef() *wf.Def {
	d := wf.New("partial-outcome")
	a := wf.NewAgent[poHand, poOut](d)
	done := wf.NewTerminal[poDone](d, wf.Success).PartialWhen(func(c wf.Ctx) bool { return !wf.OutputOf(c, a).Ready })
	a.Spec(wf.PromptText("p"), wf.Retry[poHand](wf.Const(1), a.To(done, "gave up")))
	to := a.To(done, "handed off to the coordinator")
	a.Route(func(poOut, wf.Ctx) wf.Edge[poHand] { return to }, to)
	return d
}

func TestPartialWhenEndsANotReadyRunPartial(t *testing.T) {
	d := poDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"poHand": {`{"Ready":false}`}})
	if st.Phase != wf.RunEnded || st.Terminal != "poDone" || st.Outcome != wf.Partial {
		t.Fatalf("status = phase %s terminal %s outcome %s, want ended at poDone, outcome partial", st.Phase, st.Terminal, st.Outcome)
	}
	if st.Cause != "handed off to the coordinator" {
		t.Errorf("cause = %q, want the hand-off edge's label: partial delivered, it did not fail", st.Cause)
	}
	ended := h.kind(run, wf.JournalRunEnded)
	var data wf.RunEndedData
	if len(ended) != 1 || json.Unmarshal(ended[0].Data, &data) != nil || data.Outcome != wf.Partial || data.Terminal != "poDone" {
		t.Fatalf("run_ended = %+v, want one record with outcome partial at poDone", ended)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "outcome partial") {
		t.Errorf("status view does not show the partial outcome:\n%s", view)
	}
	ins, err := h.e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != wf.InstructionEnded || ins[0].Outcome != wf.Partial {
		t.Errorf("next = %+v, want one ended instruction with outcome partial", ins)
	}
}

func TestPartialWhenLeavesAReadyRunSuccess(t *testing.T) {
	d := poDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"poHand": {`{"Ready":true}`}})
	if st.Phase != wf.RunEnded || st.Terminal != "poDone" || st.Outcome != wf.Success {
		t.Fatalf("status = phase %s terminal %s outcome %s, want ended at poDone, outcome success", st.Phase, st.Terminal, st.Outcome)
	}
}

// A child that ends partial delivered its result: it settles Accepted, so the
// parent takes the exit mapped to that terminal, and the settlement records
// the partial outcome.
func TestPartialChildSettlesAccepted(t *testing.T) {
	cd := wf.New("po-child")
	w := wf.NewAgent[poHand, poOut](cd)
	ok := wf.NewTerminal[chOK](cd, wf.Success).PartialWhen(func(c wf.Ctx) bool { return !wf.OutputOf(c, w).Ready })
	bad := wf.NewTerminal[chBad](cd, wf.Stopped)
	w.Spec(wf.PromptText("p"), wf.Retry[poHand](wf.Const(1), w.To(bad, "gave up")))
	to := w.To(ok, "handed off")
	w.Route(func(poOut, wf.Ctx) wf.Edge[poHand] { return to }, to)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(bad, res)

	d := wf.New("po-parent")
	sub := wf.NewSub[poSub, chItem, chRes](d, wf.Typed[chItem, chRes](cd), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[poFin](d, wf.Success)
	stop := wf.NewTerminal[poStop](d, wf.Stopped)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "child handed off")), wf.OnExit[chBad](sub.To(stop, "child stopped")))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"po-child/poHand": {`{"Ready":false}`}})
	if st.Phase != wf.RunEnded || st.Terminal != "poFin" {
		t.Fatalf("status = phase %s terminal %s, want ended at poFin", st.Phase, st.Terminal)
	}
	settled := h.kind(run, wf.JournalChildSettled)
	var data wf.ChildSettledData
	if len(settled) != 1 || json.Unmarshal(settled[0].Data, &data) != nil || data.Outcome != wf.Partial || data.Class != wf.Accepted {
		t.Fatalf("child_settled = %+v, want one Accepted settlement with outcome partial", settled)
	}
}

func TestPartialWhenNeedsASuccessTerminal(t *testing.T) {
	d := wf.New("po-misuse")
	a := wf.NewAgent[poHand, poOut](d)
	stop := wf.NewTerminal[poStop](d, wf.Stopped).PartialWhen(func(wf.Ctx) bool { return true })
	a.Spec(wf.PromptText("p"), wf.Retry[poHand](wf.Const(1), a.To(stop, "gave up")))
	to := a.To(stop, "stop")
	a.Route(func(poOut, wf.Ctx) wf.Edge[poHand] { return to }, to)
	if ps := d.Lint(); !hasRule(ps, wf.RuleBuild) {
		t.Fatalf("lint = %v, want a build problem: PartialWhen on a Stopped terminal", ps)
	}
}

// an earlier revision round 1: a parent reads how its Sub child ended (ChildOutcome),
// so a parent terminal can end partial when its child delivered partial
// without the child's result type changing (that type is in the adoption
// shape).
func TestAParentEndsPartialOnItsChildsOutcome(t *testing.T) {
	for _, c := range []struct {
		ready bool
		want  wf.Outcome
	}{{false, wf.Partial}, {true, wf.Success}} {
		cd := wf.New("po-child2")
		w := wf.NewAgent[poHand, poOut](cd)
		ok := wf.NewTerminal[chOK](cd, wf.Success).PartialWhen(func(c wf.Ctx) bool { return !wf.OutputOf(c, w).Ready })
		bad := wf.NewTerminal[chBad](cd, wf.Stopped)
		w.Spec(wf.PromptText("p"), wf.Retry[poHand](wf.Const(1), w.To(bad, "gave up")))
		to := w.To(ok, "handed off")
		w.Route(func(poOut, wf.Ctx) wf.Edge[poHand] { return to }, to)
		res := func(wf.Ctx) chRes { return chRes{} }
		wf.Yield(ok, res)
		wf.Yield(bad, res)

		d := wf.New("po-parent2")
		var sub *wf.Sub[poSub, chItem, chRes]
		sub = wf.NewSub[poSub, chItem, chRes](d, wf.Typed[chItem, chRes](cd), func(wf.Ctx) chItem { return chItem{} })
		fin := wf.NewTerminal[poFin](d, wf.Success).PartialWhen(func(c wf.Ctx) bool { return wf.ChildOutcome(c, sub) == wf.Partial })
		stop := wf.NewTerminal[poStop](d, wf.Stopped)
		sub.Exits(wf.OnExit[chOK](sub.To(fin, "child handed off")), wf.OnExit[chBad](sub.To(stop, "child stopped")))
		h := newHarness(t, d, nil)
		run := h.run(d, nil)
		answer := `{"Ready":false}`
		if c.ready {
			answer = `{"Ready":true}`
		}
		st := h.drive(run, map[string][]string{"po-child2/poHand": {answer}})
		if st.Phase != wf.RunEnded || st.Terminal != "poFin" || st.Outcome != c.want {
			t.Errorf("ready=%v: status = phase %s terminal %s outcome %s, want poFin %s", c.ready, st.Phase, st.Terminal, st.Outcome, c.want)
		}
	}
}

// an earlier revision post-cap (3): a child whose terminal made no readiness judgment
// (no PartialWhen: as every child settled before PartialWhen existed) reads
// "" through ChildOutcome, not success, so the parent falls back to what it
// knows instead of taking an unjudged success as ready.
func TestChildOutcomeIsEmptyForAnUnjudgedSettle(t *testing.T) {
	d := wf.New("po-parent3")
	sub := wf.NewSub[poSub, chItem, chRes](d, chChild(), func(wf.Ctx) chItem { return chItem{} })
	var seen wf.Outcome = "unset"
	fin := wf.NewTerminal[poFin](d, wf.Success).PartialWhen(func(c wf.Ctx) bool { seen = wf.ChildOutcome(c, sub); return false })
	stop := wf.NewTerminal[poStop](d, wf.Stopped)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")), wf.OnExit[chBad](sub.To(stop, "bad")))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"c-item/chWork": {`{"OK":true}`}}); st.Terminal != "poFin" {
		t.Fatalf("status = %+v, want ended at poFin", st)
	}
	if seen != "" {
		t.Errorf("ChildOutcome of a child ending at a plain Success terminal = %q, want \"\" (no readiness judgment)", seen)
	}
}
