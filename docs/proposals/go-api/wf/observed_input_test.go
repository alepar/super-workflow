package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type obsTicket struct {
	Title  string
	Labels []string
}

func renderTicket(t obsTicket) (string, error) {
	if t.Title == "" {
		return "", errors.New("the ticket has no title")
	}
	return t.Title + " [" + strings.Join(t.Labels, ",") + "]", nil
}

var ticketArgs = map[string]any{"id": "T-1"}

// observedDef is one agent whose prompt binds {{TICKET}} to an observed
// tracker read keyed by the run's id param. Without observed, the prompt is
// the same state with no observed input (a definition a run adopts from).
func observedDef(observed bool, render func(obsTicket) (string, error)) *wf.Def {
	d := wf.New("observed-input")
	d.Param("id")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	d.OnFailure(wf.Blocked, bad)
	retry := wf.Retry[eA](wf.Const(2), a.To(bad, "x")).NoRecovery()
	if observed {
		obs := wf.Observe[obsTicket]("tracker", "show", map[string]any{"id": "{id}"})
		a.Spec(wf.PromptText("Work on {{TICKET}}.", wf.ObservedAs("TICKET", obs, render)), retry)
	} else {
		a.Spec(wf.PromptText("Work on the ticket."), retry)
	}
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	return d
}

// inputReads is the observed records the input NAME journaled.
func inputReads(t *testing.T, h *harness, run wf.RunID, name string) []wf.JournalRecord {
	t.Helper()
	var out []wf.JournalRecord
	for _, r := range h.kind(run, wf.JournalObserved) {
		var od wf.ObservedData
		if err := json.Unmarshal(r.Data, &od); err != nil {
			t.Fatal(err)
		}
		if od.Purpose == "input:"+name {
			out = append(out, r)
		}
	}
	return out
}

// The read is bound in the agent's scope, journaled before the instruction
// is issued and rendered into the prompt; a refetch, and a fresh process
// replaying the journal, render the journaled value, not the moved world.
func TestObservedInputRendersTheJournaledRead(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Fix login", Labels: []string{"bug"}})
	d := observedDef(true, renderTicket)
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"id": "T-1"})
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("handed out %+v", ins)
	}
	text, err := h.e.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, "Work on Fix login [bug].") {
		t.Fatalf("prompt = %v %q, want the rendered ticket", err, text)
	}
	reads := inputReads(t, h, run, "TICKET")
	issued := h.kind(run, wf.JournalInstructionIssued)
	if len(reads) != 1 || len(issued) != 1 || reads[0].Seq > issued[0].Seq {
		t.Fatalf("input reads %+v, issued %+v: want one read journaled before the issue", reads, issued)
	}
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Renamed", Labels: []string{"bug"}})
	text, err = h.e.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, "Work on Fix login [bug].") {
		t.Fatalf("refetch = %v %q, want the journaled ticket", err, text)
	}
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	text, err = fresh.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, "Work on Fix login [bug].") {
		t.Fatalf("fresh process fetch = %v %q, want the journaled ticket", err, text)
	}
	if n := len(inputReads(t, h, run, "TICKET")); n != 1 {
		t.Fatalf("%d input reads after refetches, want the one at issue", n)
	}
}

// The observed input is part of the staleness token: a change under a live
// attempt re-issues the step, and the re-issue renders the new value.
func TestObservedInputJoinsTheStalenessToken(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Fix login"})
	d := observedDef(true, renderTicket)
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"id": "T-1"})
	in := h.next(run)[0]
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Fix logout"})
	if res := h.submit(run, in, `{}`); res.Class != wf.Stale {
		t.Fatalf("submit after the ticket changed = %+v, want stale", res)
	}
	again := h.next(run)
	if len(again) != 1 || !strings.Contains(again[0].Prompt, "Work on Fix logout [].") {
		t.Fatalf("re-issue = %+v, want the new ticket rendered", again)
	}
	if res := h.submit(run, again[0], `{}`); res.Class != wf.Accepted {
		t.Fatalf("re-issued attempt = %+v, want accepted", res)
	}
	for _, sv := range wf.Inspect(d).States {
		if sv.Name != "eA" {
			continue
		}
		if !slices.Contains(sv.Inputs, `observed TICKET = tracker.show id="{id}"`) {
			t.Fatalf("eA inputs %v, want the observed input", sv.Inputs)
		}
		if !slices.Contains(sv.Observes, `input tracker.show id="{id}"`) {
			t.Fatalf("eA observes %v, want the input observation", sv.Observes)
		}
	}
}

