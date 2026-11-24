package wf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A lease that wrote the holder record marks it released when it lets the
// lock go, so a record left after a clean release is never read as a live
// holder. A lease that only read writes nothing, and a holder that died
// leaves its record unmarked: the kernel let the flock go, and only a probe
// of the flock says the lock is free.
func TestReleaseMarksTheHolderRecord(t *testing.T) {
	dir := t.TempDir()
	j := FileJournal(dir).(*fileJournal)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if h, ok := j.recordedHolder("r"); !ok || !h.Released.IsZero() {
		t.Fatalf("while held: record %+v, %v; want one with no release", h, ok)
	}
	unlock()
	h, ok := j.recordedHolder("r")
	if !ok || h.Pid != os.Getpid() || h.Released.IsZero() || h.Released.Before(h.Since) {
		t.Fatalf("after release: record %+v, %v; want this pid marked released after it took the lock", h, ok)
	}

	// A read-only lease leaves the released record as it was.
	before, _ := os.ReadFile(filepath.Join(dir, "runs", "r", "lock"))
	unlock, err = FileJournal(dir).Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if after, _ := os.ReadFile(filepath.Join(dir, "runs", "r", "lock")); !bytes.Equal(before, after) {
		t.Errorf("a lease that appended nothing rewrote the record: %q, was %q", after, before)
	}
}

// wf lock <run> reads the run's lock without taking it: whether a process
// holds it now (the flock, not the record) and the record of its last
// writer, with its release when it marked one.
func TestLockVerbReportsTheLiveFlock(t *testing.T) {
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	dir := filepath.Join(t.TempDir(), "state")
	call := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := CLI(cfg, args, &o, &e)
		return code, o.String(), e.String()
	}
	code, out, stderr := call("run", "lock-cli", "--state-dir", dir, "--workspace", t.TempDir(), "--run-id", "lock-cli-1")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, stderr)
	}
	run := strings.TrimSpace(out)

	code, out, stderr = call("lock", run, "--state-dir", dir)
	if code != 0 {
		t.Fatalf("lock exited %d: %s", code, stderr)
	}
	if !strings.HasPrefix(out, "lock: no visible holder") || !strings.Contains(out, "last written by run, pid ") || !strings.Contains(out, ", released at ") {
		t.Errorf("free lock:\n%s\nwant it free, naming its last writer and its release", out)
	}

	holder, unlock := flockProcess(t, filepath.Join(dir, "runs", run, "lock"))
	code, out, _ = call("lock", run, "--state-dir", dir)
	if want := fmt.Sprintf("lock: held now by pid %d\n", holder); code != 0 || !strings.HasPrefix(out, want) {
		t.Errorf("held lock: exit %d\n%s\nwant it to start %q", code, out, want)
	}
	code, out, _ = call("lock", run, "--state-dir", dir, "--json")
	var v LockState
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || !v.Held || v.Pid != holder || v.Holder == nil {
		t.Errorf("lock --json: exit %d %s, want held by pid %d with the record", code, out, holder)
	}
	unlock()
	// flock(1)'s sleep holds the lock until it is reaped: wait for the kernel.
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
		if code, out, _ = call("lock", run, "--state-dir", dir); code != 0 || strings.HasPrefix(out, "lock: no visible holder") {
			break
		}
	}
	if code != 0 || !strings.HasPrefix(out, "lock: no visible holder") {
		t.Errorf("after the holder let go: exit %d\n%s", code, out)
	}
}

// The coordinator's rules point at the lock verb instead of only forbidding
// edits to lock files.
func TestTheRulesPointAtTheLockVerb(t *testing.T) {
	if !strings.Contains(RulesForThisRun, "(see the lock verb)") {
		t.Errorf("the rules do not point at the lock verb:\n%s", RulesForThisRun)
	}
}

