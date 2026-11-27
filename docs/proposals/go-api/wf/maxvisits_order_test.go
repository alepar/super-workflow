package wf_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	MoWork   struct{}
	MoDone   struct{}
	MoCapped struct{}
)

// orderDef is a command state that loops on itself: exit 1 takes "fix", which
// increments fixes; exit 2 takes "round", which increments round and so zeroes
// fixes when fixes is scoped to it; exit 0 ends. MaxVisits allows one visit
// plus one per fix, so each fix pays for the visit it causes.
func orderDef(scoped bool) *wf.Def {
	d := wf.New("visit-order").Version(1)
	round := d.Counter("round")
	var fixes wf.Counter
	if scoped {
		fixes = d.Counter("fixes", wf.ScopedTo(round))
	} else {
		fixes = d.Counter("fixes")
	}
	w := wf.NewCommand[MoWork](d, "work")
	done := wf.NewTerminal[MoDone](d, wf.Success)
	capped := wf.NewTerminal[MoCapped](d, wf.Stopped)
	fix := w.To(w, "fix").With(fixes.Inc())
	again := w.To(w, "round").With(round.Inc())
	ok := w.To(done, "ok")
	w.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[MoWork] {
		switch r.Exit {
		case 1:
			return fix
		case 2:
			return again
		}
		return ok
	}, fix, again, ok)
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(fixes)), w.To(capped, "visit cap"))
	return d
}

func runOrder(t *testing.T, d *wf.Def, exits ...int) (wf.RunStatus, *world) {
	t.Helper()
	w := newWorld()
	for _, e := range exits {
		w.runs = append(w.runs, wf.RunResult{Exit: e})
	}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, nil)
	return h.status(run), w
}

// sw-h8ge: a budget reading a counter is evaluated after the Incs of the
// transition that arrives, so the fix edge's own Inc allows its visit.
func TestMaxVisitsSeesTheArrivingTransitionsInc(t *testing.T) {
	st, w := runOrder(t, orderDef(false), 1, 1, 0)
	statusOf(t, st, wf.RunEnded, "MoDone")
	if len(w.runs) != 0 {
		t.Fatalf("%d scripted runs left, want all three visits run", len(w.runs))
	}
}

// The scope reset that the arriving transition's Inc causes applies before
// the budget is read, as it does in the counters the target state sees.
func TestMaxVisitsSeesTheArrivingTransitionsScopeReset(t *testing.T) {
	st, w := runOrder(t, orderDef(true), 1, 1, 2, 0)
	statusOf(t, st, wf.RunEnded, "MoCapped")
	if len(w.runs) != 1 {
		t.Fatalf("%d scripted runs left, want the round arrival capped after three visits", len(w.runs))
	}
}
