package wf

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// appendStarted journals run's run_started naming pred as its predecessor,
// with no recorded Worktrees (a successor from before the field).
func appendStarted(t *testing.T, j Journal, run, pred RunID) {
	t.Helper()
	data, err := json.Marshal(RunStartedData{Def: "d", Predecessor: pred})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := j.Append(JournalRecord{Run: run, Kind: JournalRunStarted, Data: data}); err != nil {
		t.Fatal(err)
	}
}

// evw-snap round 2 (2): a legacy successor's chain that repeats a run is
// refused, clearly, both by a capture's chain read and when a successor's
// inherited set is computed at creation.
func TestACorruptPredecessorChainIsRefused(t *testing.T) {
	j := FileJournal(t.TempDir()) // reads an ended run without its lock
	appendStarted(t, j, "a", "b")
	appendStarted(t, j, "b", "a")
	e := &Engine{j: j}
	unlock, err := j.Lock("a")
	if err != nil {
		t.Fatal(err)
	}
	recs, err := j.Records("a")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	c, err := endedRunCycle(e, "a", recs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.seedWorktrees(map[RunID]bool{"a": true}); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "repeats run a") {
		t.Fatalf("capture's chain read of a looping chain = %v, want a refusal naming the repeated run", err)
	}
	if _, err := e.inheritedWorktrees("a", recs); err == nil || !strings.Contains(err.Error(), "corrupt chain") {
		t.Fatalf("a successor's inherited set over a looping chain = %v, want it refused", err)
	}
}

// evw-snap final round: a capture never blocks on another run's lock while
// holding its own. Two legacy runs whose chain loops (a -> b -> a), each
// held by its own cycle as runAt holds it, compute their inherited sets at
// once: on a journal with no lock-free read, each is unavailable with why,
// and neither waits on the other.
func TestLegacyChainReadNeverWaitsOnAnotherRunsLock(t *testing.T) {
	j := MemJournal()
	appendStarted(t, j, "a", "b")
	appendStarted(t, j, "b", "a")
	e := &Engine{j: j}
	errs := make(chan error, 2)
	ready := make(chan struct{})
	held := make(chan struct{}, 2)
	for _, run := range []RunID{"a", "b"} {
		go func(run RunID) {
			unlock, err := j.Lock(run)
			if err != nil {
				errs <- err
				return
			}
			defer unlock()
			recs, err := j.Records(run)
			if err != nil {
				errs <- err
				return
			}
			held <- struct{}{}
			<-ready // both locks are held before either reads the other
			_, err = e.inheritedWorktrees(run, recs)
			errs <- err
		}(run)
	}
	<-held
	<-held
	close(ready)
	for range 2 {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "no lock-free read") {
				t.Fatalf("inherited set over a lock-only journal = %v, want unavailable: no lock-free read", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a run's chain read waited on another run's lock while holding its own")
		}
	}
}
