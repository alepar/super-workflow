package wf_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	sfA    struct{}
	sfB    struct{}
	sfC    struct{}
	sfEnd  struct{}
	sfHalt struct{}
)

// sfRetryDef is a chain A -> B -> C -> end whose agents each declare a Retry
// with an exhausted edge, like a typical multi-agent definition.
func sfRetryDef() *wf.Def {
	d := wf.New("sf-retry")
	a := wf.NewAgent[sfA, clAnswer](d)
	b := wf.NewAgent[sfB, clAnswer](d)
	c := wf.NewAgent[sfC, clAnswer](d)
	end := wf.NewTerminal[sfEnd](d, wf.Success)
	halt := wf.NewTerminal[sfHalt](d, wf.Stopped)
	a.Spec(wf.PromptText("a"), wf.Retry(wf.Const(2), a.To(halt, "gave up")))
	b.Spec(wf.PromptText("b"), wf.Retry(wf.Const(2), b.To(halt, "gave up")))
	c.Spec(wf.PromptText("c"), wf.Retry(wf.Const(2), c.To(halt, "gave up")))
	ab := a.To(b, "next")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sfA] { return ab }, ab)
	bc := b.To(c, "next")
	b.Route(func(clAnswer, wf.Ctx) wf.Edge[sfB] { return bc }, bc)
	ce := c.To(end, "ok")
	c.Route(func(clAnswer, wf.Ctx) wf.Edge[sfC] { return ce }, ce)
	return d
}

func TestStatusFutureIgnoresRetryEdges(t *testing.T) {
	d := sfRetryDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0)
	st := h.status(run)
	var states []string
	for _, u := range st.Units {
		states = append(states, u.State+":"+u.Class)
	}
	if want := []string{"pending:sfA", "future:sfB", "future:sfC"}; !reflect.DeepEqual(states, want) {
		t.Errorf("units = %v, want %v (a Retry exhausted edge is not a branch)", states, want)
	}
	if st.Future != 2 {
		t.Errorf("Future = %d, want 2", st.Future)
	}
}

// TestExampleStatusListsFutureSteps starts the critique example at its first
// agent and wants a future line: its agents declare a Retry exhausted edge
// besides their route.
func TestExampleStatusListsFutureSteps(t *testing.T) {
	d := critique.Definition()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0) // drive to the first agent
	st := h.status(run)
	var future []string
	for _, u := range st.Units {
		if u.State == "future" {
			future = append(future, u.Name)
		}
	}
	if len(future) == 0 {
		t.Errorf("no future unit ahead of the first agent; units = %+v", st.Units)
	}
}

type sfSubRun struct{}
type sfFan struct{}
type sfMerged struct{}
type sfPartial struct{}

// sfSubDef is A -> Sub(two-step child, one exit) -> B -> end. The agents carry
// Retry like the example agents.
func sfSubDef() *wf.Def {
	d := wf.New("sf-sub")
	a := wf.NewAgent[sfA, clAnswer](d)
	sub := wf.NewSub[sfSubRun, chItem, chRes](d, twoStepChild(), func(wf.Ctx) chItem { return chItem{} })
	b := wf.NewAgent[sfB, clAnswer](d)
	end := wf.NewTerminal[sfEnd](d, wf.Success)
	halt := wf.NewTerminal[sfHalt](d, wf.Stopped)
	a.Spec(wf.PromptText("a"), wf.Retry(wf.Const(2), a.To(halt, "gave up")))
	b.Spec(wf.PromptText("b"), wf.Retry(wf.Const(2), b.To(halt, "gave up")))
	as := a.To(sub, "next")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sfA] { return as }, as)
	sub.Exits(wf.OnExit[chOK](sub.To(b, "ok")))
	be := b.To(end, "ok")
	b.Route(func(clAnswer, wf.Ctx) wf.Edge[sfB] { return be }, be)
	return d
}

func unitKeys(st wf.RunStatus) []string {
	var out []string
	for _, u := range st.Units {
		out = append(out, u.State+":"+u.Class)
	}
	return out
}

func TestStatusFutureDescendsIntoSub(t *testing.T) {
	d := sfSubDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0)
	st := h.status(run)
	want := []string{"pending:sfA", "future:two-step/chWork", "future:two-step/chStep", "future:sfB"}
	if got := unitKeys(st); !reflect.DeepEqual(got, want) {
		t.Errorf("units = %v, want %v", got, want)
	}
	if st.Future != 3 {
		t.Errorf("Future = %d, want 3", st.Future)
	}
}

func TestStatusFutureContinuesAfterChild(t *testing.T) {
	d := sfSubDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions = %v, want the one at A", ins)
	}
	h.submit(run, ins[0], `{}`)
	ins = h.next(run)
	if len(ins) != 1 || ins[0].State != "two-step/chWork" {
		t.Fatalf("instructions = %v, want the child's first state", ins)
	}
	st := h.status(run)
	want := []string{"running:two-step/chWork", "future:two-step/chStep", "future:sfB"}
	var got []string // A's settled unit is listed too; the rows ahead are what is under test
	for _, g := range unitKeys(st) {
		if !strings.HasPrefix(g, "done:") {
			got = append(got, g)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("units = %v, want %v", got, want)
	}
}

func TestStatusFutureStopsAtMap(t *testing.T) {
	d := wf.New("sf-map")
	a := wf.NewAgent[sfA, clAnswer](d)
	m := wf.NewMap[sfFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return string(rune('0' + i.N)) }, chChild())
	merged := wf.NewTerminal[sfMerged](d, wf.Success)
	partial := wf.NewTerminal[sfPartial](d, wf.Stopped)
	halt := wf.NewTerminal[sfHalt](d, wf.Stopped)
	a.Spec(wf.PromptText("a"), wf.Retry(wf.Const(2), a.To(halt, "gave up")))
	am := a.To(m, "next")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sfA] { return am }, am)
	m.Parallel(2).OnSiblingFailure(wf.CancelSiblings).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0)
	st := h.status(run)
	if want := []string{"pending:sfA"}; !reflect.DeepEqual(unitKeys(st), want) {
		t.Errorf("units = %v, want %v (nothing listed past a Map)", unitKeys(st), want)
	}
	if st.Future != 1 {
		t.Errorf("Future = %d, want 1 (the Map child's agent is still counted)", st.Future)
	}
}
