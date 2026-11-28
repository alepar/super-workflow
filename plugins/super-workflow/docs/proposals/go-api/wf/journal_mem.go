package wf

import (
	"fmt"
	"slices"
	"sync"
)

type memRun struct {
	lock sync.Mutex // the run's cycle lock
	held bool       // guarded by memJournal.mu
	recs []JournalRecord
}

type memJournal struct {
	mu   sync.Mutex // guards runs, held and recs
	runs map[RunID]*memRun
	subs map[RunID][]chan struct{} // Watch subscribers, guarded by mu
}

// MemJournal is an in-process Journal for tests; its lock is a per-run mutex.
func MemJournal() Journal {
	return &memJournal{runs: map[RunID]*memRun{}, subs: map[RunID][]chan struct{}{}}
}

func (j *memJournal) Lock(run RunID) (func(), error) {
	if err := validRunID(run); err != nil {
		return nil, err
	}
	j.mu.Lock()
	r, ok := j.runs[run]
	if !ok {
		r = &memRun{}
		j.runs[run] = r
	}
	j.mu.Unlock()
	r.lock.Lock()
	j.mu.Lock()
	r.held = true
	j.mu.Unlock()
	return func() {
		j.mu.Lock()
		r.held = false
		j.mu.Unlock()
		r.lock.Unlock()
	}, nil
}

// heldRun returns the locked run; the caller holds j.mu.
func (j *memJournal) heldRun(run RunID) (*memRun, error) {
	r, ok := j.runs[run]
	if !ok || !r.held {
		return nil, fmt.Errorf("%w: %s", ErrJournalNotLocked, run)
	}
	return r, nil
}

func (j *memJournal) Append(rec JournalRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, err := j.heldRun(rec.Run)
	if err != nil {
		return err
	}
	rec.Seq = int64(len(r.recs) + 1)
	rec.Data = slices.Clone(rec.Data)
	r.recs = append(r.recs, rec)
	for _, ch := range j.subs[rec.Run] {
		select {
		case ch <- struct{}{}:
		default: // a signal is already pending
		}
	}
	return nil
}

func (j *memJournal) Watch(run RunID, stop <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{}, 1)
	j.mu.Lock()
	j.subs[run] = append(j.subs[run], ch)
	j.mu.Unlock()
	go func() {
		<-stop
		j.mu.Lock()
		j.subs[run] = slices.DeleteFunc(j.subs[run], func(c chan struct{}) bool { return c == ch })
		j.mu.Unlock()
	}()
	return ch
}

func (j *memJournal) Records(run RunID) ([]JournalRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, err := j.heldRun(run)
	if err != nil {
		return nil, err
	}
	if len(r.recs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	out := slices.Clone(r.recs)
	for i := range out {
		out[i].Data = slices.Clone(out[i].Data)
	}
	return out, nil
}

// ReadUnlocked is Records with no lock held: the read a fold before the
// lock builds on (preFold).
func (j *memJournal) ReadUnlocked(run RunID) ([]JournalRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.runs[run]
	if !ok || len(r.recs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	out := slices.Clone(r.recs)
	for i := range out {
		out[i].Data = slices.Clone(out[i].Data)
	}
	return out, nil
}

func (j *memJournal) Runs() ([]RunID, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []RunID
	for id, r := range j.runs {
		if len(r.recs) > 0 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}
