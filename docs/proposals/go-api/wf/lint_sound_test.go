package wf_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	sdStart  struct{}
	sdRecord struct{}
	sdAsk    struct{}
	sdDone   struct{}
	sdStop   struct{}
	sdLoopA  struct{}
	sdLoopB  struct{}
	sdChild  struct{}
	sdCDone  struct{}
	sdCStop  struct{}
)

func soundLines(d *wf.Def) []string {
	var out []string
	for _, p := range d.LintSound() {
		out = append(out, p.Line())
	}
	return out
}

func wantSound(t *testing.T, d *wf.Def, want ...string) {
	t.Helper()
	got := soundLines(d)
	if len(got) != len(want) {
		t.Fatalf("lint:\n%s\nwant %d lines:\n%s", strings.Join(got, "\n"), len(want), strings.Join(want, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Errorf("line %d = %q, want prefix %q", i, got[i], w)
		}
	}
}

// A1 (hermes-ds rung 3): a route straight to a stop is unmarked; the stop's
// decision is reported at the edge that commits to it, not inside the stop
// path (a record state on the way is not the decision).
func TestLintSoundStopUnmarked(t *testing.T) {
	d := wf.New("sd-stop")
	start := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	record := wf.NewCompute[sdRecord, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	done := wf.NewTerminal[sdDone](d, wf.Success)
	stop := wf.NewTerminal[sdStop](d, wf.Stopped)
	ok, refused := start.To(done, "sound"), start.To(record, "refused")
	start.Route(func(b bool, _ wf.Ctx) wf.Edge[sdStart] {
		if b {
			return ok
		}
		return refused
	}, ok, refused)
	toStop := record.To(stop, "recorded")
	record.Route(func(bool, wf.Ctx) wf.Edge[sdRecord] { return toStop }, toStop)
	wantSound(t, d, `lint: stop-unmarked sd-stop/sdStart edge "refused" -> sdRecord · `)

	// The same decision marked Ends passes, and the view carries the reason.
	d2 := wf.New("sd-stop-marked")
	start2 := wf.NewCompute[sdStart, bool](d2, func(wf.Ctx) (bool, error) { return true, nil })
	done2 := wf.NewTerminal[sdDone](d2, wf.Success)
	stop2 := wf.NewTerminal[sdStop](d2, wf.Stopped)
	ok2, refused2 := start2.To(done2, "sound"), start2.To(stop2, "refused").Ends("the tree is unsound and nothing can repair it")
	start2.Route(func(b bool, _ wf.Ctx) wf.Edge[sdStart] {
		if b {
			return ok2
		}
		return refused2
	}, ok2, refused2)
	wantSound(t, d2)
	v := wf.Inspect(d2).States[0]
	if i := slices.IndexFunc(v.Edges, func(e wf.EdgeView) bool { return e.Label == "refused" }); i < 0 || v.Edges[i].Ends == "" {
		t.Errorf("edges %+v: want the refused edge's Ends reason in the view", v.Edges)
	}
}

// A1: an exhausted retry, a visit cap, a human gate and a child's own stop
// forwarded by its parent are reasons a human accepts.
func TestLintSoundAcceptedStops(t *testing.T) {
	d := wf.New("sd-accepted")
	start := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	ans := wf.NewEnum(d, "sd-answer", "go", "stop")
	ask := wf.NewGate[sdAsk, string](d, "go on?", ans, wf.Reserved)
	done := wf.NewTerminal[sdDone](d, wf.Success)
	stop := wf.NewTerminal[sdStop](d, wf.Stopped)

	child := wf.New("sd-child")
	cw := wf.NewCompute[sdChild, bool](child, func(wf.Ctx) (bool, error) { return true, nil })
	cdone := wf.NewTerminal[sdCDone](child, wf.Success)
	cstop := wf.NewTerminal[sdCStop](child, wf.Stopped)
	cok, cbad := cw.To(cdone, "ok"), cw.To(cstop, "bad").Ends("the item cannot be done")
	cw.Route(func(b bool, _ wf.Ctx) wf.Edge[sdChild] {
		if b {
			return cok
		}
		return cbad
	}, cok, cbad)
	sub := wf.NewSub[sdRecord](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
	sub.Exits(wf.OnExit[sdCDone](sub.To(ask, "child done")), wf.OnExit[sdCStop](sub.To(stop, "child stopped")))

	toSub := start.To(sub, "go")
	start.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return toSub }, toSub)
	start.Retry(wf.Retry[sdStart](wf.Const(2), start.To(stop, "start failed twice")))
	wf.MaxVisits(wf.Const(3), start.To(stop, "visited three times"))
	ask.Answers(wf.On("go", ask.To(done, "went on").Ends(wf.Success, "the person approved completion")), wf.On("stop", ask.To(stop, "the human stopped it").Ends(wf.Stopped, "the person declined completion")))
	wantSound(t, d)
}

// A3: a cycle with no visit cap and no measure is reported; MaxVisits or
// Measure on one of its states bounds it. A2: a loop of computes with no
// effect can change nothing it reads.
func TestLintSoundCycles(t *testing.T) {
	build := func(name string, bound func(a *wf.Compute[sdLoopA, bool], back wf.Edge[sdLoopB], done wf.State), effect bool) *wf.Def {
		d := wf.New(name)
		a := wf.NewCompute[sdLoopA, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		b := wf.NewCompute[sdLoopB, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[sdDone](d, wf.Success)
		round := d.Counter("sd-round")
		on, out := a.To(b, "again"), a.To(done, "done")
		a.Route(func(x bool, _ wf.Ctx) wf.Edge[sdLoopA] {
			if x {
				return out
			}
			return on
		}, on, out)
		back := b.To(a, "back")
		if effect {
			back = back.With(round.Inc())
		}
		b.Route(func(bool, wf.Ctx) wf.Edge[sdLoopB] { return back }, back)
		if bound != nil {
			bound(a, back, done)
		}
		return d
	}
	wantSound(t, build("sd-loop", nil, true), `lint: cycle-no-measure sd-loop/sdLoopA · the cycle through sdLoopA, sdLoopB`)
	wantSound(t, build("sd-loop-capped", func(a *wf.Compute[sdLoopA, bool], _ wf.Edge[sdLoopB], done wf.State) {
		wf.MaxVisits(wf.Const(3), a.To(done, "over cap: done"))
	}, true))
	wantSound(t, build("sd-loop-measured", func(a *wf.Compute[sdLoopA, bool], _ wf.Edge[sdLoopB], _ wf.State) {
		wf.Measure(a, "each child owns a strict subset of its parent's parts")
	}, true))
	wantSound(t, build("sd-loop-dead", func(a *wf.Compute[sdLoopA, bool], _ wf.Edge[sdLoopB], _ wf.State) {
		wf.Measure(a, "rounds")
	}, false), `lint: refusal-no-repair sd-loop-dead/sdLoopA · the loop through sdLoopA, sdLoopB`)
}

// An Ends or a Measure with no reason is a build problem.
func TestLintSoundEmptyReasons(t *testing.T) {
	d := wf.New("sd-empty")
	a := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	stop := wf.NewTerminal[sdStop](d, wf.Stopped)
	e := a.To(stop, "x").Ends(" ")
	a.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return e }, e)
	wf.Measure(a, "")
	var rules []wf.RuleID
	for _, p := range d.Lint() {
		rules = append(rules, p.Rule)
	}
	if !slices.Contains(rules, wf.RuleEndsReasonEmpty) || !slices.Contains(rules, wf.RuleMeasureNoReason) {
		t.Errorf("lint rules %v: want %s and %s", rules, wf.RuleEndsReasonEmpty, wf.RuleMeasureNoReason)
	}
}

// A1: a child's non-success terminal is a stop only when its parent makes it
// one. Mapped by a Sub to a state that goes on, it is a typed result (a
// judge's "refuted"); a Sub exit into the parent's stop leaves the decision
// in the child, where it is reported.
func TestLintSoundChildStopsFollowTheParent(t *testing.T) {
	build := func(name string, toStop bool) *wf.Def {
		child := wf.New(name + "-child")
		cw := wf.NewCompute[sdChild, bool](child, func(wf.Ctx) (bool, error) { return true, nil })
		cdone := wf.NewTerminal[sdCDone](child, wf.Success)
		cstop := wf.NewTerminal[sdCStop](child, wf.Stopped)
		cok, cbad := cw.To(cdone, "ok"), cw.To(cstop, "refuted")
		cw.Route(func(b bool, _ wf.Ctx) wf.Edge[sdChild] {
			if b {
				return cok
			}
			return cbad
		}, cok, cbad)
		d := wf.New(name)
		start := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[sdDone](d, wf.Success)
		stop := wf.NewTerminal[sdStop](d, wf.Stopped)
		sub := wf.NewSub[sdRecord](d, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		var onStop wf.Edge[sdRecord]
		if toStop {
			onStop = sub.To(stop, "child stopped")
		} else {
			onStop = sub.To(done, "child refuted: a result")
		}
		sub.Exits(wf.OnExit[sdCDone](sub.To(done, "child done")), wf.OnExit[sdCStop](onStop))
		go1 := start.To(sub, "go")
		start.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return go1 }, go1)
		return d
	}
	wantSound(t, build("sd-result", false))
	wantSound(t, build("sd-forward", true), `lint: stop-unmarked sd-forward/sd-forward-child/sdChild edge "refuted" -> sdCStop · `)
}

