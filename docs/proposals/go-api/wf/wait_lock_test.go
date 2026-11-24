package wf_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// lockedJournal reports the run's lock busy (as the file backend does once its
// bounded retry ran out) on the next `busy` Lock calls.
type lockedJournal struct {
	wf.Journal
	busy atomic.Int32
}

func (j *lockedJournal) Lock(run wf.RunID) (func(), error) {
	if j.busy.Add(-1) >= 0 {
		return nil, fmt.Errorf("%w: run locked by pid 1 since now", wf.ErrRunLocked)
	}
	return j.Journal.Lock(run)
}

// A read-only wait that races a writer holding the run's lock (a child's wf
// done) keeps waiting rather than exiting with run locked.
func TestWaitRetriesALockedRun(t *testing.T) {
	d := lsDef("wt-locked", true)
	j := &lockedJournal{Journal: wf.MemJournal()}
	e := wf.NewEngine(j, nil, wf.EngineOptions{Defs: []*wf.Def{d}, HandOutWindow: 150 * time.Millisecond})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := e.HandOut(run, 1); err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	j.busy.Store(3)
	res, err := e.Wait(run, -1, 10*time.Second)
	if err != nil {
		t.Fatalf("wait on a briefly locked run failed: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Kind != wf.EventWindow {
		t.Fatalf("events = %+v, want the expired hand-out once the lock was free", res.Events)
	}
}

// A run that stays locked for the whole wait times out, as an idle wait does.
func TestWaitOnAStuckLockTimesOut(t *testing.T) {
	d := lsDef("wt-stuck", true)
	j := &lockedJournal{Journal: wf.MemJournal()}
	e := wf.NewEngine(j, nil, wf.EngineOptions{Defs: []*wf.Def{d}})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j.busy.Store(1 << 30)
	res, err := e.Wait(run, 5, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("wait on a stuck lock failed: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Kind != wf.EventTimeout || res.Seq != 5 {
		t.Fatalf("result = %+v, want a timeout that keeps the caller's seq 5", res)
	}
	if res, err := e.Wait(run, -1, 200*time.Millisecond); err != nil || res.Seq != -1 {
		t.Fatalf("a default wait on a stuck lock = %+v, %v; want seq -1 (the default baseline), never 0", res, err)
	}
}
