package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// segChild is a child of harness h: its own session (Hermes) or its own
// thread under the coordinator's session (Codex, Claude), from its
// environment.
func segChild(h, coordinator, agent string) hn.Caller {
	c := hn.Caller{Harness: h, Session: coordinator, Agent: agent, Is: hn.IsChild, FromEnv: true}
	if h == "hermes" {
		c.Session, c.Agent = agent, ""
	}
	return c
}

// runUnit hands one unit out to a coordinator of harness h and lets a child of
// h fetch and finish it.
func runUnit(t *testing.T, e *wf.Engine, run wf.RunID, h, session string) wf.Instruction {
	t.Helper()
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: h, Session: session})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("%s hand-out = %+v, %v", h, ins, err)
	}
	u := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild(h, session, "kid-"+u.Name)); err != nil {
		t.Fatalf("%s child of %s: %v", h, u.Name, err)
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatalf("%s child's done of %s: %v", h, u.Name, err)
	}
	return u
}

// an earlier revision, the user's harness model: a run moves from one harness to another
// through stop and resume, each a recorded boundary whose first coordinator
// binds the new segment; inside a segment another harness is refused, its
// text naming none. Hermes, then Codex, then Claude, and the run finishes;
// status shows the three segments.
func TestHarnessSegmentsMigrateThroughStopAndResume(t *testing.T) {
	d := fanDef("seg-migrate", chItems(1, 2, 3), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	runUnit(t, e, run, "hermes", "h-coord")
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c-coord"}); !errors.Is(err, wf.ErrRejected) || !refusedAsMismatch(err.Error()) {
		t.Fatalf("a Codex coordinator inside the Hermes segment = %v, want refused", err)
	}
	for _, next := range []struct{ h, session string }{{"codex", "c-coord"}, {"claude", "cl-coord"}} {
		if err := e.Stop(run, "move to "+next.h); err != nil {
			t.Fatal(err)
		}
		if err := e.Resume(run); err != nil {
			t.Fatal(err)
		}
		runUnit(t, e, run, next.h, next.session)
	}
	end, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "cl-coord"})
	if err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("after three units = %+v, %v; want the run ended", end, err)
	}
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range st.Segments {
		got = append(got, s.Harness+"/"+s.Cause)
		if s.Units != 1 {
			t.Errorf("segment %+v ran %d units, want 1", s, s.Units)
		}
	}
	if strings.Join(got, " ") != "hermes/start codex/resume claude/resume" || st.Segments[0].End == 0 || st.Segments[2].End != 0 {
		t.Fatalf("segments = %+v", st.Segments)
	}
	if line := wf.RenderStatus(st); !strings.Contains(line, "harness segments: hermes 1-") || !strings.Contains(line, "claude ") {
		t.Errorf("status does not show the segments:\n%s", line)
	}
}

// an earlier revision: when a stop and resume opens a new segment, a child its unit's
// earlier-segment hand-out claimed finishes under its own harness; a unit
// that segment offered and no child claimed is offered again by the new
// coordinator, and the old offer's late child is refused as stale.
func TestPreviousSegmentChildrenAcrossAResume(t *testing.T) {
	d := fanDef("seg-children", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	claimed, offered := agentsOf(ins)[0], agentsOf(ins)[1]
	if _, err := e.PromptFetchAs(run, claimed.Name, claimToken(t, claimed), segChild("hermes", "h-coord", "kid-a")); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	again, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "c-coord"})
	if err != nil || len(agentsOf(again)) != 1 || agentsOf(again)[0].Name != offered.Name {
		t.Fatalf("the Codex segment's offer = %+v, %v; want %s again", again, err, offered.Name)
	}
	if _, err := e.Done(run, claimed.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatalf("the Hermes segment's claimed child could not finish: %v", err)
	}
	if _, err := e.PromptFetchAs(run, offered.Name, claimToken(t, offered), segChild("hermes", "h-coord", "kid-late")); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "this offer expired when the run was stopped and resumed; exit without working") {
		t.Fatalf("the old offer's late Hermes child = %v, want refused as stale", err)
	}
	re := agentsOf(again)[0]
	if _, err := e.PromptFetchAs(run, re.Name, claimToken(t, re), segChild("codex", "c-coord", "kid-b")); err != nil {
		t.Fatalf("the Codex segment's child was refused: %v", err)
	}
	st, _ := e.Status(run)
	if len(st.Segments) != 2 || st.Segments[0].Units != 2 || st.Segments[1].Units != 1 {
		t.Fatalf("segments = %+v; want the Hermes one with both units and the Codex one with the re-offer", st.Segments)
	}
}

