package wf_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func mins(n int) time.Duration { return time.Duration(n) * time.Minute }

func TestRenderStatusGolden(t *testing.T) {
	sec := time.Second
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rs := wf.RunStatus{
		Run: "r1", Phase: wf.RunRunning,
		Started: t0, Now: t0.Add(mins(42)),
		ETA: mins(65), ETAKnown: true, Binding: wf.BindingGraph, Future: 3,
		UnitCounts: wf.UnitCounts{Ready: 3, Handed: 1, Running: 4},
		Progress:   wf.Progress{Done: 1, Total: 12, TotalIsLowerBound: true},
		Units: []wf.UnitStatus{
			{Name: "Review/b#1", State: "stalled", Elapsed: mins(12) + 4*sec, Quiet: mins(9) + 40*sec, Session: "sess-1"},
			{Name: "Review/a#1", State: "running", Elapsed: mins(2) + 10*sec, Estimate: mins(3) + 15*sec, Remaining: 65 * sec, Session: "sess-1"},
			{Name: "Review/over#1", State: "running", Elapsed: mins(5), Estimate: mins(4), Remaining: -mins(1), Session: "sess-2"},
			{Name: "Review/s#1", State: "started", Elapsed: 5 * sec, Session: "sess-2"},
			{Name: "Review/h#1", State: "handed", Elapsed: 20 * sec, Window: mins(2)},
			{Name: "Review/p#1", State: "pending", Estimate: mins(3) + 15*sec},
			{Name: "Review/p#2", State: "pending", Note: "(attempt 1 lost)"},
			{Name: "Impl/Sub/x#1", State: "pending", Estimate: mins(1)},
			{Name: "Review/w#1", State: "waiting", Estimate: mins(3), Note: "deps"},
			{Name: "Merge", State: "future", Estimate: mins(2)},
			{Name: "Review/d#1", State: "done", Elapsed: mins(3)},
			{Name: "Review/f#1", State: "failed", Elapsed: mins(1) + 30*sec},
			{Name: "Review/l#1", State: "lost", Note: "(attempt 1 lost)"},
			{Name: "Review/x#1", State: "withdrawn", Note: "item left the source"},
		},
	}
	want := `Review/b#1     stalled    12m04s  quiet 9m40s  session sess-1
Review/a#1     running    2m10s  eta ~1m05s  session sess-1
Review/over#1  running    5m00s  over 1m00s  session sess-2
Review/s#1     started    0m05s  session sess-2
Review/h#1     handed     0m20s  window 2m
Review/p#1     pending    est ~3m15s
Review/p#2     pending    --  (attempt 1 lost)
Impl/Sub/x#1   pending    est ~1m00s
Review/w#1     waiting    est ~3m00s  deps
Merge          future     est ~2m00s
Review/d#1     done       3m00s
Review/f#1     failed     1m30s
Review/l#1     lost       --  (attempt 1 lost)
Review/x#1     withdrawn  --  item left the source
subagents ≥ 13: 1 stalled, 2 running, 1 started, 1 handed, 3 pending, 1 waiting, 1 done, 1 failed, 1 lost, 1 withdrawn (+3 future steps not counted) · elapsed 0:42 · eta ~1:05 · graph-bound
harness: generic (legacy generic fallback)
units: 3 ready, 1 handed, 4 running
`
	if got := wf.RenderStatus(rs); got != want {
		t.Errorf("RenderStatus:\n%s\nwant:\n%s", got, want)
	}
}

// stDef is a three-step chain whose second step branches: from the first step
// only the second is on an unconditional path.
type (
	stA1   struct{}
	stA2   struct{}
	stA3   struct{}
	stEnd  struct{}
	stEnd2 struct{}
)

func stBranchDef() *wf.Def {
	d := wf.New("st-branch")
	a1 := wf.NewAgent[stA1, clAnswer](d)
	a2 := wf.NewAgent[stA2, clAnswer](d)
	a3 := wf.NewAgent[stA3, clAnswer](d)
	end := wf.NewTerminal[stEnd](d, wf.Success)
	end2 := wf.NewTerminal[stEnd2](d, wf.Stopped)
	a1.Spec(wf.PromptText("one"), wf.RetryPolicy[stA1]{})
	a2.Spec(wf.PromptText("two"), wf.RetryPolicy[stA2]{})
	a3.Spec(wf.PromptText("three"), wf.RetryPolicy[stA3]{})
	e12 := a1.To(a2, "next")
	a1.Route(func(clAnswer, wf.Ctx) wf.Edge[stA1] { return e12 }, e12)
	e23, e2e := a2.To(a3, "more"), a2.To(end2, "stop")
	a2.Route(func(clAnswer, wf.Ctx) wf.Edge[stA2] { return e23 }, e23, e2e)
	e3 := a3.To(end, "ok")
	a3.Route(func(clAnswer, wf.Ctx) wf.Edge[stA3] { return e3 }, e3)
	return d
}

