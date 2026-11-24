package wf_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCommandRouteExit(t *testing.T) {
	for _, exit := range []int{0, 1, 7, 127, -1} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			d := wf.New("exit-route")
			cmd := wf.NewCommand[eA](d, "test")
			done, bad := twoTerminals(d)
			green := cmd.To(done, "zero").With(shellEffect(""))
			red := cmd.To(bad, "nonzero")
			cmd.RouteExit(green, red)
			w := newWorld()
			w.runs = []wf.RunResult{{Exit: exit}, {Exit: 0}}
			h := newHarness(t, d, w)
			run := h.run(d, nil)
			h.next(run)
			want, effects := "eBad", 1
			if exit == 0 {
				want, effects = "eDone", 2
			}
			statusOf(t, h.status(run), wf.RunEnded, want)
			if len(w.applied) != effects {
				t.Fatalf("applied effects = %d, want %d", len(w.applied), effects)
			}
		})
	}
}

func TestOnFailuresPreservesDefaultsAndOverrides(t *testing.T) {
	build := func(multi bool) *wf.Def {
		d, a := simple("multi-default", wf.PromptText("p"))
		stop := wf.NewTerminal[eOther](d, wf.Stopped)
		own := wf.NewTerminal[eRecovered](d, wf.Stopped)
		a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(2), a.To(stop, "exhausted")).OnClass(wf.Blocked, a.To(own, "own blocked")))
		d.OnFailure(wf.Unavailable, own)
		if multi {
			if d.OnFailures(stop, wf.Blocked, wf.Unavailable, wf.Uncertain, wf.Blocked) != d {
				t.Fatal("OnFailures did not return its definition")
			}
		} else {
			for _, class := range []wf.OutcomeClass{wf.Blocked, wf.Unavailable, wf.Uncertain, wf.Blocked} {
				d.OnFailure(class, stop)
			}
		}
		return d
	}
	got, want := build(true), build(false)
	if got.Hash() != want.Hash() || got.Render() != want.Render() {
		t.Fatal("multi-class defaults differ from ordered single-class declarations")
	}
	for _, state := range wf.Inspect(got).States {
		if state.Name != "eA" {
			continue
		}
		defaults := 0
		for _, edge := range state.Edges {
			if strings.HasPrefix(edge.Label, "default ") {
				defaults++
				if edge.Label == "default blocked route" || edge.To != "eOther" {
					t.Fatalf("wrong default: %+v", edge)
				}
			}
		}
		if defaults != 2 {
			t.Fatalf("default edges = %d, want unavailable and uncertain", defaults)
		}
	}
}

func TestComputeAndCommandThenRunEdgeEffects(t *testing.T) {
	d := wf.New("then-chain")
	compute := wf.NewCompute[eA, int](d, func(wf.Ctx) (int, error) { return 42, nil })
	cmd := wf.NewCommand[eB](d, "command")
	done := wf.NewTerminal[eDone](d, wf.Success)
	compute.Then(compute.To(cmd, "computed").With(shellEffect("")))
	cmd.Then(cmd.To(done, "ran").With(shellEffect("")))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 0}, {Exit: 7}, {Exit: 0}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.next(run)
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
	if len(w.applied) != 3 {
		t.Fatalf("applied effects = %d, want both edges and the command", len(w.applied))
	}
}

func TestMapPromptInputExplainsJoinedCompute(t *testing.T) {
	d := wf.New("map-prompt")
	child := wf.New("item")
	end := wf.NewTerminal[eDone](child, wf.Success)
	wf.Yield(end, func(wf.Ctx) string { return "done" })
	m := wf.NewMap[eA, string, string, string](d,
		wf.FromObservation(wf.Observe[[]string]("items", "list", nil)),
		func(s string) string { return s }, wf.Typed[string, string](child))
	a := wf.NewAgent[eB, eOut](d)
	done, bad := twoTerminals(d)
	m.Join(wf.AllOK(), m.To(a, "joined"), m.To(bad, "failed"))
	a.Spec(wf.PromptText("Review {{results}}", wf.As("results", wf.Out(m))),
		wf.Retry(wf.Const(1), a.To(bad, "unusable")))
	a.Then(a.To(done, "reviewed"))
	for _, p := range d.Lint() {
		if p.Rule == wf.RuleInputUnresolved && strings.Contains(p.String(), "wf.Joined") && strings.Contains(p.String(), "Compute") {
			return
		}
	}
	t.Fatalf("Map input diagnostic lacks the joined-result fix: %v", d.Lint())
}