// Review round 1 (1): the release stamp never truncates the record. It goes
// after it, on a line of its own, so a short write or a crash mid-stamp
// leaves the record whole and only the stamp unreadable.
func TestReleaseStampNeverTruncatesTheRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs", "r", "lock")
	j := FileJournal(dir).(*fileJournal)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	var ino uint64
	if fi, err := os.Stat(path); err == nil {
		ino = fi.Sys().(*syscall.Stat_t).Ino
	}
	unlock()
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, before) || len(after) == len(before) {
		t.Fatalf("after release the file is %q, want the record %q kept whole with a stamp after it", after, before)
	}
	if fi, err := os.Stat(path); err != nil || fi.Sys().(*syscall.Stat_t).Ino != ino {
		t.Error("the stamp replaced the lock file: its inode changed")
	}
	// A stamp cut short: the record still reads, with no release.
	if err := os.WriteFile(path, append(append([]byte{}, before...), []byte("\n{\"relea")...), 0o644); err != nil {
		t.Fatal(err)
	}
	h, ok := j.recordedHolder("r")
	if !ok || h.Pid != os.Getpid() || !h.Released.IsZero() {
		t.Errorf("record with a torn stamp = %+v, %v; want the record, no release", h, ok)
	}
}

// Review round 1 (2): a record at its bound still reads after its stamp: the
// reader's bound covers the record's and the stamp's.
func TestReleaseStampFitsAFullRecord(t *testing.T) {
	// Sessions of every length that lands the record at or near its bound.
	near := 0
	for n := 800; n <= 1100; n++ {
		dir := t.TempDir()
		j := FileJournal(dir).(*fileJournal)
		setLockOwner(j, lockOwner{Verb: "done", Session: strings.Repeat("s", n)}, nil, nil)
		unlock, err := j.Lock("r")
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		rec, _ := os.ReadFile(filepath.Join(dir, "runs", "r", "lock"))
		if len(rec) > holderRecordMax-64 {
			near++
		}
		unlock()
		h, ok := j.recordedHolder("r")
		if !ok || h.Verb != "done" || h.Session == "" || h.Released.IsZero() {
			t.Fatalf("a %d-byte record after its stamp reads %+v, %v; want it whole and released", len(rec), h, ok)
		}
	}
	if near == 0 {
		t.Fatal("no record came near its bound: the test proves nothing")
	}
}

// Review round 1 (3, 4): the verb never certifies a lock free. A table that
// lists no holder is "no visible holder" (a hold it does not show is still
// one); a table it cannot read is unknown. A record with no stamp says only
// that no release was recorded (a release before the stamp existed left
// none).
func TestRenderLockNeverCertifiesFree(t *testing.T) {
	legacy := &LockHolder{Pid: 42, Verb: "next"}
	for _, tc := range []struct {
		st   LockState
		want []string
	}{
		{LockState{Known: true}, []string{"lock: no visible holder"}},
		{LockState{}, []string{"lock: unknown"}},
		{LockState{Known: true, Holder: legacy}, []string{"lock: no visible holder", "no release was recorded"}},
	} {
		got := RenderLock(tc.st)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("RenderLock(%+v) = %q, want %q", tc.st, got, w)
			}
		}
		for _, bad := range []string{"free", "exited"} {
			if strings.Contains(got, bad) {
				t.Errorf("RenderLock(%+v) = %q claims %q", tc.st, got, bad)
			}
		}
	}
}

// Review round 1 (3), for an earlier revision's probe: a lock table that cannot be read
// says nothing about the holder, so an open effect may be running.
func TestUnreadableLockTableIsUnknown(t *testing.T) {
	old := procLocksPath
	procLocksPath = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procLocksPath = old })
	dir := t.TempDir()
	j := FileJournal(dir).(*fileJournal)
	unlock, err := j.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if held, known := j.heldNow("r"); held || known {
		t.Errorf("heldNow with no table = %v, %v; want not known", held, known)
	}
	if st := j.lockState("r"); st.Known || st.Held {
		t.Errorf("lockState with no table = %+v, want unknown", st)
	}
}
