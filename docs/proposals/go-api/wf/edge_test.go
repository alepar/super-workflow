package wf_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	edChk   struct{}
	edPlain struct{}
	edRes   struct{ Failed bool }
)

func shellEffect(exit string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"argv": []string{"false"}, "dir": exit}}
}

// edgeDef: A takes an edge carrying the effects to Chk, a compute that reads
// EdgeOutcome and routes Done or Bad. optional marks the edge Optional, so a
// failed effect still enters Chk; otherwise a failed effect stops the edge and
// A takes its Blocked route ("edge blocked", to Bad).
func edgeDef(optional bool, effects ...wf.Effect) *wf.Def {
	return edgeDefRouted(optional, "", effects...)
}

// edgeDefRouted is edgeDef with an OptionalRoutedBy note on the Optional edge.
func edgeDefRouted(optional bool, routedBy string, effects ...wf.Effect) *wf.Def {
	d := wf.New("edge")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	chk := wf.NewCompute[edChk, edRes](d, func(c wf.Ctx) (edRes, error) {
		return edRes{Failed: wf.EdgeOutcome(c).Failed()}, nil
	})
	toDone, toBad := chk.To(done, "ok"), chk.To(bad, "failed")
	chk.Route(func(o edRes, _ wf.Ctx) wf.Edge[edChk] {
		if o.Failed {
			return toBad
		}
		return toDone
	}, toDone, toBad)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "edge blocked")))
	next := a.To(chk, "go")
	for _, e := range effects {
		next = next.With(e)
	}
	switch {
	case routedBy != "":
		next = next.OptionalRoutedBy(routedBy)
	case optional:
		next = next.Optional()
	}
	a.Then(next)
	return d
}

// Then is Route with one candidate: the same hash, the edge's effects and
// Optional kept.
func TestThenIsRouteOfOneEdge(t *testing.T) {
	build := func(then bool) *wf.Def {
		d := wf.New("then")
		a := wf.NewAgent[eA, eOut](d)
		done, bad := twoTerminals(d)
		a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "edge blocked")))
		next := a.To(done, "go").With(wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"argv": []string{"true"}}}).Optional()
		if then {
			a.Then(next)
		} else {
			a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
		}
		return d
	}
	if got, want := build(true).Hash(), build(false).Hash(); got != want {
		t.Fatalf("Then hash %s, Route hash %s", got, want)
	}
}

func lastEffectResult(t *testing.T, h *harness, run wf.RunID) wf.EffectResultData {
	t.Helper()
	rs := h.kind(run, wf.JournalEffectResult)
	if len(rs) == 0 {
		t.Fatal("no effect_result journaled")
	}
	var rd wf.EffectResultData
	if err := json.Unmarshal(rs[len(rs)-1].Data, &rd); err != nil {
		t.Fatal(err)
	}
	return rd
}

func transitionTo(t *testing.T, h *harness, run wf.RunID, to string) wf.TransitionData {
	t.Helper()
	for _, r := range h.kind(run, wf.JournalTransition) {
		var td wf.TransitionData
		if err := json.Unmarshal(r.Data, &td); err != nil {
			t.Fatal(err)
		}
		if td.To == to {
			return td
		}
	}
	t.Fatalf("no transition to %s", to)
	return wf.TransitionData{}
}

func TestShellEdgeNonZeroExitIsFailed(t *testing.T) {
	d := edgeDef(true, shellEffect(""), mergeEffect(nil))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 3, Stderr: "  boom\n"}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	rd := lastEffectResult(t, h, run)
	if rd.OK || rd.Outcome != "failed" || rd.Error != "exit 3: boom" {
		t.Fatalf("effect_result = %+v, want failed with %q", rd, "exit 3: boom")
	}
	if !strings.Contains(string(rd.Result), `"Exit":3`) {
		t.Fatalf("Result = %s, want the RunResult kept", rd.Result)
	}
	if w.appliedCount() != 1 {
		t.Fatalf("applied = %d, want 1: the rest of the edge is skipped", w.appliedCount())
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad") // the transition was taken, and the target saw the failure
}

func TestShellEdgeExitErrorIsBounded(t *testing.T) {
	d := edgeDef(true, shellEffect(""))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 1, Stderr: strings.Repeat("x", 5000)}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	if got := len(lastEffectResult(t, h, run).Error); got > len("exit 1: ")+2000 {
		t.Fatalf("error length = %d, want stderr trimmed to 2000 bytes", got)
	}
}