func TestStatusFutureCount(t *testing.T) {
	d := stBranchDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0) // issues the first instruction without handing it out
	st := h.status(run)
	if st.Future != 2 {
		t.Errorf("Future = %d, want 2 (stA2, stA3 ahead of stA1)", st.Future)
	}
	var states []string
	for _, u := range st.Units {
		states = append(states, u.State+":"+u.Class)
	}
	if want := []string{"pending:stA1", "future:stA2"}; !reflect.DeepEqual(states, want) {
		t.Errorf("units = %v, want %v (stA3 lies past the branch: counted, not listed)", states, want)
	}
	got := wf.RenderStatus(st)
	if !strings.Contains(got, "subagents ≥ 1:") || !strings.Contains(got, "(+2 future steps not counted)") {
		t.Errorf("totals line lacks the lower bound or the future count:\n%s", got)
	}
}

// Status counts what a coordinator acts on: ready, handed and running units.
// A hand-out stays handed while any child of the run runs. Ready work that waits never
// binds the run: the engine has no cap of its own.
func TestStatusUnitCounts(t *testing.T) {
	d := fanDef("st-counts", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut(1) = %+v", ins)
	}
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if again := h.handOut(run, 1); len(again) != 1 {
		t.Fatalf("second HandOut(1) = %+v", again)
	}
	st := h.status(run)
	if want := (wf.UnitCounts{Ready: 1, Handed: 1, Running: 1}); st.UnitCounts != want {
		t.Fatalf("counts = %+v, want %+v", st.UnitCounts, want)
	}
	if got := wf.RenderStatus(st); !strings.Contains(got, "units: 1 ready, 1 handed, 1 running\n") || strings.Contains(strings.ToLower(got), "slot") {
		t.Errorf("view lacks the counts or names a slot:\n%s", got)
	}
	h.now = h.now.Add(2*time.Minute + 30*time.Second) // past the hand-out window, inside the spawn window
	st = h.status(run)
	if want := (wf.UnitCounts{Ready: 1, Handed: 1, Running: 1}); st.UnitCounts != want {
		t.Fatalf("after the hand-out window counts = %+v, want %+v (a hand-out never expires while a child runs)", st.UnitCounts, want)
	}
	h.now = h.now.Add(time.Hour)
	if st := h.status(run); st.Binding != wf.BindingNone {
		t.Errorf("Binding = %q after an hour of waiting ready work, want none", st.Binding)
	}
}

// isReasonLine is a line of the status reasons that follow the totals line.
func isReasonLine(l string) bool {
	for _, p := range []string{"due: ", "open effect ", "lock: ", "nothing ready: "} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func TestStatusUnitsMatchJSON(t *testing.T) {
	d := fanDef("st-json", chItems(1, 2, 3, 4), 4, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.handOut(run, 3)
	if len(ins) != 3 {
		t.Fatalf("HandOut(3) = %d units", len(ins))
	}
	if err := h.e.Started(run, ins[0].Name, claim("a")); err != nil {
		t.Fatal(err)
	}
	if err := h.e.Started(run, ins[1].Name, claim("b")); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if _, err := h.e.Done(run, ins[0].Name, wf.Submission{Output: []byte(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	st := h.status(run)
	if len(st.Units) < 4 {
		t.Fatalf("units = %+v, want a done, a running, a handed and a pending one", st.Units)
	}
	var order []string
	for _, u := range st.Units {
		order = append(order, u.State)
	}
	if want := []string{"started", "handed", "pending", "done"}; !reflect.DeepEqual(order, want) {
		t.Errorf("unit states = %v, want %v (active, handed, pending, then settled)", order, want)
	}
	var lines []string
	for _, l := range strings.Split(wf.RenderStatus(st), "\n") {
		if !strings.HasPrefix(l, "subagents") && l != "" && !strings.HasPrefix(l, "units:") && !strings.HasPrefix(l, "harness:") && !isReasonLine(l) {
			lines = append(lines, l)
		}
	}
	if len(lines) != len(st.Units) {
		t.Fatalf("%d unit lines for %d units:\n%s", len(lines), len(st.Units), strings.Join(lines, "\n"))
	}
	for i, u := range st.Units {
		if !strings.HasPrefix(lines[i], u.Name+" ") {
			t.Errorf("line %d = %q, want unit %s", i, lines[i], u.Name)
		}
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
		t.Errorf("JSON Units differ:\n%+v\nwant\n%+v", back.Units, st.Units)
	}
}
