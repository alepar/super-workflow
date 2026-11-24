package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// endedAfterFailedEffect runs a def to its Stopped terminal over an Optional
// edge whose merge effect fails.
func endedAfterFailedEffect(t *testing.T) (*harness, wf.RunID) {
	t.Helper()
	d := wf.New("ended-effects")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")))
	stop := a.To(bad, "merge refused").With(mergeEffect(wf.KeyOf("merge", "feat"))).Optional()
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return stop }, stop, a.To(done, "unused"))
	w := newWorld()
	w.failOp["git.merge"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	return h, run
}

func TestEndedRunHandsOutOneEndedInstructionOnEveryForm(t *testing.T) {
	h, run := endedAfterFailedEffect(t)
	forms := map[string][]wf.Instruction{}
	var err error
	if forms["next"], err = h.e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if forms["next <n>"], err = h.e.HandOut(run, 3); err != nil {
		t.Fatal(err)
	}
	for form, ins := range forms {
		if len(ins) != 1 || ins[0].Kind != wf.InstructionEnded {
			t.Fatalf("%s = %+v, want exactly one ended instruction", form, ins)
		}
		in := ins[0]
		if in.Terminal != "eBad" || in.Outcome != wf.Stopped || !strings.Contains(in.Cause, "git.merge failed") {
			t.Errorf("%s ended instruction = %+v, want terminal eBad, outcome stopped, cause naming the failed merge", form, in)
		}
	}
	st := h.status(run)
	if st.Cause != forms["next"][0].Cause || st.Cause == "" {
		t.Errorf("status cause = %q, next cause = %q: both must read the one Run cause", st.Cause, forms["next"][0].Cause)
	}
	if view := wf.RenderStatus(st); !strings.Contains(view, "cause: "+st.Cause) {
		t.Errorf("status view lacks the cause line:\n%s", view)
	}
}