// An outage backs off with the reason, issuing nothing; a read that does not
// decode, or that the render refuses, blocks the state with the reason.
func TestObservedInputFailures(t *testing.T) {
	t.Run("unavailable backs off", func(t *testing.T) {
		rt := newFakeRuntime()
		rt.down["tracker.show"] = true
		d := observedDef(true, renderTicket)
		h := newHarness(t, d, rt)
		run := h.run(d, map[string]string{"id": "T-1"})
		for _, in := range h.next(run) {
			if in.Kind == wf.InstructionAgent {
				t.Fatalf("issued %+v during an outage", in)
			}
		}
		backoffs := h.kind(run, wf.JournalBackoffScheduled)
		if len(backoffs) == 0 || !strings.Contains(string(backoffs[0].Data), "input TICKET: adapter unavailable: tracker down") {
			t.Fatalf("backoffs %+v, want one naming the outage", backoffs)
		}
		if n := len(h.kind(run, wf.JournalInstructionIssued)); n != 0 {
			t.Fatalf("%d issued during an outage", n)
		}
	})
	for name, tc := range map[string]struct {
		value  any
		reason string
	}{
		"render refuses":        {obsTicket{}, "the ticket has no title"},
		"value does not decode": {"not a ticket", "TICKET"},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newFakeRuntime()
			rt.set("tracker", "show", ticketArgs, tc.value)
			d := observedDef(true, renderTicket)
			h := newHarness(t, d, rt)
			run := h.run(d, map[string]string{"id": "T-1"})
			h.next(run)
			statusOf(t, h.status(run), wf.RunEnded, "eBad")
			found := false
			for _, r := range h.records(run) {
				found = found || (r.Class == wf.Blocked && strings.Contains(string(r.Data), tc.reason))
			}
			if !found {
				t.Fatalf("no Blocked record naming %q", tc.reason)
			}
		})
	}
}

// A run adopted onto the definition while an attempt is open reads the
// input once, at the first render, journals it against the open issue, and
// renders that record from then on.
func TestObservedInputAdoptedWithAnOpenAttempt(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Fix login"})
	old := observedDef(false, nil)
	h := newHarness(t, old, rt)
	run := h.run(old, map[string]string{"id": "T-1"})
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("handed out %+v", ins)
	}
	adopted := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{observedDef(true, renderTicket)}, Adopt: true})
	if _, err := adopted.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	text, err := adopted.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, "Work on Fix login [].") {
		t.Fatalf("first render after adoption = %v %q", err, text)
	}
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "Fix logout"})
	text, err = adopted.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, "Work on Fix login [].") {
		t.Fatalf("second render after adoption = %v %q, want the journaled read", err, text)
	}
	reads := inputReads(t, h, run, "TICKET")
	issued := h.kind(run, wf.JournalInstructionIssued)
	if len(reads) != 1 || reads[0].Seq < issued[len(issued)-1].Seq {
		t.Fatalf("input reads %+v: want one, journaled after the open issue", reads)
	}
}

// The definition hash covers the observed input's descriptor.
func TestObservedInputIsHashed(t *testing.T) {
	mk := func(op string) *wf.Def {
		d := wf.New("observed-hash")
		a := wf.NewAgent[eA, eOut](d)
		done, bad := twoTerminals(d)
		obs := wf.Observe[obsTicket]("tracker", op, map[string]any{"id": "x"})
		a.Spec(wf.PromptText("{{T}}", wf.ObservedAs("T", obs, renderTicket)), wf.Retry[eA](wf.Const(1), a.To(bad, "x")))
		next := a.To(done, "done")
		a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
		return d
	}
	if mk("show").Hash() == mk("view").Hash() {
		t.Fatal("hash ignores the observed input's descriptor")
	}
}