func TestCommandExitStaysData(t *testing.T) {
	d := wf.New("cmd")
	cmd := wf.NewCommand[edPlain](d, "false")
	done, bad := twoTerminals(d)
	toDone, toBad := cmd.To(done, "ran"), cmd.To(bad, "bad")
	cmd.Route(func(r wf.RunResult, _ wf.Ctx) wf.Edge[edPlain] {
		if r.Exit == 3 {
			return toDone
		}
		return toBad
	}, toDone, toBad)
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 3}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.next(run)
	if rd := lastEffectResult(t, h, run); !rd.OK || rd.Outcome != "applied" {
		t.Fatalf("command effect_result = %+v, want applied: exit is data", rd)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
}

func TestEdgeOutcomeReadByTarget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runs    []wf.RunResult
		wantEnd string
	}{
		{"failed effect", []wf.RunResult{{Exit: 1}}, "eBad"},
		{"clean effect", []wf.RunResult{{Exit: 0}}, "eDone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := edgeDef(true, shellEffect(""))
			w := newWorld()
			w.runs = tc.runs
			h := newHarness(t, d, w)
			run := h.run(d, nil)
			h.drive(run, map[string][]string{"eA": {`{}`}})
			statusOf(t, h.status(run), wf.RunEnded, tc.wantEnd)
			// B4: going on past the failed effect is a degraded record.
			ds := degradedOf(t, h, run)
			if failed := tc.wantEnd == "eBad"; failed != (len(ds) == 1 && ds[0].Kind == wf.DegradedOptional && strings.Contains(ds[0].Cause, "exit 1")) || !failed && len(ds) != 0 {
				t.Fatalf("degraded = %+v, want one optional record exactly when the effect failed", ds)
			}
		})
	}
}

// An Optional edge whose target routes a failed effect itself says so in the
// degraded record: what happened, not an Optional edge going on.
func TestOptionalRoutedByNamesWhatHappened(t *testing.T) {
	d := edgeDefRouted(true, "Chk reads the outcome and routes a failure to Bad", shellEffect(""))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 1}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
	ds := degradedOf(t, h, run)
	if len(ds) != 1 || !strings.Contains(ds[0].Action, "which routes it: Chk reads the outcome") || strings.Contains(ds[0].Action, "(Optional edge)") {
		t.Fatalf("degraded = %+v, want the routing named", ds)
	}
}

// refusedClasses is the class of each refused record of the run, in order.
func refusedClasses(t *testing.T, h *harness, run wf.RunID) []wf.OutcomeClass {
	t.Helper()
	var out []wf.OutcomeClass
	for _, r := range h.kind(run, wf.JournalRefused) {
		out = append(out, r.Class)
	}
	return out
}

func hasTransitionTo(h *harness, run wf.RunID, to string) bool {
	for _, r := range h.kind(run, wf.JournalTransition) {
		var td wf.TransitionData
		if json.Unmarshal(r.Data, &td) == nil && td.To == to {
			return true
		}
	}
	return false
}

// A1: a failed effect on a non-Optional edge stops the transition; the source
// state takes its route for the effect's class.
func TestFailedEdgeEffectStopsTransition(t *testing.T) {
	d := edgeDef(false, shellEffect(""), mergeEffect(nil))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 2, Stderr: "nope"}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	if hasTransitionTo(h, run, "edChk") {
		t.Fatal("the edge into edChk was taken past its failed effect")
	}
	if w.appliedCount() != 1 {
		t.Fatalf("applied = %d, want 1: the rest of the edge is skipped", w.appliedCount())
	}
	if got := refusedClasses(t, h, run); len(got) != 1 || got[0] != wf.Blocked {
		t.Fatalf("refused classes = %v, want one Blocked for the failed effect", got)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
	if h.kind(run, wf.JournalRefused)[0].Data == nil || !strings.Contains(string(h.kind(run, wf.JournalRefused)[0].Data), "exit 2: nope") {
		t.Fatalf("refused = %s, want the effect's error as the reason", h.kind(run, wf.JournalRefused)[0].Data)
	}
}

