package wf

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

type (
	stA   struct{}
	stB   struct{}
	stC   struct{}
	stEnd struct{}
	stOut struct{}
)

// stChain is A -> B -> C -> End, all agents.
func stChain() *Def {
	d := New("st-chain")
	a := NewAgent[stA, stOut](d)
	b := NewAgent[stB, stOut](d)
	c := NewAgent[stC, stOut](d)
	end := NewTerminal[stEnd](d, Success)
	a.Spec(PromptText("a"), Retry[stA](Const(1), a.To(end, "gave up")))
	b.Spec(PromptText("b"), Retry[stB](Const(1), b.To(end, "gave up")))
	c.Spec(PromptText("c"), Retry[stC](Const(1), c.To(end, "gave up")))
	ea, eb, ec := a.To(b, "next"), b.To(c, "next"), c.To(end, "next")
	a.Route(func(stOut, Ctx) Edge[stA] { return ea }, ea)
	b.Route(func(stOut, Ctx) Edge[stB] { return eb }, eb)
	c.Route(func(stOut, Ctx) Edge[stC] { return ec }, ec)
	return d
}

var statsT0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func mins(n float64) time.Duration { return time.Duration(n * float64(time.Minute)) }

func rec(kind JournalKind, inst string, at time.Time, data any) JournalRecord {
	r := JournalRecord{Kind: kind, Instance: inst, At: at}
	if data != nil {
		r.Data, _ = json.Marshal(data)
	}
	return r
}

// attemptRecs is one attempt of unit name on inst, ending in end after dur.
func attemptRecs(inst, name string, start time.Time, dur time.Duration, end JournalKind) []JournalRecord {
	return []JournalRecord{
		rec(JournalAttemptStarted, inst, start, AttemptStartedData{Name: name}),
		rec(end, inst, start.Add(dur), nil),
	}
}

// statsState is a root instance of d with the given unit names indexed at state.
func statsState(d *Def, state string, names ...string) *runState {
	st := &runState{insts: map[string]*instance{"": {def: d, DefName: d.name, Cur: state}}, order: []string{""}}
	st.initIndex()
	for i, n := range names {
		st.names[n] = unitRef{Instance: "", State: state, Attempt: i + 1}
	}
	return st
}

func near(a, b time.Duration) bool {
	d := a - b
	return d > -time.Microsecond && d < time.Microsecond
}

func TestDurationEWMA(t *testing.T) {
	st := statsState(stChain(), "stA", "stA#1", "stA#2", "stA#3")
	var recs []JournalRecord
	for i, m := range []float64{10, 20, 30} {
		name := []string{"stA#1", "stA#2", "stA#3"}[i]
		recs = append(recs, attemptRecs("", name, statsT0.Add(mins(float64(100*i))), mins(m), JournalAccepted)...)
	}
	s := foldStats(recs, st, statsConfig{})
	got, ok := s.Duration("stA")
	want := mins(0.3*30 + 0.7*(0.3*20+0.7*10))
	if !ok || !near(got, want) {
		t.Errorf("Duration(stA) = %v, %v; want %v, true", got, ok, want)
	}
	if d, ok := s.Duration("stB"); ok {
		t.Errorf("Duration of an unseen class = %v, true; want ok == false", d)
	}
}

func TestDurationClosersAndDropped(t *testing.T) {
	st := statsState(stChain(), "stA", "stA#1", "stA#2", "stA#3", "stA#4")
	var recs []JournalRecord
	recs = append(recs, attemptRecs("", "stA#1", statsT0, mins(10), JournalRefused)...)
	// A lost attempt teaches no duration and stops counting as running.
	recs = append(recs, rec(JournalAttemptStarted, "", statsT0.Add(mins(20)), AttemptStartedData{Name: "stA#2"}))
	recs = append(recs, rec(JournalAttemptLost, "", statsT0.Add(mins(90)), AttemptLostData{Name: "stA#2"}))
	recs = append(recs, attemptRecs("", "stA#3", statsT0.Add(mins(100)), mins(20), JournalLateVote)...)
	s := foldStats(recs, st, statsConfig{})
	got, _ := s.Duration("stA")
	if want := mins(0.3*20 + 0.7*10); !near(got, want) {
		t.Errorf("Duration = %v, want %v (refused and late_vote count, lost does not)", got, want)
	}
}

