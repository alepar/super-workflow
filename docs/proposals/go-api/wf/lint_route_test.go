package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	rtA     struct{}
	rtB     struct{}
	rtC     struct{}
	rtSub   struct{}
	rtDone  struct{}
	rtStop  struct{}
	rtChild struct{}
	rtCDone struct{}
	rtCStop struct{}
)

// workflow-checker layer 1: every finding carries the shortest path from the
// entry to its state, through the parent state that runs a child.
func TestLintFindingsCarryTheShortestPath(t *testing.T) {
	child := wf.New("rt-child")
	cw := wf.NewCompute[rtChild, bool](child, func(wf.Ctx) (bool, error) { return true, nil })
	cdone := wf.NewTerminal[rtCDone](child, wf.Success)
	cstop := wf.NewTerminal[rtCStop](child, wf.Stopped)
	cok, cbad := cw.To(cdone, "ok"), cw.To(cstop, "child gave up")
	cw.Route(func(b bool, _ wf.Ctx) wf.Edge[rtChild] {
		if b {
			return cok
		}
		return cbad
	}, cok, cbad)

	d := wf.New("rt")
	a := wf.NewCompute[rtA, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	b := wf.NewCompute[rtB, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	c := wf.NewCompute[rtC, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	sub := wf.NewSub[rtSub](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
	done := wf.NewTerminal[rtDone](d, wf.Success)
	stop := wf.NewTerminal[rtStop](d, wf.Stopped)
	toB := a.To(b, "first")
	a.Route(func(bool, wf.Ctx) wf.Edge[rtA] { return toB }, toB)
	toC, toSub := b.To(c, "second"), b.To(sub, "into the child")
	b.Route(func(x bool, _ wf.Ctx) wf.Edge[rtB] {
		if x {
			return toC
		}
		return toSub
	}, toC, toSub)
	ok, bad := c.To(done, "ok"), c.To(stop, "gave up")
	c.Route(func(x bool, _ wf.Ctx) wf.Edge[rtC] {
		if x {
			return ok
		}
		return bad
	}, ok, bad)
	sub.Exits(wf.OnExit[rtCDone](sub.To(done, "child done")), wf.OnExit[rtCStop](sub.To(stop, "child stopped")))

	var lines []string
	for _, p := range d.LintSound() {
		lines = append(lines, p.Line())
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		`lint: stop-unmarked rt/rtC edge "gave up" -> rtStop · a stop reached by no human gate, exhausted budget or failure route: mark it Ends(reason) or route it to a repair · path: rtA -[first]-> rtB -[second]-> rtC`,
		`· path: rtA -[first]-> rtB -[into the child]-> rtSub > rtChild`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lint lacks %q:\n%s", want, got)
		}
	}
}