// lastIssue is the token of the run's latest instruction_issued.
func lastIssue(t *testing.T, h *harness, run wf.RunID) string {
	t.Helper()
	issued := h.kind(run, wf.JournalInstructionIssued)
	var d wf.InstructionIssuedData
	if err := json.Unmarshal(issued[len(issued)-1].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Token
}

// adoptEngine is an engine over h's journal running d, adopting onto it.
func adoptEngine(h *harness, d *wf.Def) wf.Executor {
	return wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true, Clock: func() time.Time { return h.now }})
}

// An open attempt adopted onto the observed input takes the read at its
// first render as the staleness baseline: a change before the submission is
// stale, though the issue itself carried no token.
func TestObservedInputAdoptedBaselineIsTheStalenessToken(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "A"})
	old := observedDef(false, nil)
	h := newHarness(t, old, rt)
	run := h.run(old, map[string]string{"id": "T-1"})
	ins := h.handOut(run, 1)
	adopted := adoptEngine(h, observedDef(true, renderTicket))
	if _, err := adopted.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if text, err := adopted.Prompt(run, ins[0].Name); err != nil || !strings.Contains(text, "Work on A [].") {
		t.Fatalf("adopted render = %v %q", err, text)
	}
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "B"})
	res, err := adopted.Submit(run, wf.Submission{Token: lastIssue(t, h, run), Output: json.RawMessage(`{}`)})
	if err != nil || res.Class != wf.Stale {
		t.Fatalf("submit after the adopted input changed = %+v %v, want stale", res, err)
	}
}

// A read journaled for one issue never serves another: an attempt issued
// after the input was adopted away and back reads anew at its first render.
func TestObservedInputReadBelongsToItsIssue(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "A"})
	plain, observed := observedDef(false, nil), observedDef(true, renderTicket)
	h := newHarness(t, plain, rt)
	run := h.run(plain, map[string]string{"id": "T-1"})
	first := h.handOut(run, 1)
	in := adoptEngine(h, observed)
	if _, err := in.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if text, err := in.Prompt(run, first[0].Name); err != nil || !strings.Contains(text, "Work on A [].") {
		t.Fatalf("first issue render = %v %q", err, text)
	}
	away := adoptEngine(h, plain)
	if _, err := away.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	firstToken := lastIssue(t, h, run)
	if res, err := away.Submit(run, wf.Submission{Token: firstToken, Output: json.RawMessage(`"not an eOut"`)}); err != nil || res.Class == wf.Accepted {
		t.Fatalf("refusing submission = %+v %v", res, err)
	}
	if _, err := away.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if lastIssue(t, h, run) == firstToken {
		t.Fatal("no second issue")
	}
	handed := h.kind(run, wf.JournalHandedOut)
	var second wf.HandedOutData
	if err := json.Unmarshal(handed[len(handed)-1].Data, &second); err != nil || second.Name == first[0].Name {
		t.Fatalf("second unit = %+v %v", second, err)
	}
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "B"})
	back := adoptEngine(h, observed)
	if _, err := back.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if text, err := back.Prompt(run, second.Name); err != nil || !strings.Contains(text, "Work on B [].") {
		t.Fatalf("second issue render = %v %q, want its own read", err, text)
	}
	if n := len(inputReads(t, h, run, "TICKET")); n != 2 {
		t.Fatalf("%d input reads, want one per issue", n)
	}
}

// countingRuntime answers each tracker read with the next title: a world
// moving between any two reads.
type countingRuntime struct {
	mu    sync.Mutex
	reads int
}