func TestRunningRunHasNoCause(t *testing.T) {
	d, _ := simple("run-cause-live", wf.PromptText("p"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("instructions = %+v, want one agent instruction", ins)
	}
	if st := h.status(run); st.Cause != "" || st.Phase == wf.RunEnded {
		t.Errorf("status = phase %s cause %q, want a live run with no cause", st.Phase, st.Cause)
	}
}

type (
	edSubRun struct{}
	edFin    struct{}
	edStop   struct{}
)

// A failure inside a Sub child surfaces as the run's cause, not the parent's
// bare edge label.
func TestEndedCauseNamesFailedChild(t *testing.T) {
	d := wf.New("ended-sub")
	sub := wf.NewSub[edSubRun, chItem, chRes](d, chChild(), func(wf.Ctx) chItem { return chItem{} })
	fin := wf.NewTerminal[edFin](d, wf.Success)
	stop := wf.NewTerminal[edStop](d, wf.Stopped)
	sub.Exits(wf.OnExit[chOK](sub.To(fin, "ok")), wf.OnExit[chBad](sub.To(stop, "child said bad")))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	st := h.drive(run, map[string][]string{"c-item/chWork": {`{"OK":false}`}})
	if st.Phase != wf.RunEnded || st.Terminal != "edStop" {
		t.Fatalf("status = %+v, want ended at edStop", st)
	}
	if !strings.Contains(st.Cause, "chBad") || !strings.Contains(st.Cause, "c-item") || st.Cause == "child said bad" {
		t.Errorf("cause = %q, want the failed child's settlement, not the parent's edge label", st.Cause)
	}
}

type (
	edAsk  struct{}
	edAns  string
	edDone struct{}
)

// abortGate adds a reserved gate whose "abort" answer stops the run and whose
// "go" answer ends it in success.
func abortGate(d *wf.Def) *wf.Gate[edAsk, edAns] {
	ans := wf.NewEnum(d, "ed-ans", edAns("go"), edAns("abort"))
	g := wf.NewGate[edAsk, edAns](d, "Go on?", ans, wf.Reserved)
	done := wf.NewTerminal[edDone](d, wf.Success)
	stop := wf.NewTerminal[edStop](d, wf.Stopped)
	g.Answers(wf.On(edAns("go"), g.To(done, "went on")), wf.On(edAns("abort"), g.To(stop, "aborted at the gate")))
	return g
}

// driveToGate answers agent instructions from answers until the run's next
// instruction is a human gate, then answers that gate with value.
func driveToGate(h *harness, run wf.RunID, answers map[string][]string, gate, value string) {
	h.t.Helper()
	for i := 0; i < 100; i++ {
		ins := h.next(run)
		if len(ins) == 1 && ins[0].Kind == wf.InstructionHumanGate {
			if err := h.e.Answer(run, gate, value); err != nil {
				h.t.Fatal(err)
			}
			return
		}
		if len(ins) == 0 || ins[0].Kind != wf.InstructionAgent {
			h.t.Fatalf("instructions = %+v, want agent work or the gate; status %+v; journal %v", ins, h.status(run), h.records(run))
		}
		for _, in := range ins {
			q := answers[in.State]
			if len(q) == 0 {
				h.t.Fatalf("no answer left for instruction at %s", in.State)
			}
			answers[in.State] = q[1:]
			h.submit(run, in, q[0])
		}
	}
	h.t.Fatal("never reached the gate")
}

// A child that failed and was replaced by a succeeding one is off the path of
// a later stop: the cause is the abort edge, not the first child's failure.
func TestEndedCauseSkipsReplacedChildFailure(t *testing.T) {
	d := wf.New("ended-respawn")
	sub := wf.NewSub[edSubRun, chItem, chRes](d, chChild(), func(wf.Ctx) chItem { return chItem{} })
	g := abortGate(d)
	sub.Exits(wf.OnExit[chOK](sub.To(g, "child ok")), wf.OnExit[chBad](sub.To(sub, "respawn")))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	driveToGate(h, run, map[string][]string{"c-item/chWork": {`{"OK":false}`, `{"OK":true}`}}, "edAsk", "abort")
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Terminal != "edStop" {
		t.Fatalf("status = %+v, want ended at edStop", st)
	}
	if st.Cause != "edAsk: aborted at the gate" {
		t.Errorf("cause = %q, want the abort edge label", st.Cause)
	}
}

// A root effect error on an (Optional) edge the run took and moved past
// (through another state) is off the path of a later stop at a gate.
func TestEndedCauseSkipsEffectErrorBeforeLastEntry(t *testing.T) {
	d := wf.New("ended-effect-then-gate")
	a := wf.NewAgent[eA, eOut](d)
	b := wf.NewAgent[eB, eOut](d)
	g := abortGate(d)
	bad := wf.NewTerminal[eBad](d, wf.Stopped)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(1), a.To(bad, "gave up")))
	merge := a.To(b, "merged").With(mergeEffect(wf.KeyOf("merge", "feat"))).Optional()
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return merge }, merge)
	b.Spec(wf.PromptText("p"), wf.Retry[eB](wf.Const(1), b.To(bad, "gave up")))
	ask := b.To(g, "ask")
	b.Route(func(eOut, wf.Ctx) wf.Edge[eB] { return ask }, ask)
	w := newWorld()
	w.failOp["git.merge"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	driveToGate(h, run, map[string][]string{"eA": {`{}`}, "eB": {`{}`}}, "edAsk", "abort")
	if r := h.kind(run, wf.JournalEffectResult); len(r) != 1 || !strings.Contains(string(r[0].Data), "apply failed") {
		t.Fatalf("effect results = %v, want the one failed merge", r)
	}
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Terminal != "edStop" {
		t.Fatalf("status = %+v, want ended at edStop", st)
	}
	if st.Cause != "edAsk: aborted at the gate" {
		t.Errorf("cause = %q, want the abort edge label, not the old merge error", st.Cause)
	}
}

// A run that stops after a failed observation (a Map source that is down)
// names the observation as its cause.
func TestEndedCauseNamesFailedObservation(t *testing.T) {
	d := srcDef("ended-obs")
	w := newWorld()
	w.down["kv.items"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.next(run)
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Outcome == wf.Success {
		t.Fatalf("status = %+v, want a run that ended without success", st)
	}
	if !strings.Contains(st.Cause, "kv.items unavailable") || !strings.Contains(st.Cause, "down") {
		t.Errorf("cause = %q, want the unavailable kv.items read", st.Cause)
	}
}

// A failed effect on the edge into a ChecksEdge state that then routes to a
// Stopped terminal is the cause: transit journals the edge's effects before
// its transition into the check state, so they still belong to the final
// edge's visit.
func TestEndedCauseNamesEffectErrorIntoCheckState(t *testing.T) {
	d := edgeDef(true, mergeEffect(wf.KeyOf("merge", "feat")))
	w := newWorld()
	w.failOp["git.merge"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	st := h.status(run)
	if st.Phase != wf.RunEnded || st.Terminal != "eBad" {
		t.Fatalf("status = %+v, want ended at eBad", st)
	}
	if want := "eA: git.merge failed: apply failed"; st.Cause != want {
		t.Errorf("cause = %q, want %q", st.Cause, want)
	}
}
