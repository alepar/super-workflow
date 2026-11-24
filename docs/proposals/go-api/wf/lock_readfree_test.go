package wf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The read verbs read a run's committed records with no lock: a
// status, explain or journal from the process that holds the lock (the
// provenance snapshot is an effect that calls wf status on its own run)
// returns at once instead of waiting out the lock wait.
func TestReadVerbsNeverWaitOnTheLock(t *testing.T) {
	old := fileLockWait
	fileLockWait = 3 * time.Second
	t.Cleanup(func() { fileLockWait = old })
	noHarnessEnv(t)
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	dir := filepath.Join(t.TempDir(), "state")
	var o, e bytes.Buffer
	if code := CLI(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", "rf"}, &o, &e); code != 0 {
		t.Fatal(e.String())
	}
	o.Reset()
	if code := CLI(cfg, []string{"next", "rf", "1", "--state-dir", dir, "--json"}, &o, &e); code != 0 {
		t.Fatal(e.String())
	}
	unlock, err := FileJournal(dir).Lock("rf")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, args := range [][]string{
		{"status", "rf", "--state-dir", dir},
		{"status", "rf", "--state-dir", dir, "--json"},
		{"journal", "rf", "--state-dir", dir},
		{"explain", "rf", "lkAsk#1", "--state-dir", dir},
	} {
		o.Reset()
		e.Reset()
		start := time.Now()
		code := CLI(cfg, args, &o, &e)
		if took := time.Since(start); took > time.Second {
			t.Errorf("%v waited %v under a held lock", args, took)
		}
		if code != 0 {
			t.Errorf("%v under a held lock: exit %d: %s", args, code, e.String())
		}
	}
}

// A lock-free read sees a committed prefix: a record being written (a torn
// final line) is never read, and the records it reads are always 1..n.
func TestReadCommittedNeverSeesATornRecord(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir).(*fileJournal)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{"Text":"first"}`)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runs", "r", journalFile)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		// The writer: whole records, each written in two halves, so a read can
		// land between them.
		defer wg.Done()
		for i := 2; i <= 200; i++ {
			line, _ := json.Marshal(JournalRecord{Seq: int64(i), Run: "r", Kind: JournalNote, Data: []byte(`{"Text":"x"}`)})
			line = append(line, '\n')
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Error(err)
				return
			}
			half := len(line) / 2
			_, _ = f.Write(line[:half])
			time.Sleep(50 * time.Microsecond)
			_, _ = f.Write(line[half:])
			f.Close()
		}
		close(stop)
	}()
	reader := FileJournal(dir).(*fileJournal)
	last := 0
	for done := false; !done; {
		select {
		case <-stop:
			done = true
		default:
		}
		recs, err := reader.ReadCommitted("r")
		if err != nil {
			t.Fatalf("a lock-free read: %v", err)
		}
		for i, r := range recs {
			if r.Seq != int64(i+1) || !strings.Contains(string(r.Data), "Text") {
				t.Fatalf("record %d read as %+v", i+1, r)
			}
		}
		if len(recs) < last {
			t.Fatalf("a later read saw %d records after %d", len(recs), last)
		}
		last = len(recs)
	}
	wg.Wait()
	unlock()
	if recs, _ := reader.ReadCommitted("r"); len(recs) != 200 {
		t.Errorf("read %d records at the end, want 200", len(recs))
	}
}

// A journal that cannot read without its lock (MemJournal), wrapped for its
// state root, is read under its lock: status still works (review round 1).
func TestStatusOverAJournalWithoutLockFreeReads(t *testing.T) {
	e := NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{lkDef()}, StateRoot: t.TempDir()})
	run, err := e.Run(lkDef(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Status(run); err != nil {
		t.Fatalf("status over a MemJournal with a state root: %v", err)
	}
	if _, err := e.(*Engine).Explain(run, "nothing"); err == nil || strings.Contains(err.Error(), "without its lock") {
		t.Fatalf("explain over a MemJournal with a state root: %v, want its own refusal", err)
	}
}

// A lock-free read keeps its path projection to itself: it leaves no roots
// behind for a run nobody holds, and never replaces the roots of a call
// that holds the run (review round 1).
func TestReadCommittedLeavesTheWritersRoots(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	e := NewEngine(FileJournal(dir), NewAdapters(), EngineOptions{Defs: []*Def{lkDef()}})
	run, err := e.Run(lkDef(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rj := rootedFileJournal(dir, t.TempDir()).(*rootedJournal)
	if _, err := rj.ReadCommitted(run); err != nil {
		t.Fatal(err)
	}
	if len(rj.active) != 0 {
		t.Errorf("a lock-free read left roots %v", rj.active)
	}
	unlock, err := rj.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	writer := PathRoots{Workspace: "/the/writers/workspace", State: "/the/writers/state"}
	rj.set(run, writer)
	if _, err := rj.ReadCommitted(run); err != nil {
		t.Fatal(err)
	}
	if got := rj.active[run]; got != writer {
		t.Errorf("a lock-free read replaced the writer's roots with %+v", got)
	}
}

// Status names the lock file's holder, the latest, not the one this engine
// saw when it last took the lock itself (review round 1).
func TestAReusedEngineReportsTheCurrentHolder(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir)
	e := NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{lkDef()}})
	run, err := e.Run(lkDef(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// This engine's journal takes the lock again: it caches the holder it found.
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	cached, _ := j.(*fileJournal).lastHolder(run)
	time.Sleep(20 * time.Millisecond)
	// Another call takes the run and writes.
	other := FileJournal(dir)
	unlock, err = other.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(JournalRecord{Run: run, Kind: JournalNote, Data: []byte(`{"Text":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	unlock()
	current, ok := other.(*fileJournal).recordedHolder(run)
	if !ok || !current.Since.After(cached.Since) {
		t.Fatalf("the other call's holder record %+v (%v) is not later than %+v", current, ok, cached)
	}
	st, err := e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	if st.LockHolder == nil || !st.LockHolder.Since.Equal(current.Since) {
		t.Errorf("LockHolder = %+v, want the current record %+v", st.LockHolder, current)
	}
}
