package wf

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shortLockWaits(t *testing.T, wait, after time.Duration) {
	t.Helper()
	oldWait, oldAfter := fileLockWait, lockProgressAfter
	fileLockWait, lockProgressAfter = wait, after
	t.Cleanup(func() { fileLockWait, lockProgressAfter = oldWait, oldAfter })
}

// sw-ex2v: every acquisition that waits prints its own line with a bound it
// keeps, so a next that tries again after a timed-out wait says so again
// rather than going quiet; next's line says a lock wait is no stop.
func TestEachTimedOutLockWaitPrintsItsOwnLine(t *testing.T) {
	shortLockWaits(t, 300*time.Millisecond, 20*time.Millisecond)
	dir := t.TempDir()
	unlock, err := FileJournal(dir).Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var progress bytes.Buffer
	j := FileJournal(dir)
	setLockOwner(j, lockOwner{Verb: "next", Session: "s"}, &progress, nil)
	for i := 0; i < 2; i++ {
		if _, err := j.Lock("r"); !errors.Is(err, ErrRunLocked) {
			t.Fatalf("try %d: %v, want ErrRunLocked", i+1, err)
		}
	}
	lines := strings.Split(strings.TrimSpace(progress.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("progress = %q, want one line per timed-out acquisition (2)", progress.String())
	}
	for _, l := range lines {
		for _, want := range []string{"waiting for the run: held by ", "the most this call waits for the lock", "is not a stop: do not interrupt it", "run status, then next again"} {
			if !strings.Contains(l, want) {
				t.Errorf("line %q lacks %q", l, want)
			}
		}
	}
	// A child's verb is not told to run next.
	progress.Reset()
	setLockOwner(j, lockOwner{Verb: "done", Unit: "u"}, &progress, nil)
	if _, err := j.Lock("r"); !errors.Is(err, ErrRunLocked) {
		t.Fatal(err)
	}
	if l := progress.String(); !strings.Contains(l, "waiting for the run") || strings.Contains(l, "next again") {
		t.Errorf("done's wait line = %q", l)
	}
}

// sw-ex2v: a holder that never appends is named to its waiters, and once it
// lets go the lock file is as it found it.
func TestAHolderThatNeverAppendsIsNamed(t *testing.T) {
	shortLockWaits(t, 200*time.Millisecond, 20*time.Millisecond)
	dir := t.TempDir()
	writer := FileJournal(dir)
	setLockOwner(writer, lockOwner{Verb: "run"}, nil, nil)
	unlock, err := writer.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(JournalRecord{Run: "r", Kind: JournalNote, Data: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	unlock()
	path := filepath.Join(dir, "runs", "r", "lock")
	before, err := os.ReadFile(path)
	if err != nil || len(before) == 0 {
		t.Fatalf("lock file after an append: %q, %v", before, err)
	}

	reader := FileJournal(dir)
	setLockOwner(reader, lockOwner{Verb: "prepare", Unit: "quiet-unit", Session: "s"}, nil, nil)
	unlock, err = reader.Lock("r")
	if err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	waiter := FileJournal(dir)
	setLockOwner(waiter, lockOwner{Verb: "next"}, &progress, nil)
	if _, err := waiter.Lock("r"); !errors.Is(err, ErrRunLocked) || !strings.Contains(err.Error(), "prepare for unit quiet-unit") {
		t.Errorf("waiter's error = %v, want it to name the quiet holder", err)
	}
	if !strings.Contains(progress.String(), "prepare for unit quiet-unit") {
		t.Errorf("wait line = %q, want it to name the quiet holder", progress.String())
	}
	unlock()
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Errorf("lock file after a lease that appended nothing = %q, want it as it was, %q", after, before)
	}
}