// A1 with C2: a source with no route of its own takes the definition default,
// an unavailable effect is classed Unavailable, and with no route at all the
// instance halts rather than proceeding.
func TestFailedEdgeEffectRoutes(t *testing.T) {
	build := func(withDefault bool) *wf.Def {
		d := wf.New("edgedefault")
		c := wf.NewCompute[edPlain, edRes](d, func(wf.Ctx) (edRes, error) { return edRes{}, nil })
		done, bad := twoTerminals(d)
		next := c.To(done, "go").With(shellEffect(""))
		c.Route(func(edRes, wf.Ctx) wf.Edge[edPlain] { return next }, next)
		if withDefault {
			d.OnFailure(wf.Blocked, bad).OnFailure(wf.Unavailable, bad)
		}
		return d
	}
	t.Run("default route", func(t *testing.T) {
		d := build(true)
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: 1}}
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.next(run)
		statusOf(t, h.status(run), wf.RunEnded, "eBad")
	})
	t.Run("unavailable effect", func(t *testing.T) {
		d := build(true)
		w := newWorld()
		w.applyErr = wf.ErrUnavailable
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.next(run)
		passBackoff(h, run)
		if got := refusedClasses(t, h, run); len(got) == 0 || got[len(got)-1] != wf.Unavailable {
			t.Fatalf("refused classes = %v, want Unavailable last", got)
		}
	})
	t.Run("no route halts", func(t *testing.T) {
		d := build(false)
		w := newWorld()
		w.runs = []wf.RunResult{{Exit: 1}}
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.next(run)
		if st := h.status(run); st.Phase == wf.RunEnded || hasTransitionTo(h, run, "eDone") {
			t.Fatalf("status = %+v, want the run halted short of eDone", st)
		}
	})
}

func TestOptionalEdgeIsHashed(t *testing.T) {
	plain, optional := edgeDef(false, shellEffect("")), edgeDef(true, shellEffect(""))
	for _, sv := range wf.Inspect(plain).States {
		b, _ := json.Marshal(sv)
		if strings.Contains(string(b), "Optional") {
			t.Fatalf("state %s with no Optional edge serializes it: %s", sv.Name, b)
		}
	}
	if plain.Hash() == optional.Hash() {
		t.Fatal("Optional must change the definition hash")
	}
}

// C2: OnFailure adds a labelled default edge to every state without its own
// route, skips the target and terminals, and shows in the graph.
func TestOnFailureDefaultEdges(t *testing.T) {
	d := wf.New("defaults")
	a := wf.NewAgent[eA, eOut](d)
	b := wf.NewAgent[edChk, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(1), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "own blocked")))
	b.Spec(wf.PromptText("p"), wf.Retry[edChk](wf.Const(1), b.To(bad, "gave up")))
	ab, bd := a.To(b, "next"), b.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return ab }, ab)
	b.Route(func(eOut, wf.Ctx) wf.Edge[edChk] { return bd }, bd)
	d.OnFailure(wf.Blocked, bad)
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint = %v, want clean", ps)
	}
	labels := map[string][]string{}
	for _, sv := range wf.Inspect(d).States {
		for _, e := range sv.Edges {
			labels[sv.Name] = append(labels[sv.Name], e.Label)
		}
	}
	if got := strings.Join(labels["eA"], ","); strings.Contains(got, "default") {
		t.Fatalf("eA edges = %s, want no default: it routes Blocked itself", got)
	}
	if got := strings.Join(labels["edChk"], ","); !strings.Contains(got, "default blocked route") {
		t.Fatalf("edChk edges = %s, want the default blocked route", got)
	}
	if got := strings.Join(labels["eBad"], ","); got != "" {
		t.Fatalf("terminal edges = %s, want none", got)
	}
	d.OnFailure(wf.Stale, bad)
	if ps := d.Lint(); len(ps) == 0 || ps[0].Rule != wf.RuleBuild {
		t.Fatalf("lint = %v, want OnFailure after use to be a build problem", ps)
	}
}

func TestAgentBlockedRouteLint(t *testing.T) {
	d := wf.New("noblocked")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(1), a.To(bad, "gave up")))
	ad := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return ad }, ad)
	var found bool
	for _, p := range d.Lint() {
		found = found || p.Rule == wf.RuleAgentBlockedRoute
	}
	if !found {
		t.Fatal("an agent with no Blocked route must fail lint rule agent-blocked-route")
	}
}