type sdLoopC struct{}

// Review probes: a cap on one cycle of an SCC does not bound another cycle
// that skips it; a route that is also an OnClass target is still a decision.
func TestLintSoundReviewHoles(t *testing.T) {
	d := wf.New("sd-two-cycles")
	a := wf.NewCompute[sdLoopA, int](d, func(wf.Ctx) (int, error) { return 0, nil })
	b := wf.NewCompute[sdLoopB, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	cc := wf.NewCompute[sdLoopC, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	done := wf.NewTerminal[sdDone](d, wf.Success)
	round := d.Counter("sd-round")
	toB, toC, out := a.To(b, "b"), a.To(cc, "c"), a.To(done, "done")
	a.Route(func(x int, _ wf.Ctx) wf.Edge[sdLoopA] {
		switch x {
		case 1:
			return toB
		case 2:
			return toC
		}
		return out
	}, toB, toC, out)
	bBack, cBack := b.To(a, "back").With(round.Inc()), cc.To(a, "back").With(round.Inc())
	b.Route(func(bool, wf.Ctx) wf.Edge[sdLoopB] { return bBack }, bBack)
	cc.Route(func(bool, wf.Ctx) wf.Edge[sdLoopC] { return cBack }, cBack)
	wf.MaxVisits(wf.Const(3), b.To(done, "over"))
	wantSound(t, d, `lint: cycle-no-measure sd-two-cycles/sdLoopA · the cycle through sdLoopA, sdLoopC`)

	d2 := wf.New("sd-shared-edge")
	x := wf.NewCompute[sdStart, bool](d2, func(wf.Ctx) (bool, error) { return true, nil })
	done2 := wf.NewTerminal[sdDone](d2, wf.Success)
	stop2 := wf.NewTerminal[sdStop](d2, wf.Stopped)
	ok, st := x.To(done2, "ok"), x.To(stop2, "silent stop")
	x.Route(func(b bool, _ wf.Ctx) wf.Edge[sdStart] {
		if b {
			return ok
		}
		return st
	}, ok, st)
	x.Retry(wf.Retry[sdStart](wf.Const(1), x.To(stop2, "failed")).OnClass(wf.Blocked, st))
	wantSound(t, d2, `lint: stop-unmarked sd-shared-edge/sdStart edge "silent stop" -> sdStop · `)

	d3 := wf.New("sd-doomed")
	y := wf.NewCompute[sdStart, bool](d3, func(wf.Ctx) (bool, error) { return true, nil })
	wf.NewTerminal[sdDone](d3, wf.Success)
	stop3 := wf.NewTerminal[sdStop](d3, wf.Stopped)
	e3 := y.To(stop3, "always").Ends("probe")
	y.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return e3 }, e3)
	wantSound(t, d3, `lint: stop-unmarked sd-doomed/sdStart · every way through this definition ends at a stop`)
}

