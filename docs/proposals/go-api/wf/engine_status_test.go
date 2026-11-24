package wf_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Engine-built status tests: every RunStatus below comes from Engine.Status on a
// real run, never from a literal, so the per-unit rows and totals are the ones
// the status view really prints.

// unitRow returns the one Units row with the given state and name; it fails
// the test when there is none.
func unitRow(t *testing.T, st wf.RunStatus, state, name string) wf.UnitStatus {
	t.Helper()
	for _, u := range st.Units {
		if u.State == state && u.Name == name {
			return u
		}
	}
	t.Fatalf("no %s row for %s; units = %+v", state, name, st.Units)
	return wf.UnitStatus{}
}

// checkRendered asserts the status view has one line per Units entry plus the
// totals line, each unit line starting with its name, and that the status
// survives a JSON round trip with the same Units.
func checkRendered(t *testing.T, st wf.RunStatus) string {
	t.Helper()
	out := wf.RenderStatus(st)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < len(st.Units)+1 {
		t.Fatalf("view has %d lines for %d units plus totals:\n%s", len(lines), len(st.Units), out)
	}
	for i, u := range st.Units {
		if !strings.HasPrefix(lines[i], u.Name) || !strings.Contains(lines[i], u.State) {
			t.Errorf("line %d = %q, want unit %s in state %s", i, lines[i], u.Name, u.State)
		}
	}
	if want := wf.StatusTotals(st); lines[len(st.Units)] != want {
		t.Errorf("line after the units = %q, want the totals line %q", lines[len(st.Units)], want)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back wf.RunStatus
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Units, st.Units) {
		t.Errorf("Units after a JSON round trip = %+v, want %+v", back.Units, st.Units)
	}
	return out
}

func TestEngineStatusLostAttempt(t *testing.T) {
	d := lsDef("es-lost", true)
	h := newHarnessOpts(t, d, nil, lsOpts())
	run, first := handOneUnit(t, h, d)
	if err := h.e.Started(run, first.Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(3*time.Minute + time.Second) // past the spawn window
	h.handOut(run, 0)

	st := h.status(run)
	lost := unitRow(t, st, "lost", first.Name)
	if lost.Note != "(attempt 1 lost)" || lost.Attempt != 1 {
		t.Errorf("lost row = %+v, want attempt 1 with note (attempt 1 lost)", lost)
	}
	var pending *wf.UnitStatus
	for i, u := range st.Units {
		if u.State == "pending" {
			pending = &st.Units[i]
		}
	}
	if pending == nil || pending.Attempt != 2 || pending.Note != "(attempt 1 lost)" || pending.Name == first.Name {
		t.Fatalf("pending row = %+v, want a new unit at attempt 2 noting the loss; units = %+v", pending, st.Units)
	}
	if totals := wf.StatusTotals(st); !strings.Contains(totals, "1 pending, 1 lost") {
		t.Errorf("totals = %q, want 1 pending and 1 lost", totals)
	}
	out := checkRendered(t, st)
	for _, name := range []string{first.Name, pending.Name} {
		if !strings.Contains(out, name+"  ") || strings.Count(out, "(attempt 1 lost)") != 2 {
			t.Errorf("view lacks the line of %s with its loss note:\n%s", name, out)
		}
	}
}

func TestEngineStatusWithdrawnMapChild(t *testing.T) {
	d := requeryDef("es-withdrawn")
	w := newWorld()
	setItems(w, 1, 2)
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	ins := h.handOut(run, 5)
	if len(ins) != 2 {
		t.Fatalf("HandOut(5) = %v, want both children", childIDs(ins))
	}
	names := map[string]string{}
	for _, in := range ins {
		names[in.Child] = in.Name
	}
	setItems(w, 1)
	h.handOut(run, 5)

	st := h.status(run)
	gone := unitRow(t, st, "withdrawn", names["chFan[2]"])
	if gone.Note != "item left the source" {
		t.Errorf("withdrawn row note = %q, want the withdrawal reason", gone.Note)
	}
	unitRow(t, st, "handed", names["chFan[1]"])
	if totals := wf.StatusTotals(st); !strings.Contains(totals, "1 handed, 1 withdrawn") {
		t.Errorf("totals = %q, want 1 handed and 1 withdrawn", totals)
	}
	out := checkRendered(t, st)
	if !strings.Contains(out, "item left the source") {
		t.Errorf("view lacks the withdrawal reason:\n%s", out)
	}
}

func TestEngineStatusSubNestedUnit(t *testing.T) {
	d := sfSubDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("Next = %+v, want the one at sfA", ins)
	}
	h.submit(run, ins[0], `{}`)
	// The Sub's child is now at its first agent, not yet handed out.
	st := h.status(run)
	got := unitKeys(st)
	want := []string{"pending:two-step/chWork", "future:two-step/chStep", "future:sfB", "done:sfA"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
	if st.Future != 2 {
		t.Errorf("Future = %d, want 2", st.Future)
	}
	if totals := wf.StatusTotals(st); !strings.Contains(totals, "1 pending, 1 done") || !strings.Contains(totals, "+2 future") {
		t.Errorf("totals = %q, want 1 pending, 1 done and 2 future steps", totals)
	}
	checkRendered(t, st)

	child := h.handOut(run, 1)
	if len(child) != 1 || child[0].State != "two-step/chWork" {
		t.Fatalf("HandOut(1) = %+v, want the child's first state", child)
	}
	if err := h.e.Started(run, child[0].Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	st = h.status(run)
	row := unitRow(t, st, "started", child[0].Name)
	if row.Class != "two-step/chWork" || row.Session != "sess-1" {
		t.Errorf("started row = %+v, want class two-step/chWork with session sess-1", row)
	}
	if out := checkRendered(t, st); !strings.Contains(out, "session sess-1") {
		t.Errorf("view lacks the claiming session:\n%s", out)
	}
}

func TestEngineStatusExampleFutureAndLowerBound(t *testing.T) {
	d := critique.Definition()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0) // drive to the first agent
	st := h.status(run)
	counted, future := 0, 0
	for _, u := range st.Units {
		if u.State == "future" {
			future++
		} else {
			counted++
		}
	}
	if future == 0 || st.Future == 0 {
		t.Fatalf("future rows = %d, Future = %d, want future steps ahead of the first agent", future, st.Future)
	}
	if !st.Progress.TotalIsLowerBound {
		t.Errorf("Progress = %+v, want TotalIsLowerBound with unentered agent states", st.Progress)
	}
	if want := "subagents ≥ " + itoaTest(counted); !strings.HasPrefix(wf.StatusTotals(st), want) {
		t.Errorf("totals = %q, want prefix %q (N = the %d counted units)", wf.StatusTotals(st), want, counted)
	}
	checkRendered(t, st)
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
