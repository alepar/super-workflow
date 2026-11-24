package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type snapAsk struct{}
type snapDone struct{}

// snapDef is a keyed one-step run whose finishing edge writes the run's
// status snapshot to path (F4: the workflow no longer runs its own binary's
// wf status, nor finds the state dir by path arithmetic).
func snapDef(path string) *wf.Def {
	d := wf.New("status-snap")
	d.RunKey(d.Param("topic"))
	a := wf.NewAgent[snapAsk, string](d)
	done := wf.NewTerminal[snapDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	fin := a.To(done, "done").With(wf.StatusSnapshot(wf.KeyOf("status-snapshot"), path, true))
	a.Spec(wf.PromptText("ask"), wf.Retry[snapAsk](wf.Const(1), a.To(done, "gave up")))
	a.Route(func(string, wf.Ctx) wf.Edge[snapAsk] { return fin }, fin)
	return d
}

// snapDoc is the snapshot document: wf status --json's fields, and the
// predecessors' documents newest first.
type snapDoc struct {
	wf.RunStatus
	Predecessors []struct {
		Run        wf.RunID
		Unreadable bool
	}
}

func readSnap(t *testing.T, path string) snapDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d snapDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("snapshot %s: %v\n%s", path, err, raw)
	}
	return d
}

// F4: the engine writes a run's status snapshot itself, as wf status --json
// prints it, with its predecessor chain; a successor's names its
// predecessors newest first.
func TestStatusSnapshotWritesTheRunsStatusWithItsChain(t *testing.T) {
	state, ws := t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "status.json")
	def := snapDef(path)
	e := wf.NewEngine(wf.FileJournal(state), wf.NewAdapters(), wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: ws, StateRoot: state,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	params := map[string]string{"topic": "x"}
	finish := func(run wf.RunID) {
		t.Helper()
		ins, err := e.Next(run, "")
		if err != nil || len(ins) != 1 {
			t.Fatalf("next: %+v %v", ins, err)
		}
		if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	first, err := e.Run(def, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	finish(first)
	if d := readSnap(t, path); d.Run != first || d.Def != "status-snap" || len(d.Predecessors) != 0 {
		t.Fatalf("first snapshot = run %s def %s predecessors %+v, want %s's own", d.Run, d.Def, d.Predecessors, first)
	}
	second, err := e.Run(def, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	finish(second)
	d := readSnap(t, path)
	if d.Run != second || d.Predecessor != first || len(d.Predecessors) != 1 || d.Predecessors[0].Run != first || d.Predecessors[0].Unreadable {
		t.Fatalf("successor snapshot = run %s predecessor %s chain %+v, want %s after %s", d.Run, d.Predecessor, d.Predecessors, second, first)
	}
}

// A run's status is read without its lock, which the applying cycle holds:
// on a journal with no lock-free read the snapshot is unavailable, never a
// wait on the run's own lock.
func TestStatusSnapshotOnALockOnlyJournalIsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	// A carried snapshot (a predecessor's, in records it carried over) must
	// not survive a failed one and read as this run's.
	if err := os.WriteFile(path, []byte(`{"Run":"carried"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	def := snapDef(path)
	j := wf.MemJournal()
	e := wf.NewEngine(j, wf.NewAdapters(), wf.EngineOptions{Defs: []*wf.Def{def}, Workspace: t.TempDir()}).(*wf.Engine)
	run, err := e.Run(def, map[string]string{"topic": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v %v", ins, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)})
		done <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a failed snapshot left a file behind (a carried one, or its own): %v", err)
	}
	var unavailable bool
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := j.Records(run)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == wf.JournalEffectResult {
			var d wf.EffectResultData
			if json.Unmarshal(r.Data, &d) == nil && !d.OK {
				unavailable = true
			}
		}
	}
	if !unavailable {
		t.Fatal("no failed effect result for the unavailable snapshot")
	}
}

// F4 round 2: with StateRoot set the journal is wrapped (rootedJournal),
// which offers ReadCommitted even over a backend that cannot. Every status
// read of the snapshot, predecessors' included, is lock-free through every
// wrapper, or the snapshot is unavailable: it never falls back to the lock
// its own cycle holds.
func TestStatusSnapshotNeverTakesTheLockThroughAWrapper(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	def := snapDef(path)
	state := t.TempDir()
	e := wf.NewEngine(wf.MemJournal(), wf.NewAdapters(), wf.EngineOptions{Defs: []*wf.Def{def}, Workspace: t.TempDir(), StateRoot: state,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }}).(*wf.Engine)
	run, err := e.Run(def, map[string]string{"topic": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v %v", ins, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the snapshot waited on the lock its own cycle holds")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a lock-only backend wrote a snapshot: %v", err)
	}
}

// F4 round 2 (main's ruling): a run adopted mid-step can write its snapshot
// twice, once by the old detached writer and again by the new success edge.
// Both are this run's status; the later is the more current, and it is what
// the file holds.
func TestStatusSnapshotRewriteHoldsTheCurrentStatus(t *testing.T) {
	state, ws := t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "status.json")
	def := snapDef(path)
	e := wf.NewEngine(wf.FileJournal(state), wf.NewAdapters(), wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: ws, StateRoot: state,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	run, err := e.Run(def, map[string]string{"topic": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %+v %v", ins, err)
	}
	// The old detached writer's snapshot of this run, taken before the step's
	// submission: no tokens yet.
	if err := os.WriteFile(path, []byte(`{"Run":"`+string(run)+`","Tokens":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`), Tokens: 42}); err != nil {
		t.Fatal(err)
	}
	if d := readSnap(t, path); d.Run != run || d.Tokens != 42 {
		t.Fatalf("after the rewrite the file holds run %s tokens %d, want %s's current status (42 tokens)", d.Run, d.Tokens, run)
	}
}