func TestParallelismEWMA(t *testing.T) {
	d := stChain()
	st := &runState{insts: map[string]*instance{"": {def: d}, "a": {ID: "a", def: d, DefName: "kid"}, "b": {ID: "b", def: d, DefName: "kid"}}}
	st.initIndex()
	st.names["x#1"] = unitRef{Instance: "a", State: "stA"}
	st.names["y#1"] = unitRef{Instance: "b", State: "stA"}
	recs := []JournalRecord{
		rec(JournalAttemptStarted, "a", statsT0, AttemptStartedData{Name: "x#1"}),
		rec(JournalAttemptStarted, "b", statsT0.Add(mins(1)), AttemptStartedData{Name: "y#1"}),
	}
	s := foldStats(recs, st, statsConfig{})
	// The second start sees two running units of kid/stA: 0.3*2 + 0.7*1.
	if got, want := s.classes["kid/stA"].par, 1.3; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("parallelism EWMA = %v, want %v", got, want)
	}
}

func TestStallThresholdDefault(t *testing.T) {
	st := statsState(stChain(), "stA", "stA#1")
	s := foldStats(attemptRecs("", "stA#1", statsT0, mins(10), JournalAccepted), st, statsConfig{})
	if got := s.StallThreshold("stA"); got != mins(30) {
		t.Errorf("StallThreshold with history = %v, want 3x10m", got)
	}
	if got := s.StallThreshold("stB"); got != mins(20) {
		t.Errorf("StallThreshold without history = %v, want the 20m default", got)
	}
}

// statsWith is classStats with fixed class estimates.
func statsWith(dur, par float64, classes ...string) *classStats {
	s := &classStats{cfg: statsConfig{}.withDefaults(), classes: map[string]*classStat{}}
	for _, c := range classes {
		s.classes[c] = &classStat{dur: float64(mins(dur)), par: par, samples: 1}
	}
	return s
}

func TestRemaining(t *testing.T) {
	s := statsWith(10, 1, "x")
	now := statsT0.Add(mins(30))
	for _, tc := range []struct {
		u    unitView
		want time.Duration
		ok   bool
	}{
		{unitView{Class: "x", State: UnitRunning, Since: now.Add(-mins(4))}, mins(6), true},
		{unitView{Class: "x", State: UnitRunning, Since: now.Add(-mins(14))}, -mins(4), true}, // over
		{unitView{Class: "x", State: UnitPending, Since: now.Add(-mins(4))}, mins(10), true},
		{unitView{Class: "x", State: UnitDone, Since: now.Add(-mins(4))}, 0, true},
		{unitView{Class: "unseen", State: UnitRunning, Since: now}, 0, false},
	} {
		if got, ok := s.Remaining(tc.u, now); got != tc.want || ok != tc.ok {
			t.Errorf("Remaining(%v %s) = %v, %v; want %v, %v", tc.u.Class, tc.u.State, got, ok, tc.want, tc.ok)
		}
	}
}

func TestETANeverBelowCriticalPath(t *testing.T) {
	s := statsWith(10, 1, "x", "y")
	s.classes["x"].par, s.classes["y"].par = 5, 5 // 10 units in parallel
	now := statsT0.Add(time.Hour)
	units := []unitView{{Class: "x", State: UnitRunning, Since: now.Add(-mins(5))}}
	got, ok := s.ETA(units, [][]string{{"x", "y"}}, now)
	if !ok || got != mins(25) {
		t.Errorf("ETA = %v, %v; want 25m (5m left plus two 10m states), not the 2.5m the work spread over 10 parallel units gives", got, ok)
	}
	if _, ok := (&classStats{classes: map[string]*classStat{}}).ETA(units, nil, now); ok {
		t.Error("ETA with no history reported ok")
	}
}

func TestETAUsesWorkOverParallelism(t *testing.T) {
	s := statsWith(10, 2, "x")
	now := statsT0.Add(time.Hour)
	units := []unitView{
		{Class: "x", State: UnitPending, Ready: true, Since: now},
		{Class: "x", State: UnitPending, Ready: true, Since: now},
		{Class: "x", State: UnitPending, Ready: true, Since: now},
		{Class: "x", State: UnitPending, Ready: true, Since: now},
	}
	// 40m of work over 2 parallel units beats the 10m critical path.
	if got, _ := s.ETA(units, nil, now); got != mins(20) {
		t.Errorf("ETA = %v, want 20m", got)
	}
}