// A policy-answerable gate is no human: its stop needs Ends.
func TestLintSoundPolicyGateIsNotHuman(t *testing.T) {
	d := wf.New("sd-policy-gate")
	ans := wf.NewEnum(d, "sd-policy-answer", "go", "stop")
	ask := wf.NewGate[sdAsk, string](d, "go on?", ans, wf.PolicyAnswerable)
	done := wf.NewTerminal[sdDone](d, wf.Success)
	stop := wf.NewTerminal[sdStop](d, wf.Stopped)
	ask.Answers(wf.On("go", ask.To(done, "went on").Ends(wf.Success, "the person approved completion")), wf.On("stop", ask.To(stop, "stopped")))
	wantSound(t, d, `lint: stop-unmarked sd-policy-gate/sdAsk edge "stopped" -> sdStop · `, `lint: gate-ends-missing sd-policy-gate/sdAsk`)
}

// Cross-review: a visit cap whose over edge leads back into the cycle
// bounds nothing; once the cap is spent the state only takes its over edge.
func TestLintSoundCapWhoseOverEdgeStaysInTheCycle(t *testing.T) {
	build := func(name string, overOut bool) *wf.Def {
		d := wf.New(name)
		a := wf.NewCompute[sdLoopA, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		b := wf.NewCompute[sdLoopB, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[sdDone](d, wf.Success)
		round := d.Counter("sd-round")
		on, out := a.To(b, "again"), a.To(done, "done")
		a.Route(func(x bool, _ wf.Ctx) wf.Edge[sdLoopA] {
			if x {
				return out
			}
			return on
		}, on, out)
		back := b.To(a, "back").With(round.Inc())
		b.Route(func(bool, wf.Ctx) wf.Edge[sdLoopB] { return back }, back)
		if overOut {
			wf.MaxVisits(wf.Const(1), a.To(done, "over cap: done"))
		} else {
			wf.MaxVisits(wf.Const(1), a.To(b, "over cap"))
		}
		return d
	}
	wantSound(t, build("sd-over-in", false), `lint: cycle-no-measure sd-over-in/sdLoopA · the cycle through sdLoopA, sdLoopB`)
	wantSound(t, build("sd-over-out", true))
}

// Cross-review repros. (a) Compute A goes to command B or to success, and B
// back to A; A's cap redirects every arrival to B for ever. (b) An effect on
// one use of a self-edge does not make the bare use write.
func TestLintSoundCrossReviewRepros(t *testing.T) {
	d := wf.New("sd-cr-a")
	a := wf.NewCompute[sdLoopA, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	b := wf.NewCommand[sdLoopB](d, "true")
	done := wf.NewTerminal[sdDone](d, wf.Success)
	toB, out := a.To(b, "b"), a.To(done, "done")
	a.Route(func(x bool, _ wf.Ctx) wf.Edge[sdLoopA] {
		if x {
			return out
		}
		return toB
	}, toB, out)
	back := b.To(a, "back")
	b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[sdLoopB] { return back }, back)
	wf.MaxVisits(wf.Const(1), a.To(b, "over"))
	wantSound(t, d, `lint: cycle-no-measure sd-cr-a/sdLoopA · the cycle through sdLoopA, sdLoopB`)

	d2 := wf.New("sd-cr-b")
	x := wf.NewCompute[sdStart, int](d2, func(wf.Ctx) (int, error) { return 0, nil })
	done2 := wf.NewTerminal[sdDone](d2, wf.Success)
	round := d2.Counter("sd-round")
	self, ok := x.To(x, "again"), x.To(done2, "done")
	selfInc := self.With(round.Inc())
	x.Route(func(n int, _ wf.Ctx) wf.Edge[sdStart] {
		switch n {
		case 0:
			return ok
		case 1:
			return selfInc
		}
		return self
	}, self, selfInc, ok)
	wf.Measure(x, "probe: A3 is not under test here")
	wantSound(t, d2, `lint: refusal-no-repair sd-cr-b/sdStart · the loop through sdStart`)
}

// E2 (hermes-ds rung 3c): a refused tracker write on an edge took the
// definition's default Blocked route to the stop. That route is a stop the
// lint reports, while an unavailable read's route stays a failure route.
func TestLintSoundRefusedTrackerWriteIsNotAFailureRoute(t *testing.T) {
	d := wf.New("sd-tracker")
	w := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	done := wf.NewTerminal[sdDone](d, wf.Success)
	stop := wf.NewTerminal[sdStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	d.OnFailure(wf.Unavailable, stop)
	wire := w.To(done, "wired").With(bd.DepAdd(wf.KeyOf("dep", "a", "b"), "a", "b", "r"))
	w.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return wire }, wire)
	wantSound(t, d, `lint: stop-unmarked sd-tracker/sdStart edge "default blocked route" -> sdStop · `)

	// The same write made through a shell effect, as a command and inside a
	// bash -c script; a read through the shell is not a write.
	for name, run := range map[string]shell.Invocation{
		"sd-tracker-shell":  shell.Run("bd", "dep", "add", "a", "b"),
		"sd-tracker-script": shell.Run("bash", "-c", "set -e\nbd update \"$1\" -t epic", "promote", "a"),
		"sd-tracker-read":   shell.Run("bash", "-c", "bd list --json", "read"),
	} {
		d := wf.New(name)
		w := wf.NewCompute[sdStart, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
		done := wf.NewTerminal[sdDone](d, wf.Success)
		stop := wf.NewTerminal[sdStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		e := w.To(done, "wired").With(shell.Effect(wf.KeyOf("w", name), run))
		w.Route(func(bool, wf.Ctx) wf.Edge[sdStart] { return e }, e)
		if name == "sd-tracker-read" {
			wantSound(t, d)
			continue
		}
		wantSound(t, d, `lint: stop-unmarked `+name+`/sdStart edge "default blocked route" -> sdStop · `)
	}
}