// an earlier revision, the workflow run blocker 1: a coordinator session known from an earlier
// segment still binds the new one, so the segment is never left unbound for
// another harness to take.
func TestASameSessionCoordinatorBindsTheNewSegment(t *testing.T) {
	d := fanDef("seg-same", chItems(1, 2), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	runUnit(t, e, run, "hermes", "same")
	if err := e.Stop(run, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOutAs(run, 0, wf.Coordinator{Harness: "hermes", Session: "same"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := wf.Driver(e, run); got != "hermes" {
		t.Fatalf("after the same session's next the new segment is bound to %q, want hermes", got)
	}
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "other"}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a Codex coordinator in the re-bound segment = %v, want refused", err)
	}
}

// an earlier revision, the workflow run blocker 2: the binding is set and checked under the
// hand-out's own lock, so of two first callers of an unbound segment the
// second, of another harness, is refused there, not only in the CLI's
// preflight, and hands out nothing.
func TestTheSecondFirstCallerIsRefusedUnderTheLock(t *testing.T) {
	d := fanDef("seg-race", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "first"}); err != nil {
		t.Fatal(err)
	}
	handed := len(h.kind(run, wf.JournalHandedOut))
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "second"}); !errors.Is(err, wf.ErrRejected) || !refusedAsMismatch(err.Error()) {
		t.Fatalf("the second first caller = %v, want refused against the first's binding", err)
	}
	if n := len(h.kind(run, wf.JournalHandedOut)); n != handed {
		t.Fatalf("the refused caller handed out %d units", n-handed)
	}
}

// an earlier revision: rewind, then another harness drives the fork to its end: the
// rewound record opens a segment the fork's first coordinator binds.
func TestRewindThenAnotherHarnessFinishes(t *testing.T) {
	refuse := true
	d := rewindDef(&refuse)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild("hermes", "h-coord", "kid")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if end, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"}); err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("source = %+v, %v; want ended", end, err)
	}
	rep, err := e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	refuse = false
	end, err := e.HandOutAs(rep.Run, 1, wf.Coordinator{Harness: "codex", Session: "c-coord"})
	if err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("the Codex-driven fork = %+v, %v; want it ended", end, err)
	}
	if got, _ := wf.Driver(e, rep.Run); got != "codex" {
		t.Fatalf("the fork is bound to %q, want codex", got)
	}
	st, _ := e.Status(rep.Run)
	if len(st.Segments) != 2 || st.Segments[0].Harness != "hermes" || st.Segments[1].Harness != "codex" || st.Segments[1].Cause != wf.SegmentRewind {
		t.Fatalf("fork segments = %+v", st.Segments)
	}
	if got, _ := wf.Driver(e, run); got != "hermes" {
		t.Fatalf("the source's binding changed to %q", got)
	}
}