func (*countingRuntime) Ops() []string { return []string{"tracker.show"} }
func (r *countingRuntime) Read(string, wf.ObservationDesc) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return json.Marshal(obsTicket{Title: fmt.Sprintf("v%d", r.reads)})
}
func (*countingRuntime) Apply(string, wf.ExternalEffect) (json.RawMessage, error) { return nil, nil }
func (*countingRuntime) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectDone, nil
}

// Two inputs over one bound descriptor share one read at issue: both render
// the same value, and the staleness token is that value.
func TestObservedInputsShareOneReadPerDescriptor(t *testing.T) {
	d := wf.New("observed-twice")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	d.OnFailure(wf.Blocked, bad)
	obs := wf.Observe[obsTicket]("tracker", "show", map[string]any{"id": "T-1"})
	title := func(t obsTicket) (string, error) { return t.Title, nil }
	a.Spec(wf.PromptText("first {{ONE}} second {{TWO}}", wf.ObservedAs("ONE", obs, title), wf.ObservedAs("TWO", obs, title)),
		wf.Retry[eA](wf.Const(2), a.To(bad, "x")).NoRecovery())
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	rt := &countingRuntime{}
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	in := h.next(run)[0]
	if !strings.Contains(in.Prompt, "first v1 second v1") {
		t.Fatalf("prompt %q, want both inputs from one read", in.Prompt)
	}
	if rt.reads != 1 {
		t.Fatalf("%d reads at issue, want one per descriptor", rt.reads)
	}
	if len(inputReads(t, h, run, "ONE")) != 1 || len(inputReads(t, h, run, "TWO")) != 1 {
		t.Fatal("each input name keeps its journaled read")
	}
	if res := h.submit(run, in, `{}`); res.Class != wf.Stale {
		t.Fatalf("submit = %+v, want stale against the shared read v1", res)
	}
}

// staleObservedDef is observedDef routing Stale to eBad, with extra
// declared input observations.
func staleObservedDef(extra ...wf.Reader) *wf.Def {
	d := wf.New("observed-stale")
	d.Param("id")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	d.OnFailure(wf.Blocked, bad)
	obs := wf.Observe[obsTicket]("tracker", "show", map[string]any{"id": "{id}"})
	a.Spec(wf.PromptText("Work on {{TICKET}}.", wf.ObservedAs("TICKET", obs, renderTicket)),
		wf.Retry[eA](wf.Const(2), a.To(bad, "x")).NoRecovery().OnClass(wf.Stale, a.To(bad, "stale")))
	a.Observes(extra...)
	next := a.To(done, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[eA] { return next }, next)
	return d
}

// Replaying a run whose observed input changed between issue and submission
// takes the route the run took: the issue's read is one adapter read, the
// staleness token's copy of it an alias that replay does not feed.
func TestObservedInputReplaysTheStaleRoute(t *testing.T) {
	rt := newFakeRuntime()
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "A"})
	d := staleObservedDef()
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"id": "T-1"})
	in := h.next(run)[0]
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "B"})
	if res := h.submit(run, in, `{}`); res.Class != wf.Stale {
		t.Fatalf("source submit = %+v, want stale", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eBad")
	rep, err := wf.Replay(h.records(run), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Differences) != 0 {
		t.Fatalf("replay differs: %+v (stop %+v)", rep.Differences, rep.Stop)
	}
}

// An ordinary input observation that errors (not unavailable) stays in the
// baseline as its error: a change of the observed input is still stale.
func TestObservedInputStaleBesideAnErroringObservation(t *testing.T) {
	rt := newFakeRuntime() // no scripted read for x.y: every read of it errors
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "A"})
	d := staleObservedDef(wf.Observe[eRes]("x", "y", map[string]any{"k": "v"}))
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"id": "T-1"})
	in := h.next(run)[0]
	rt.set("tracker", "show", ticketArgs, obsTicket{Title: "B"})
	if res := h.submit(run, in, `{}`); res.Class != wf.Stale {
		t.Fatalf("submit after the ticket changed = %+v, want stale", res)
	}
}
