package wf

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// failingAppendJournal fails every append, counting them, as a write that
// may have committed before it reported failure would.
type failingAppendJournal struct {
	Journal
	appends int
}

var errAppendFailed = errors.New("append failed")

func (j *failingAppendJournal) Append(JournalRecord) error {
	j.appends++
	return errAppendFailed
}

// A reused checkpoint whose append fails returns that failure once: it is
// never retried as an unavailable record, which could journal the
// checkpoint twice (an earlier revision review round 1, P1).
func TestCheckpointReuseAppendFailureIsNotRetried(t *testing.T) {
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "records"), 0o700); err != nil {
		t.Fatal(err)
	}
	stamp, err := runDataDigest(runDir)
	if err != nil {
		t.Fatal(err)
	}
	j := &failingAppendJournal{}
	c := &cycle{
		e:        &Engine{j: j, opts: EngineOptions{RunDir: func(RunID) string { return runDir }, Clock: time.Now}},
		run:      "r",
		st:       &runState{started: &RunStartedData{}, insts: map[string]*instance{}},
		captured: &WorldSnapshotData{Version: 2, Directory: "snapshots/store.git", Tree: "t"}, capturedStamp: stamp,
	}
	if err := c.checkpoint("root", "review", 1, JournalInstructionIssued); !errors.Is(err, errAppendFailed) {
		t.Fatalf("checkpoint: %v", err)
	}
	if j.appends != 1 {
		t.Fatalf("appends %d, want 1", j.appends)
	}
}
