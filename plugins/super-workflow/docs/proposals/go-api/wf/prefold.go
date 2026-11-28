package wf

import (
	"bytes"
	"fmt"
	"reflect"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/foldcheck"
)

// unlockedReader is a journal that reads a run's committed records without
// its lock, for a fold made before the lock is taken (preFold). A journal
// without it is folded under the lock only.
type unlockedReader interface {
	ReadUnlocked(run RunID) ([]JournalRecord, error)
}

// prefolded is a run folded from the records read with no lock: what an
// acquisition of the lock builds on (foldUnder), so that the fold of the
// whole journal, the largest part of a lock acquisition on a long run, runs
// outside the lock.
type prefolded struct {
	recs []JournalRecord
	def  *Def
	st   *runState
}

// preFold folds run from its committed records read with no lock; nil when
// the journal cannot read so, or the read or the fold fails (the fold under
// the lock then reports it).
func (e *Engine) preFold(run RunID) *prefolded {
	ur, ok := e.j.(unlockedReader)
	if !ok {
		return nil
	}
	recs, err := ur.ReadUnlocked(run)
	if err != nil || len(recs) == 0 {
		return nil
	}
	if foldcheck.On() && len(recs) > 1 {
		// Checked folds build on a shorter prefix, so that every
		// acquisition folds a delta, and the check compares it with the
		// full fold, even where nothing appends before the lock.
		recs = recs[:1+(37*len(recs))%(len(recs)-1)]
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return nil
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return nil
	}
	return &prefolded{recs: recs, def: def, st: st}
}

// foldUnder is foldRun(recs, def) for recs read under the run's lock. When
// p's records are an unchanged prefix of recs under the same definition, it
// folds only the records appended since onto p's state, which it takes over;
// otherwise (no p, a rewritten or shortened journal, another definition) it
// folds recs whole. The state is the same either way: the fold applies the
// records in order and reads nothing else. p holds nothing afterwards.
func foldUnder(p *prefolded, recs []JournalRecord, def *Def) (*runState, error) {
	st, _, err := foldOnto(p, recs, def)
	return st, err
}

// foldOnto is foldUnder, reporting whether it folded onto p.
func foldOnto(p *prefolded, recs []JournalRecord, def *Def) (_ *runState, onto bool, _ error) {
	if p != nil {
		// What p holds is let go as soon as this fold no longer needs it,
		// not when the verb returns.
		defer func() { p.recs, p.st = nil, nil }()
	}
	if p == nil || p.def != def || len(p.recs) == 0 || len(p.recs) > len(recs) || !sameRecords(p.recs, recs[:len(p.recs)]) {
		st, err := foldRun(recs, def)
		return st, false, err
	}
	st := p.st
	for _, r := range recs[len(p.recs):] {
		if err := st.apply(r); err != nil {
			return nil, true, err
		}
	}
	if foldcheck.On() {
		full, err := foldRun(recs, def)
		if err != nil {
			panic(fmt.Sprintf("wf: fold check: the delta fold of %d+%d records succeeded, the full fold failed: %v", len(p.recs), len(recs)-len(p.recs), err))
		}
		if !reflect.DeepEqual(st, full) {
			panic(fmt.Sprintf("wf: fold check: the delta fold of %d+%d records differs from the full fold:\ndelta %s\nfull  %s", len(p.recs), len(recs)-len(p.recs), st.digest(), full.digest()))
		}
	}
	return st, true, nil
}

// sameRecords reports whether a and b hold the same records: every field
// the fold reads.
func sameRecords(a, b []JournalRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := &a[i], &b[i]
		if x.Seq != y.Seq || x.Kind != y.Kind || x.Run != y.Run || x.Instance != y.Instance || x.State != y.State ||
			x.Attempt != y.Attempt || x.Class != y.Class || x.DefHash != y.DefHash || x.RelativePaths != y.RelativePaths ||
			!x.At.Equal(y.At) || !sameZone(x.At, y.At) || !bytes.Equal(x.Data, y.Data) {
			return false
		}
		if (x.Roots == nil) != (y.Roots == nil) || x.Roots != nil && *x.Roots != *y.Roots {
			return false
		}
	}
	return true
}

func sameZone(a, b time.Time) bool {
	_, x := a.Zone()
	_, y := b.Zone()
	return x == y
}