func TestETAIncludesFutureSteps(t *testing.T) {
	// The chain is Compute stA -> Agent stB. An Agent always carries a Retry
	// exhausted edge, so futureSteps (single-edge states only) stops after the
	// first agent it reaches until an earlier revision (Task 71) ignores fallback edges;
	// stChain's three-agent path is therefore not the input here.
	d := New("st-future")
	a := NewCompute[stA, stOut](d, func(Ctx) (stOut, error) { return stOut{}, nil })
	b := NewAgent[stB, stOut](d)
	end := NewTerminal[stEnd](d, Success)
	b.Spec(PromptText("b"), Retry[stB](Const(1), b.To(end, "gave up")))
	ea, eb := a.To(b, "next"), b.To(end, "next")
	a.Route(func(stOut, Ctx) Edge[stA] { return ea }, ea)
	b.Route(func(stOut, Ctx) Edge[stB] { return eb }, eb)
	st := statsState(d, "stA")
	fs := (&cycle{st: st}).futureSteps()
	if len(fs) != 1 {
		t.Fatalf("futureSteps = %d entries, want 1", len(fs))
	}
	future := fs[0].classes()
	if want := []string{"stB"}; !slices.Equal(future, want) {
		t.Fatalf("futureSteps classes at stA = %v, want %v", future, want)
	}
	s := statsWith(10, 1, "stA", "stB")
	now := statsT0.Add(time.Hour)
	units := []unitView{{Class: "stA", State: UnitRunning, Since: now.Add(-mins(2))}}
	if got, _ := s.ETA(units, [][]string{future}, now); got != mins(18) {
		t.Errorf("ETA = %v, want 18m: 8m left of stA, then stB", got)
	}
	if got, _ := s.ETA(units, nil, now); got != mins(8) {
		t.Errorf("ETA without the future path = %v, want 8m", got)
	}
	st.insts[""].Cur = "stB"
	fs = (&cycle{st: st}).futureSteps()
	if len(fs) != 1 || len(fs[0].classes()) != 0 {
		t.Errorf("futureSteps at stB = %v, want one entry with no classes", fs)
	}
}

func TestETAUnseenClassUsesMean(t *testing.T) {
	s := statsWith(10, 1, "x")
	now := statsT0.Add(time.Hour)
	units := []unitView{{Class: "x", State: UnitRunning, Since: now.Add(-mins(10))}}
	if got, _ := s.ETA(units, [][]string{{"never-seen"}}, now); got != mins(10) {
		t.Errorf("ETA = %v, want 10m (the unseen class takes the mean estimate)", got)
	}
}

func TestBinding(t *testing.T) {
	s := statsWith(10, 1, "x")
	now := statsT0.Add(time.Hour)
	ready := func(age time.Duration) unitView {
		return unitView{Class: "x", State: UnitPending, Ready: true, Since: now.Add(-age)}
	}
	blocked := unitView{Class: "x", State: UnitPending, Since: now}
	run := unitView{Class: "x", State: UnitRunning, Since: now.Add(-mins(1))}
	for _, tc := range []struct {
		name  string
		units []unitView
		gates int
		want  Binding
	}{
		{"human gate, nothing running", []unitView{blocked}, 1, BindingHuman},
		{"human gate, but a unit runs", []unitView{run}, 1, BindingNone},
		{"old ready work binds nothing: the engine has no cap", []unitView{ready(mins(60)), ready(mins(1)), run}, 0, BindingNone},
		{"graph", []unitView{blocked, blocked, ready(mins(1)), run}, 0, BindingGraph},
		{"none", []unitView{run, ready(mins(1))}, 0, BindingNone},
	} {
		if got := s.Binding(tc.units, tc.gates); got != tc.want {
			t.Errorf("%s: Binding = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestProgressLowerBound(t *testing.T) {
	s := statsWith(10, 1, "x")
	units := []unitView{
		{State: UnitDone}, {State: UnitFailed}, {State: UnitRunning},
		{State: UnitPending, Ready: true}, {State: UnitPending}, {State: UnitPending},
	}
	p := s.Progress(units, false, 0)
	if p.Done != 2 || p.Total != 6 || p.WaitingOnDeps != 2 || p.TotalIsLowerBound {
		t.Errorf("Progress = %+v, want Done 2 Total 6 WaitingOnDeps 2 exact", p)
	}
	if !s.Progress(units, true, 0).TotalIsLowerBound {
		t.Error("growing source: want TotalIsLowerBound")
	}
	if !s.Progress(units, false, 1).TotalIsLowerBound {
		t.Error("future agent states: want TotalIsLowerBound")
	}
}
