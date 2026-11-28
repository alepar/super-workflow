package wf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type (
	pfFirst  struct{}
	pfSecond struct{}
	pfDone   struct{}
	pfOut    struct{ OK bool }
)

func prefoldDef() *Def {
	d := New("prefold")
	first := NewAgent[pfFirst, pfOut](d)
	second := NewAgent[pfSecond, pfOut](d)
	done := NewTerminal[pfDone](d, Success)
	d.OnFailure(Blocked, done)
	first.Spec(PromptText("first"), Retry[pfFirst](Const(1), first.To(done, "gave up")))
	second.Spec(PromptText("second"), Retry[pfSecond](Const(1), second.To(done, "gave up")))
	toSecond := first.To(second, "first done")
	first.Route(func(pfOut, Ctx) Edge[pfFirst] { return toSecond }, toSecond)
	toDone := second.To(done, "second done")
	second.Route(func(pfOut, Ctx) Edge[pfSecond] { return toDone }, toDone)
	return d
}

// prefoldRun starts the run on a file journal under dir and hands out its
// first agent: the instruction a second process submits.
func prefoldRun(t *testing.T, dir string) (*Engine, RunID, Instruction) {
	t.Helper()
	e := NewEngine(FileJournal(dir), NewAdapters(), EngineOptions{Defs: []*Def{prefoldDef()}}).(*Engine)
	id, err := e.Run(prefoldDef(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(id, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != InstructionAgent {
		t.Fatalf("next = %+v, %v; want the first agent", ins, err)
	}
	return e, id, ins[0]
}

// other is a second process's engine on the same state dir: its own journal
// value, so what it appends reaches e only through the file.
func other(dir string) *Engine {
	return NewEngine(FileJournal(dir), NewAdapters(), EngineOptions{Defs: []*Def{prefoldDef()}}).(*Engine)
}

func submitOK(t *testing.T, e *Engine, id RunID, in Instruction) {
	t.Helper()
	out, _ := json.Marshal(pfOut{OK: true})
	if _, err := e.Submit(id, Submission{Token: in.Token, Output: out}); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

// An append by another process between the fold before the lock and the
// lock is folded as a delta onto the fold made before, and the state is the
// full fold's.
func TestPreFoldAppliesAConcurrentAppendAsADelta(t *testing.T) {
	dir := t.TempDir()
	e, id, in := prefoldRun(t, dir)
	pre := e.preFold(id)
	if pre == nil {
		t.Fatal("no fold before the lock on a file journal")
	}
	before, err := e.j.(unlockedReader).ReadUnlocked(id)
	if err != nil {
		t.Fatal(err)
	}
	submitOK(t, other(dir), id, in)

	unlock, err := e.j.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := e.j.Records(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) <= len(before) {
		t.Fatalf("records under the lock = %d, before = %d: the other process appended nothing", len(recs), len(before))
	}
	def, _ := e.defFor(recs[0])
	st, onto, err := foldOnto(pre, recs, def)
	if err != nil {
		t.Fatal(err)
	}
	if !onto {
		t.Fatal("foldUnder folded the whole journal; want the delta onto the fold before the lock")
	}
	full, err := foldRun(recs, def)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(st, full) {
		t.Fatalf("delta fold differs from the full fold:\ndelta %s\nfull  %s", st.digest(), full.digest())
	}
}

// A journal that is no longer what the fold before the lock read, here
// with its first record's time rewritten, is folded whole under the lock.
func TestPreFoldFallsBackOnAChangedPrefix(t *testing.T) {
	dir := t.TempDir()
	e, id, _ := prefoldRun(t, dir)
	pre := e.preFold(id)
	path := filepath.Join(dir, "runs", string(id), journalFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(data, []byte(`"At":"2`), []byte(`"At":"1`), 1)
	if bytes.Equal(changed, data) {
		t.Fatal("no record time to rewrite")
	}
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, err := e.j.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := e.j.Records(id)
	if err != nil {
		t.Fatal(err)
	}
	def, _ := e.defFor(recs[0])
	st, onto, err := foldOnto(pre, recs, def)
	if err != nil {
		t.Fatal(err)
	}
	if onto {
		t.Fatal("foldUnder built on a fold of records the journal no longer holds")
	}
	full, _ := foldRun(recs, def)
	if !reflect.DeepEqual(st, full) {
		t.Fatal("fallback fold differs from the full fold")
	}
}

// Through a verb: another process's submit lands between the fold before
// the lock and the lock, and the verb, folding only that delta, sees it and
// hands out the second agent.
func TestPreFoldVerbSeesAnAppendBeforeItsLock(t *testing.T) {
	dir := t.TempDir()
	e, id, in := prefoldRun(t, dir)
	o := other(dir)
	hooked := 0
	preFoldedHook = func(run RunID) {
		if hooked++; hooked == 1 {
			submitOK(t, o, run, in)
		}
	}
	t.Cleanup(func() { preFoldedHook = nil })
	ins, err := e.Next(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if hooked == 0 {
		t.Fatal("Next took no fold before its lock")
	}
	if len(ins) != 1 || ins[0].Kind != InstructionAgent || ins[0].Prompt == in.Prompt {
		t.Fatalf("next = %+v; want the second agent, after the other process's submit", ins)
	}
}
