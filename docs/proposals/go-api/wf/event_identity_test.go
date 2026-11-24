package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	eiLoop struct{}
	eiEnd  struct{}
)

// eiRevisitDef is one agent state that routes back to itself while the output
// says OK, then to the terminal: its second visit has a unit of its own.
func eiRevisitDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[eiLoop, clAnswer](d)
	end := wf.NewTerminal[eiEnd](d, wf.Success)
	a.Spec(wf.PromptText("Again?"), wf.RetryPolicy[eiLoop]{})
	again := a.To(a, "again")
	out := a.To(end, "done")
	a.Route(func(o clAnswer, _ wf.Ctx) wf.Edge[eiLoop] {
		if o.OK {
			return again
		}
		return out
	}, again, out)
	return d
}

func TestWaitEventsNameEachVisit(t *testing.T) {
	d := eiRevisitDef("ei-visits")
	w := newWaiter(t, d, wf.EngineOptions{})
	run := w.start(d)
	first := w.started(run)
	if _, err := w.e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	second := w.started(run)
	if second.Name == first.Name {
		t.Fatalf("both visits are called %q", first.Name)
	}
	if _, err := w.e.Done(run, second.Name, wf.Submission{Output: json.RawMessage(`{"OK":false}`)}); err != nil {
		t.Fatal(err)
	}
	done := eventsOf(w.wait(run, 0, time.Second), wf.EventDone)
	if len(done) != 2 || done[0].Name != first.Name || done[1].Name != second.Name {
		t.Fatalf("done events = %+v, want %s then %s", done, first.Name, second.Name)
	}
}

// eiHaltWorld runs the command def over a world whose apply fails: with no
// Retry that halts the step Blocked.
func eiHaltWorld(t *testing.T, retry func(*wf.Command[cRun], *wf.Terminal[cFail]) wf.RetryPolicy[cRun]) (*harness, wf.RunID) {
	t.Helper()
	d, _ := commandDef(retry)
	w := newWorld()
	w.applyErr = errors.New("exec: permission denied")
	h := newHarness(t, d, w)
	return h, h.run(d, map[string]string{"topic": "x"})
}

func TestWaitBlockedOnCommandHalt(t *testing.T) {
	h, run := eiHaltWorld(t, nil)
	begin := time.Now()
	res, err := h.e.Wait(run, -1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Fatalf("Wait took %v, want a wake well before the 5s timeout", took)
	}
	ev := oneEvent(t, res, wf.EventBlocked)
	if ev.State != "cRun" || ev.Name != "" || !strings.HasPrefix(ev.Line(), "blocked cRun") {
		t.Fatalf("event = %+v (line %q), want blocked at cRun with no unit", ev, ev.Line())
	}
	if res.UnitCounts != (wf.UnitCounts{}) {
		t.Fatalf("counts = %+v, a blocked run has no unit to hand out or running", res.UnitCounts)
	}
}

func TestWaitNonHaltingRefusalNoEvent(t *testing.T) {
	// Retry(2): the first failed apply is spent and re-issued, the second takes
	// the gave-up edge; neither is a halt, so neither is blocked or failed.
	h, run := eiHaltWorld(t, func(c *wf.Command[cRun], fail *wf.Terminal[cFail]) wf.RetryPolicy[cRun] {
		return wf.Retry[cRun](wf.Const(2), c.To(fail, "gave up"))
	})
	res, err := h.e.Wait(run, -1, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range res.Events {
		if ev.Kind == wf.EventBlocked || ev.Kind == wf.EventFailed {
			t.Fatalf("events = %+v, a re-issue or fallback edge is not a halt", res.Events)
		}
	}
	oneEvent(t, res, wf.EventRunEnded)
}

func TestLostNoteReadsIndexAttempt(t *testing.T) {
	d := eiRevisitDef("ei-lost")
	h := newHarnessOpts(t, d, nil, lsOpts())
	run := h.run(d, nil)
	first := h.handOut(run, 1)[0]
	if err := h.e.Started(run, first.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.Done(run, first.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	second := h.handOut(run, 1)[0] // the second visit; its name ends #2
	if err := h.e.Started(run, second.Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(4 * time.Minute)
	h.handOut(run, 0) // the spawn window expires: the second visit's attempt 1 is lost
	for _, u := range h.status(run).Units {
		if u.State == "pending" && u.Note != "" {
			if u.Note != "(attempt 1 lost)" {
				t.Fatalf("note of the reissue of %s = %q, want (attempt 1 lost): the lost unit's attempt, not its visit ordinal", second.Name, u.Note)
			}
			return
		}
	}
	t.Fatalf("no pending unit carries a lost note: %+v", h.status(run).Units)
}