// A failure route whose own effect fails halts instead of deciding again
// (which would take the same edge and fail forever).
func TestFailureRouteEffectFailureHalts(t *testing.T) {
	d := wf.New("route-loop")
	c := wf.NewCompute[edPlain, edRes](d, func(wf.Ctx) (edRes, error) { return edRes{}, nil })
	done, bad := twoTerminals(d)
	next := c.To(done, "go").With(shellEffect(""))
	c.Route(func(edRes, wf.Ctx) wf.Edge[edPlain] { return next }, next)
	c.Retry(wf.Retry[edPlain](wf.Const(1), c.To(bad, "gave up")).OnClass(wf.Blocked, c.To(bad, "blocked").With(shellEffect(""))))
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 1}, {Exit: 1}, {Exit: 1}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.next(run)
	st := h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "the failure route's edge effect") {
		t.Fatalf("status = %+v, want a halt naming the failure route's failed effect", st)
	}
	if w.appliedCount() != 2 {
		t.Fatalf("applied = %d, want 2: the edge, then its failure route once", w.appliedCount())
	}
}

// An unavailable edge effect at a state with no Unavailable route takes its
// Blocked route, with the effect as the cause.
//
// B1: before that, the outage is retried: the edge's effect is applied again
// on each delay of the default Unavailable schedule, with no attempt spent,
// and an outage that clears in time takes the edge.
func TestUnavailableEdgeEffectFallsBackToBlocked(t *testing.T) {
	d := edgeDef(false, mergeEffect(nil))
	w := newWorld()
	w.applyErr = wf.ErrUnavailable
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	if n := len(h.kind(run, wf.JournalBackoffScheduled)); n != 1 || len(h.kind(run, wf.JournalRefused)) != 0 {
		t.Fatalf("backoffs %d refused %d, want the first backoff and no failure yet", n, len(h.kind(run, wf.JournalRefused)))
	}
	passBackoff(h, run)
	if got := refusedClasses(t, h, run); len(got) != 1 || got[0] != wf.Blocked {
		t.Fatalf("refused classes = %v, want one Blocked", got)
	}
	if n := w.appliedCount(); n != 1+len(wf.DefaultUnavailableBackoff) {
		t.Fatalf("applied = %d, want the effect once and again after each delay", n)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")

	t.Run("an outage that clears takes the edge", func(t *testing.T) {
		w := newWorld()
		w.applyErr = wf.ErrUnavailable
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.submit(run, h.next(run)[0], `{}`)
		w.applyErr = nil
		h.now = h.now.Add(time.Hour)
		h.next(run)
		if !hasTransitionTo(h, run, "edChk") || len(h.kind(run, wf.JournalRefused)) != 0 {
			t.Fatal("the retried edge was not taken once the outage cleared")
		}
	})
}

// passBackoff walks the clock through every backoff delay the run waits on.
func passBackoff(h *harness, run wf.RunID) {
	for i := 0; i <= len(wf.DefaultUnavailableBackoff); i++ {
		h.now = h.now.Add(time.Hour)
		h.next(run)
	}
}

// An Optional step whose failure skipped a later step's effect, which is not
// Optional, stops the edge: the required effect never ran.
func TestOptionalFailureSkippingRequiredEffectStops(t *testing.T) {
	d := wf.New("optional-skip")
	a := wf.NewAgent[eA, eOut](d)
	b := wf.NewCompute[edPlain, edRes](d, func(wf.Ctx) (edRes, error) { return edRes{}, nil })
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(2), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(bad, "edge blocked")))
	next := a.To(b, "go").With(shellEffect("")).Optional()
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	wf.MaxVisits(wf.Const(0), b.To(done, "over").With(mergeEffect(nil)))
	toDone := b.To(done, "done")
	b.Route(func(edRes, wf.Ctx) wf.Edge[edPlain] { return toDone }, toDone)
	w := newWorld()
	w.runs = []wf.RunResult{{Exit: 1}}
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.drive(run, map[string][]string{"eA": {`{}`}})
	if hasTransitionTo(h, run, "eDone") {
		t.Fatal("the edge was taken although a required effect was skipped")
	}
	if r := h.kind(run, wf.JournalRefused); len(r) != 1 || !strings.Contains(string(r[0].Data), "was skipped after it") {
		t.Fatalf("refused = %v, want the skipped required effect named", r)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
}
