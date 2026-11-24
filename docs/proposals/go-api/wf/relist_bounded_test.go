package wf_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// neverStartedNotes counts run's degraded records of kind never started.
func neverStartedNotes(t *testing.T, h *harness, run wf.RunID) int {
	t.Helper()
	n := 0
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var d wf.DegradedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Kind == wf.DegradedUnstarted {
			n++
		}
	}
	return n
}

// A spawn that never starts does not grow the journal: with the engine clock
// held still, one unit, one session and no fetch, 32 whole waits show the
// coordinator the unit 32 times and write one hand-out and one never-started
// note. Every wait offers the same claim token, and the first child, late,
// still binds the unit with it.
func TestRepeatedRelistWritesNothingNew(t *testing.T) {
	d := fanDef("relist-bounded", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")
	token := claimToken(t, first[0])
	for i := 1; i <= 32; i++ {
		res, err := e.NextWait(run, 1, co, time.Millisecond)
		again := agentsOf(res.Instructions)
		if err != nil || !res.Relisted || len(again) != 1 || again[0].Name != first[0].Name || !again[0].NotStarted {
			t.Fatalf("wait %d = %+v, %v; want %s listed again as never started", i, res, err, first[0].Name)
		}
		if got := claimToken(t, again[0]); got != token {
			t.Fatalf("wait %d offered claim %s, want the first offer's %s again", i, got, token)
		}
		for _, in := range res.Instructions {
			if in.Kind == wf.InstructionStopped || in.Kind == wf.InstructionEnded {
				t.Fatalf("wait %d told the session to end: %+v", i, in)
			}
		}
	}
	if handed, notes := len(h.kind(run, wf.JournalHandedOut)), neverStartedNotes(t, h, run); handed != 1 || notes != 1 {
		t.Fatalf("32 whole waits with nothing new journaled %d hand-outs and %d never-started notes, want 1 and 1", handed, notes)
	}
	before := len(h.records(run))
	if res, err := e.NextWait(run, 1, co, time.Millisecond); err != nil || !res.Relisted {
		t.Fatalf("one more wait = %+v, %v", res, err)
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("one more wait with nothing new wrote %d record(s)", n-before)
	}
	fetchAs(t, h, run, first[0], codexChild("boss", "late"))
	if _, err := e.Done(run, first[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	statusOf(t, h.status(run), wf.RunEnded, "chMerged")
}

// Once the grace passes on the engine clock, the unit gets a fresh offer: a
// new hand-out under a new claim token, journaled once, and the waits after
// it repeat that one.
func TestRelistJournalsAFreshOfferAfterTheGrace(t *testing.T) {
	d := fanDef("relist-grace", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")
	for i := 0; i < 4; i++ {
		if res, err := e.NextWait(run, 1, co, time.Millisecond); err != nil || !res.Relisted {
			t.Fatalf("wait %d = %+v, %v", i, res, err)
		}
	}
	h.now = h.now.Add(5 * time.Minute) // UnstartedGrace
	res, err := e.NextWait(run, 1, co, time.Second)
	fresh := agentsOf(res.Instructions)
	if err != nil || len(fresh) != 1 || !fresh[0].NotStarted || claimToken(t, fresh[0]) == claimToken(t, first[0]) {
		t.Fatalf("after the grace next = %+v, %v; want a fresh offer under a new claim", res, err)
	}
	if handed := len(h.kind(run, wf.JournalHandedOut)); handed != 2 {
		t.Fatalf("%d hand-outs after the grace, want 2", handed)
	}
	for i := 0; i < 8; i++ {
		res, err := e.NextWait(run, 1, co, time.Millisecond)
		again := agentsOf(res.Instructions)
		if err != nil || !res.Relisted || len(again) != 1 || claimToken(t, again[0]) != claimToken(t, fresh[0]) {
			t.Fatalf("wait %d after the fresh offer = %+v, %v; want that offer again", i, res, err)
		}
	}
	if handed := len(h.kind(run, wf.JournalHandedOut)); handed != 2 {
		t.Fatalf("%d hand-outs after 8 more waits, want still 2", handed)
	}
	// A new coordinator session is offered the unit afresh, journaled.
	other := handOutAs(t, h, run, 1, "boss-2")
	if len(other) != 1 || claimToken(t, other[0]) == claimToken(t, fresh[0]) || len(h.kind(run, wf.JournalHandedOut)) != 3 {
		t.Fatalf("a new session = %+v with %d hand-outs; want a fresh journaled offer", other, len(h.kind(run, wf.JournalHandedOut)))
	}
}

// What the spawn printed is evidence: when the stderr file of the offer
// changes between two waits, the next re-list journals one never-started
// note with the new lines, still under the same claim; unchanged, it writes
// nothing.
func TestRelistJournalsNewSpawnEvidence(t *testing.T) {
	state := t.TempDir() + "/state"
	h, run := errLogHarness(t, state)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "pi", Session: "coord"}
	first := quoteHandOut(t, h, run, "pi")
	if len(first) != 1 || first[0].ErrLog == "" {
		t.Fatalf("first offer = %+v", first)
	}
	wait := func(what string) wf.Instruction {
		t.Helper()
		res, err := e.NextWait(run, 1, co, time.Millisecond)
		again := agentsOf(res.Instructions)
		if err != nil || !res.Relisted || len(again) != 1 || claimToken(t, again[0]) != claimToken(t, first[0]) || again[0].ErrLog != first[0].ErrLog {
			t.Fatalf("%s: next = %+v, %v; want the first offer again", what, res, err)
		}
		return again[0]
	}
	wait("no stderr yet")
	wait("no stderr yet, again")
	if n := neverStartedNotes(t, h, run); n != 1 {
		t.Fatalf("%d never-started notes with no stderr, want 1", n)
	}
	if err := os.WriteFile(first[0].ErrLog, []byte("pi: unknown model m1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if in := wait("the spawn printed"); len(in.PrevErrTail) != 1 || in.PrevErrTail[0] != "pi: unknown model m1" {
		t.Fatalf("the re-list does not show what the spawn printed: %+v", in.PrevErrTail)
	}
	wait("the same lines")
	wait("the same lines, again")
	if n := neverStartedNotes(t, h, run); n != 2 {
		t.Fatalf("%d never-started notes after the spawn printed once, want 2", n)
	}
	var last wf.DegradedData
	recs := h.kind(run, wf.JournalDegraded)
	if err := json.Unmarshal(recs[len(recs)-1].Data, &last); err != nil || !strings.Contains(last.Cause, "pi: unknown model m1") {
		t.Fatalf("the note does not carry the spawn's lines: %+v, %v", last, err)
	}
	if err := os.WriteFile(first[0].ErrLog, []byte("pi: no API key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wait("other lines")
	wait("other lines, again")
	if handed, notes := len(h.kind(run, wf.JournalHandedOut)), neverStartedNotes(t, h, run); handed != 1 || notes != 3 {
		t.Fatalf("%d hand-outs and %d never-started notes, want 1 and 3", handed, notes)
	}
}

// What next prints for a unit shown again matches what the engine can do
// about a second child. A re-list repeats the offer under the same claim, so
// two children of it would both work under one claim and neither is told to
// stop: the unit's own line says to start its child again only if the
// earlier start is no longer running, and makes no promise. A fresh offer
// after the grace is under a new claim, where the engine does tell the
// second child the unit is taken: its line keeps that promise. The JSON
// form carries the distinction as sameClaim.
func TestRelistedUnitTextMatchesItsClaim(t *testing.T) {
	const promise = "one of the two is told the unit is taken and stops"
	const conditional = "only if its earlier start is no longer running"
	d := fanDef("relist-text", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Harness: "codex", Session: "boss"}
	first := handOutAs(t, h, run, 1, "boss")

	res, err := e.NextWait(run, 1, co, time.Millisecond)
	same := agentsOf(res.Instructions)
	if err != nil || !res.Relisted || len(same) != 1 || claimToken(t, same[0]) != claimToken(t, first[0]) {
		t.Fatalf("the re-list = %+v, %v; want the first offer again", res, err)
	}
	out := wf.RenderNext(res, "codex", plainVerb)
	_, unitText, _ := strings.Cut(out, wf.NextRelisted) // below the heading: the unit's own lines
	if strings.Contains(out, promise) || !strings.Contains(unitText, first[0].Name) || !strings.Contains(unitText, conditional) || !strings.Contains(unitText, "under the same claim") {
		t.Errorf("a re-presented offer promises what the engine cannot do, or does not say when to start its child:\n%s", out)
	}
	if strings.Contains(unitText, "start its child again (") {
		t.Errorf("a re-presented offer says to start its child again unconditionally:\n%s", out)
	}
	if raw, err := json.Marshal(same[0]); err != nil || !strings.Contains(string(raw), `"sameClaim":true`) || !strings.Contains(string(raw), `"notStarted":true`) {
		t.Errorf("the JSON form of a re-presented offer = %s, %v; want notStarted and sameClaim", raw, err)
	}

	h.now = h.now.Add(5 * time.Minute) // UnstartedGrace: a fresh offer, a new claim
	res, err = e.NextWait(run, 1, co, time.Second)
	fresh := agentsOf(res.Instructions)
	if err != nil || len(fresh) != 1 || !fresh[0].NotStarted || claimToken(t, fresh[0]) == claimToken(t, first[0]) {
		t.Fatalf("after the grace next = %+v, %v; want a fresh offer under a new claim", res, err)
	}
	out = wf.RenderNext(res, "codex", plainVerb)
	if !strings.Contains(out, promise) || !strings.Contains(out, "start its child again (") || strings.Contains(out, conditional) {
		t.Errorf("a fresh re-offer does not keep the losing-claim promise:\n%s", out)
	}
	if raw, err := json.Marshal(fresh[0]); err != nil || strings.Contains(string(raw), "sameClaim") {
		t.Errorf("the JSON form of a fresh re-offer = %s, %v; want no sameClaim", raw, err)
	}
	// The promise holds: the second child of the two offers is told.
	fetchAs(t, h, run, fresh[0], codexChild("boss", "k1"))
	if _, err := e.PromptFetchAs(run, first[0].Name, claimToken(t, first[0]), codexChild("boss", "k2")); err == nil || !strings.Contains(err.Error(), "another child already owns this unit") {
		t.Errorf("the second child of a fresh re-offer = %v, want told the unit is taken", err)
	}
}
