package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A coordinator session that moves to another records root within the same
// harness (a Claude session moved to another profile: another
// CLAUDE_CONFIG_DIR) keeps driving the run. Its next is served, the journal
// notes the change once, and units handed out afterwards record the new
// root, where their children's records are. A journal written before the
// change folds as it did, and another harness is still refused.
func TestAChangedRootInTheSameHarnessIsJournaledNotRefused(t *testing.T) {
	d := fanDef("root-moved", chItems(1, 2, 3, 4), 4, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	next := func(e *wf.Engine, root string) wf.Instruction {
		t.Helper()
		ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "s", Root: root})
		if err != nil {
			t.Fatalf("next under %s: %v", root, err)
		}
		if len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
			t.Fatalf("next under %s handed out %+v, want one unit", root, ins)
		}
		return ins[0]
	}
	handedRoot := func(name string) string {
		t.Helper()
		root := "(not handed out)"
		for _, r := range h.kind(run, wf.JournalHandedOut) {
			var hd wf.HandedOutData
			if err := json.Unmarshal(r.Data, &hd); err != nil {
				t.Fatal(err)
			}
			if hd.Name == name {
				root = hd.Root
			}
		}
		return root
	}
	notes := func() (out []wf.DegradedData) {
		t.Helper()
		for _, r := range h.kind(run, wf.JournalDegraded) {
			var dg wf.DegradedData
			if err := json.Unmarshal(r.Data, &dg); err != nil {
				t.Fatal(err)
			}
			if dg.Kind == wf.DegradedRootChanged {
				out = append(out, dg)
			}
		}
		return out
	}

	first := next(e, "/profiles/a")
	if got := handedRoot(first.Name); got != "/profiles/a" || len(notes()) != 0 {
		t.Fatalf("before the move: unit root %q, notes %+v", got, notes())
	}

	// The journal so far is one an earlier binary wrote too. A fresh engine
	// folds it and serves the moved session.
	e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{d}}).(*wf.Engine)
	second := next(e, "/profiles/b")
	third := next(e, "/profiles/b")
	for _, in := range []wf.Instruction{second, third} {
		if got := handedRoot(in.Name); got != "/profiles/b" {
			t.Errorf("%s, handed out after the move, records root %q, want the new one", in.Name, got)
		}
	}
	got := notes()
	if len(got) != 1 || !strings.Contains(got[0].Cause, "changed from /profiles/a to /profiles/b") || !strings.Contains(got[0].Action, "the run goes on") {
		t.Fatalf("root change notes = %+v, want one naming both roots", got)
	}
	// The note is in the run's status after a fold of the whole journal.
	e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{d}}).(*wf.Engine)
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, dg := range st.Degraded {
		if dg.Kind == wf.DegradedRootChanged {
			listed++
		}
	}
	if listed != 1 {
		t.Errorf("status lists %d root change notes, want 1: %+v", listed, st.Degraded)
	}

	// Another harness is still refused, and journals nothing.
	before := len(h.records(run))
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "s2", Root: "/profiles/b"}); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), wf.HarnessMismatch) {
		t.Errorf("a next from another harness = %v, want the harness mismatch refusal", err)
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("the refused next appended %d records", after-before)
	}
	next(e, "/profiles/b")
	if n := len(notes()); n != 1 {
		t.Errorf("%d root change notes after a fourth next, want still 1", n)
	}
}