// an earlier revision, the workflow run blocker 3: an occupied run id of the fork form whose
// records merely match the fork's prefix (here one run_started) is another
// run's: rewind skips it and leaves it as it was, rather than finishing it
// as an interrupted fork and appending a rewound record that would unbind it.
func TestRewindSkipsAnOccupiedUnrelatedRunID(t *testing.T) {
	refuse := true
	d := rewindDef(&refuse)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"rwAsk": {`{}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("source = %+v, want ended", st)
	}
	occupied := wf.RunID(string(run) + ".rw1")
	first := h.records(run)[0]
	first.Run = occupied
	unlock, err := h.j.Lock(occupied)
	if err != nil {
		t.Fatal(err)
	}
	err = h.j.Append(first)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	rep, err := h.e.(*wf.Engine).Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run == occupied {
		t.Fatalf("rewind finished the occupied %s as its fork", occupied)
	}
	if recs := h.records(occupied); len(recs) != 1 {
		t.Fatalf("the occupied run gained %d records", len(recs)-1)
	}
}

// an earlier revision review: next --adopt reports what it did, refuses an ended run, and
// is not combined with --until-unit.
func TestNextAdoptReportsAndRefuses(t *testing.T) {
	v1 := sizedAgentDef("adopt-report", "Old prompt.")
	state := startPlain(t, v1, "codex")
	if code, out, er := singleCLI(v1, "wf", "next", "--adopt", "--state-dir", state); code != 0 || !strings.HasPrefix(out, "nothing to adopt: ") {
		t.Fatalf("next --adopt on a current run = exit %d %q %q", code, out, er)
	}
	if code, _, er := singleCLI(v1, "wf", "next", "--adopt", "--until-unit", "x", "--state-dir", state); code != 2 || !strings.Contains(er, "--adopt only adopts") {
		t.Fatalf("next --adopt --until-unit = exit %d %q", code, er)
	}

	refuse := true
	d := rewindDef(&refuse)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if st := h.drive(run, map[string][]string{"rwAsk": {`{}`}}); st.Phase != wf.RunEnded {
		t.Fatalf("run = %+v, want ended", st)
	}
	if _, _, err := h.e.(*wf.Engine).AdoptOnly(run); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "never adopted") {
		t.Fatalf("adopt-only on an ended run = %v, want refused", err)
	}
}

// an earlier revision review P1-1: a resume is a migration, not a crash restart. A
// Codex child an earlier segment's coordinator spawned is not lost when the
// new segment's first coordinator comes (stop and resume does not establish
// that the old coordinator, and its children, exited): it finishes.
func TestAResumeKeepsTheOldSegmentsLiveChildren(t *testing.T) {
	d := fanDef("seg-keep", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "c-coord"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild("codex", "c-coord", "kid")); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "cl-coord"}); err != nil {
		t.Fatal(err)
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Fatalf("the new segment's coordinator lost the old segment's live child: %d attempt_lost", len(lost))
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatalf("the old segment's Codex child could not finish: %v", err)
	}
}

// an earlier revision review P1-2: an unclaimed offer of an earlier segment expires at the
// boundary. Before any re-offer its late child is refused; the new segment's
// coordinator, even the same session returning, is offered the unit again;
// the old token stays refused and the new one claims it.
func TestAnEarlierSegmentsOfferExpiresAtTheBoundary(t *testing.T) {
	d := fanDef("seg-expire", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	old := agentsOf(ins)[0]
	if err := e.Stop(run, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchAs(run, old.Name, claimToken(t, old), segChild("hermes", "h-coord", "late")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("an earlier segment's offer claimed after the boundary = %v, want refused", err)
	}
	again, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(again)) != 1 || agentsOf(again)[0].Name != old.Name {
		t.Fatalf("the returning session's next = %+v, %v; want %s offered again", again, err, old.Name)
	}
	if _, err := e.PromptFetchAs(run, old.Name, claimToken(t, old), segChild("hermes", "h-coord", "late")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the old token after the re-offer = %v, want refused", err)
	}
	re := agentsOf(again)[0]
	if _, err := e.PromptFetchAs(run, re.Name, claimToken(t, re), segChild("hermes", "h-coord", "kid")); err != nil {
		t.Fatalf("the new segment's offer = %v", err)
	}
}

// On a hand-out a coordinator of an identifying harness made with a session,
// a caller whose environment identifies another harness (here none) and
// holds the unit's token is served, and the journal notes the mismatch.
func TestAnUnidentifiedCallerOfAnIdentifiedHandOutIsNoted(t *testing.T) {
	root := t.TempDir()
	d := fanDef("seg-unidentified", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord", Root: root})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	u := agentsOf(ins)[0]
	log := filepath.Join(root, "cache", "delegation", "live", "deleg_1", "task-0.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("=== Hermes subagent live transcript ===\ngoal: Run: super wf prompt --claim "+claimToken(t, u)+", then do what it prints\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	generic := hn.Caller{Harness: "generic", Session: "x", Root: root, FromEnv: true}
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), generic); err != nil {
		t.Fatalf("an unidentified caller of a Hermes hand-out holding its token = %v, want served", err)
	}
	notes := h.kind(run, wf.JournalDegraded)
	if len(notes) != 1 || !strings.Contains(string(notes[0].Data), string(wf.DegradedChildCaller)) || !strings.Contains(string(notes[0].Data), "is not the one its unit was handed out in") {
		t.Fatalf("degraded records %+v, want one harness mismatch note", notes)
	}
}

// an earlier revision review P2-5: a journal from before segments, where a known session
// resumed with no new coordinator record, binds its post-resume segment from
// its first session-bearing hand-out, so another harness is still refused.
func TestALegacyResumedSegmentBindsFromItsHandOut(t *testing.T) {
	d := fanDef("seg-legacy", chItems(1, 2), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	runUnit(t, e, run, "hermes", "h-coord")
	if err := e.Stop(run, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"}); err != nil {
		t.Fatal(err)
	}
	// The same journal as an older engine wrote it: no segment fields, and
	// no coordinator record for the known session after the resume.
	legacy := wf.RunID(string(run) + "-legacy")
	unlock, err := h.j.Lock(legacy)
	if err != nil {
		t.Fatal(err)
	}
	coordinators := 0
	for _, r := range h.records(run) {
		var data map[string]any
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		switch r.Kind {
		case wf.JournalCoordinator:
			if coordinators++; coordinators > 1 {
				continue
			}
			delete(data, "SegmentStart")
		case wf.JournalHandedOut:
			delete(data, "Segment")
		}
		r.Data, _ = json.Marshal(data)
		r.Run, r.Seq = legacy, 0
		if err := h.j.Append(r); err != nil {
			unlock()
			t.Fatal(err)
		}
	}
	unlock()
	if got, err := wf.Driver(e, legacy); err != nil || got != "hermes" {
		t.Fatalf("the legacy journal's resumed segment is bound to %q, %v; want hermes", got, err)
	}
	if _, err := e.HandOutAs(legacy, 1, wf.Coordinator{Harness: "codex", Session: "c-coord"}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("a Codex coordinator on the legacy journal = %v, want refused", err)
	}
}

// an earlier revision review round 3: a unit a child claimed before the boundary with a
// started (claimed, not yet fetched) is that child's: its first fetch after
// the resume, with its original offer's token, is accepted. A started that
// would newly claim an earlier segment's unclaimed offer is refused.
func TestAStartedClaimCrossesTheBoundary(t *testing.T) {
	d := fanDef("seg-started", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "c-coord"})
	if err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	owned, stale := agentsOf(ins)[0], agentsOf(ins)[1]
	if err := e.Started(run, owned.Name, wf.Claim{Harness: "codex", Session: "c-coord", Agent: "kid"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(run, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchAs(run, owned.Name, claimToken(t, owned), segChild("codex", "c-coord", "kid")); err != nil {
		t.Fatalf("the pre-boundary started child's first fetch = %v, want accepted", err)
	}
	if err := e.Started(run, stale.Name, wf.Claim{Harness: "codex", Session: "c-coord", Agent: "late"}); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a started on an earlier segment's unclaimed offer = %v, want refused as expired", err)
	}
}

// an earlier revision review residual: a unit re-offered in the new segment and started
// there is claimed under that hand-out; an earlier segment's token, still in
// its offers, is refused at the first fetch, and the new offer's token works.
func TestAnExpiredTokenCannotFetchAUnitStartedInTheNewSegment(t *testing.T) {
	d := fanDef("seg-restarted", chItems(1), 1, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("hand-out %+v, %v", ins, err)
	}
	old := agentsOf(ins)[0]
	if err := e.Stop(run, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	again, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "hermes", Session: "h-coord"})
	if err != nil || len(agentsOf(again)) != 1 {
		t.Fatalf("re-offer %+v, %v", again, err)
	}
	re := agentsOf(again)[0]
	if err := e.Started(run, re.Name, wf.Claim{Harness: "hermes", Session: "kid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PromptFetchAs(run, old.Name, claimToken(t, old), segChild("hermes", "h-coord", "kid")); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the first segment's token on a unit started in the second = %v, want refused", err)
	}
	if _, err := e.PromptFetchAs(run, re.Name, claimToken(t, re), segChild("hermes", "h-coord", "kid")); err != nil {
		t.Fatalf("the second segment's token = %v, want accepted", err)
	}
}