// A changed root is a handover only for a coordinator session the run
// already knows (a session moved between profiles keeps its id). A new
// session under another root is refused as before, so it cannot retire a
// live child of the session it would replace by that fact alone; the known
// session under its new root retires nothing.
func TestANewSessionUnderAnotherRootIsRefusedAndRetiresNothing(t *testing.T) {
	d := fanDef("root-new-session", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: "/profiles/a"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("first next: %v %+v", err, ins)
	}
	// Its child has just fetched, under profile A, in the coordinator's session.
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sA", Root: "/profiles/a", FromEnv: true})
	before := len(h.records(run))

	// With no time passed, another session under profile B calls next.
	_, err = e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sB", Root: "/profiles/b"})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), wf.HarnessMismatch) {
		t.Errorf("a new session under another root = %v, want the mismatch refusal", err)
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Errorf("the new session retired the live child: %d attempt_lost records", len(lost))
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("the refused next appended %d records", after-before)
	}
	// heartbeat and recover from it are refused the same way.
	if err := e.HeartbeatAs(run, wf.HeartbeatTarget{Name: ins[0].Name}, true, wf.Coordinator{Harness: "claude", Session: "sB", Root: "/profiles/b"}); !errors.Is(err, wf.ErrRejected) {
		t.Errorf("a new session's heartbeat under another root = %v, want it refused", err)
	}

	// The known session, moved to profile B, goes on and retires nothing.
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: "/profiles/b"}); err != nil {
		t.Fatalf("the moved session's next: %v", err)
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Errorf("the moved session retired its own child: %d attempt_lost records", len(lost))
	}
	notes := 0
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var dg wf.DegradedData
		if err := json.Unmarshal(r.Data, &dg); err != nil {
			t.Fatal(err)
		}
		if dg.Kind == wf.DegradedRootChanged {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("%d root change notes, want 1", notes)
	}

	// The session now lives under profile B. A fresh session at the root it
	// vacated is a new session under another root: refused, nothing retired.
	before = len(h.records(run))
	_, err = e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sC", Root: "/profiles/a"})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), wf.HarnessMismatch) {
		t.Errorf("a new session at the vacated root = %v, want the mismatch refusal", err)
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Errorf("the new session at the vacated root retired the live child: %d attempt_lost records", len(lost))
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("the refused next appended %d records", after-before)
	}
}

// rootNotes counts the run's records root changed notes.
func rootNotes(t *testing.T, h *harness, run wf.RunID) int {
	t.Helper()
	n := 0
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var dg wf.DegradedData
		if err := json.Unmarshal(r.Data, &dg); err != nil {
			t.Fatal(err)
		}
		if dg.Kind == wf.DegradedRootChanged {
			n++
		}
	}
	return n
}

// The same known session moving A to B and back to A is noted at each
// change, once, and retires nothing; the current root holds across a fold.
func TestASessionMovingBackIsNotedEachTimeAndRetiresNothing(t *testing.T) {
	d := fanDef("root-back", chItems(1, 2, 3, 4, 5, 6), 6, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	next := func(root string) {
		t.Helper()
		if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: root}); err != nil {
			t.Fatalf("next under %s: %v", root, err)
		}
	}
	ins, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: "/profiles/a"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("first next: %v %+v", err, ins)
	}
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sA", Root: "/profiles/a", FromEnv: true})
	for i, step := range []struct {
		root  string
		notes int
	}{{"/profiles/b", 1}, {"/profiles/b", 1}, {"/profiles/a", 2}, {"/profiles/a", 2}, {"/profiles/b", 3}} {
		if i == 3 {
			// A fold of the whole journal keeps the current root.
			e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{d}}).(*wf.Engine)
		}
		next(step.root)
		if got := rootNotes(t, h, run); got != step.notes {
			t.Errorf("step %d (next under %s): %d root change notes, want %d", i, step.root, got, step.notes)
		}
	}
	if lost := h.kind(run, wf.JournalAttemptLost); len(lost) != 0 {
		t.Errorf("the moves retired the session's own child: %d attempt_lost records", len(lost))
	}
}

// A journal with no root change note (any written before the note existed)
// folds with the segment's bound root as its current root.
func TestAJournalWithoutARootNoteKeepsTheSegmentsRoot(t *testing.T) {
	d := fanDef("root-old-journal", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	if _, err := h.e.(*wf.Engine).HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: "/profiles/a"}); err != nil {
		t.Fatal(err)
	}
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Clock: func() time.Time { return h.now }, Defs: []*wf.Def{d}}).(*wf.Engine)
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sB", Root: "/profiles/b"}); !errors.Is(err, wf.ErrRejected) {
		t.Errorf("a new session under another root after the fold = %v, want it refused", err)
	}
	if _, err := e.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sA", Root: "/profiles/a"}); err != nil {
		t.Errorf("the bound session under its root after the fold: %v", err)
	}
	if n := rootNotes(t, h, run); n != 0 {
		t.Errorf("%d root change notes with no move, want 0", n)
	}
}
