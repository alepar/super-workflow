package wf_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type aOut2 struct{ Score int }

func TestAdoptRefusesAmbiguousLegacyAssetRoot(t *testing.T) {
	v1 := migDef(false)
	v1.Param("skillsRoot")
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "assets-legacy"})
	run := h.run(v1, map[string]string{"skillsRoot": t.TempDir()})
	h.next(run)
	fixed := adoptDef("new prompt", false, false).Assets(t.TempDir())
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{fixed}, Adopt: true})
	if _, err := e.Next(run, ""); err == nil || !strings.Contains(err.Error(), "--assets") {
		t.Fatalf("ambiguous old root accepted: %v", err)
	}
}

func TestLegacyAssetChoiceIsJournaledEvenWithoutHashChange(t *testing.T) {
	d := promptHashDef("root {{skillsRoot}}")
	d.Param("skillsRoot")
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunID: "legacy-choice"})
	run := h.run(d, map[string]string{"skillsRoot": oldRoot})
	h.next(run)
	t.Chdir(filepath.Dir(newRoot))
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}, Adopt: true, Assets: filepath.Base(newRoot)}).(*wf.Engine)
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	recs := h.kind(run, wf.JournalMigrated)
	if len(recs) != 1 {
		t.Fatalf("asset choice needs a migration even with same hash: %v", recs)
	}
	var data wf.MigratedData
	_ = json.Unmarshal(recs[0].Data, &data)
	if data.Assets != newRoot || data.FromHash != data.ToHash {
		t.Fatalf("wrong asset choice record: %+v", data)
	}
	e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}}).(*wf.Engine)
	f, err := e.PromptFetch(run, "pfA#1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Prompt, newRoot) || strings.Contains(f.Prompt, oldRoot) {
		t.Fatalf("explicit asset choice not used: %s", f.Prompt)
	}
}

// adoptDef is migDef's v1 shape (mOld -> mEnd) with prompt text and, when
// otherOut, a different result type on mOld; endFirst declares the terminal
// first, which moves the definition's first state.
func adoptDef(prompt string, otherOut, endFirst bool) *wf.Def {
	d := wf.New("mig")
	var end *wf.Terminal[mEnd]
	if endFirst {
		end = wf.NewTerminal[mEnd](d, wf.Success)
	}
	if otherOut {
		a := wf.NewAgent[mOld, aOut2](d)
		if end == nil {
			end = wf.NewTerminal[mEnd](d, wf.Success)
		}
		a.Spec(wf.PromptText(prompt), wf.Retry[mOld](wf.Const(2), a.To(end, "x")))
		next := a.To(end, "done")
		a.Route(func(aOut2, wf.Ctx) wf.Edge[mOld] { return next }, next)
		return d
	}
	a := wf.NewAgent[mOld, eOut](d)
	if end == nil {
		end = wf.NewTerminal[mEnd](d, wf.Success)
	}
	a.Spec(wf.PromptText(prompt), wf.Retry[mOld](wf.Const(2), a.To(end, "x")))
	next := a.To(end, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[mOld] { return next }, next)
	return d
}

// E1: resume --adopt journals migrated by itself when every live state
// still exists under the same name with the same result type, and the open
// attempt carries on; without --adopt the changed hash is still refused; a
// definition whose live state vanished or changed its result type is refused
// with the state listed, and nothing is journaled.
func TestAdoptACompatibleDefinition(t *testing.T) {
	v1 := migDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "a1"})
	run := h.run(v1, nil)
	old := h.next(run)
	if len(old) != 1 || old[0].State != "mOld" {
		t.Fatalf("v1 instruction = %+v", old)
	}

	fixed := adoptDef("old, fixed", false, false)
	if fixed.Hash() == v1.Hash() {
		t.Fatal("test needs two different hashes")
	}
	if _, err := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{fixed}, RunID: "a1"}).Next(run, ""); !errors.Is(err, wf.ErrDefChanged) {
		t.Fatalf("Next without adopt = %v, want ErrDefChanged", err)
	}
	for _, c := range []struct {
		name string
		def  *wf.Def
		want string
	}{
		{"vanished", migDef(true), "mOld (no such state)"},
		{"result type", adoptDef("old", true, false), "mOld (agent"},
		{"first state moved", adoptDef("old", false, true), "mOld (the definition's first state is now mEnd)"},
	} {
		_, err := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{c.def}, RunID: "a1", Adopt: true}).Next(run, "")
		if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Migration") {
			t.Errorf("%s: adopt = %v, want a refusal listing %q", c.name, err, c.want)
		}
	}
	if n := len(h.kind(run, wf.JournalMigrated)); n != 0 {
		t.Fatalf("a refused adoption journaled %d migrated records", n)
	}

	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{fixed}, RunID: "a1", Adopt: true})
	ins, err := e2.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "mOld" || ins[0].Token != old[0].Token {
		t.Fatalf("adopted instruction = %+v, %v: the open attempt carries on", ins, err)
	}
	var md wf.MigratedData
	if rs := h.kind(run, wf.JournalMigrated); len(rs) != 1 || json.Unmarshal(rs[0].Data, &md) != nil || !md.Adopted || md.FromHash != v1.Hash() || md.ToHash != fixed.Hash() {
		t.Fatalf("migrated = %+v, want one adopted record with both hashes", md)
	}
	var detail struct{ Carried map[string]string }
	_ = json.Unmarshal(h.kind(run, wf.JournalMigrated)[0].Data, &detail)
	if detail.Carried[""] != "mOld" {
		t.Fatalf("adoption omitted carried root instance: %+v", detail)
	}
	// Adopted once: an engine without --adopt now resumes at the new hash.
	e3 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{fixed}, RunID: "a1"})
	if res, err := e3.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("Submit after adoption = %+v, %v", res, err)
	}
	st, _ := e3.Status(run)
	statusOf(t, st, wf.RunEnded, "mEnd")
}

type (
	agGate struct{}
	agAns  string
	agDone struct{}
)

// adoptGateDef is mOld (an agent whose output type is out) then a reserved
// gate asking question, then a terminal.
func adoptGateDef(question string, otherOut bool) *wf.Def {
	d := wf.New("adg")
	if otherOut {
		a := wf.NewAgent[mOld, aOut2](d)
		g, done := adoptGate(d, question)
		a.Spec(wf.PromptText("p"), wf.Retry[mOld](wf.Const(2), a.To(done, "x")))
		next := a.To(g, "ask")
		a.Route(func(aOut2, wf.Ctx) wf.Edge[mOld] { return next }, next)
		return d
	}
	a := wf.NewAgent[mOld, eOut](d)
	g, done := adoptGate(d, question)
	a.Spec(wf.PromptText("p"), wf.Retry[mOld](wf.Const(2), a.To(done, "x")))
	next := a.To(g, "ask")
	a.Route(func(eOut, wf.Ctx) wf.Edge[mOld] { return next }, next)
	return d
}

func adoptGate(d *wf.Def, question string) (*wf.Gate[agGate, agAns], *wf.Terminal[agDone]) {
	ans := wf.NewEnum(d, "ag-ans", agAns("go"))
	g := wf.NewGate[agGate, agAns](d, question, ans, wf.Reserved)
	done := wf.NewTerminal[agDone](d, wf.Success)
	g.Answers(wf.On(agAns("go"), g.To(done, "went")))
	return g, done
}

// E1 review: an instance that moved past its first state and waits at a gate
// (whose arrival journals nothing) adopts a definition that changed the gate's
// question; a definition that changed the result type of a state the run
// already finished is refused, since later states may read it; an ended run
// is never adopted.
func TestAdoptAfterTheRunMoved(t *testing.T) {
	v1 := adoptGateDef("Go?", false)
	if v1.Hash() == adoptGateDef("Go on?", false).Hash() {
		t.Fatal("test needs different hashes")
	}
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "g1"})
	run := h.run(v1, nil)
	ins := h.next(run)
	if _, err := h.e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{adoptGateDef("Go?", true)}, RunID: "g1", Adopt: true}).Next(run, ""); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "mOld (agent") {
		t.Fatalf("adopting a changed result type of a finished state = %v, want refused naming mOld", err)
	}
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{adoptGateDef("Go on?", false)}, RunID: "g1", Adopt: true})
	got, err := e2.Next(run, "")
	if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionHumanGate || got[0].Question != "Go on?" {
		t.Fatalf("adopted next = %+v, %v; want the gate with its new question", got, err)
	}
	ended := migDef(false)
	he := newHarnessOpts(t, ended, nil, wf.EngineOptions{RunID: "g2"})
	r2 := he.run(ended, nil)
	he.drive(r2, map[string][]string{"mOld": {`{}`}})
	if err := wf.NewEngine(he.j, he.reg, wf.EngineOptions{Defs: []*wf.Def{adoptDef("x", false, false)}, RunID: "g2", Adopt: true}).(*wf.Engine).Resume(r2); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "rewind") {
		t.Fatalf("resume --adopt of an ended run = %v, want refused pointing at rewind", err)
	}
}

type (
	seamOld  struct{ Value string }
	seamNew  struct{ Value int }
	seamWork struct{}
	seamEnd  struct{}
	seamSub  struct{}
	seamNext struct{}
	seamFin  struct{}
)

// seamDef is a Sub whose child is one agent ending at seamEnd, yielding Old
// (or New when changed), then a parent agent and a terminal.
func seamDef(changed bool) *wf.Def {
	kid := wf.New("seam-kid")
	w := wf.NewAgent[seamWork, eOut](kid)
	end := wf.NewTerminal[seamEnd](kid, wf.Success)
	w.Spec(wf.PromptText("w"), wf.Retry[seamWork](wf.Const(1), w.To(end, "x")))
	toEnd := w.To(end, "done")
	w.Route(func(eOut, wf.Ctx) wf.Edge[seamWork] { return toEnd }, toEnd)
	d := wf.New("seam")
	var sub interface {
		To(wf.State, string) wf.Edge[seamSub]
	}
	if changed {
		wf.Yield(end, func(wf.Ctx) seamNew { return seamNew{} })
		s := wf.NewSub[seamSub, chItem, seamNew](d, wf.Typed[chItem, seamNew](kid), func(wf.Ctx) chItem { return chItem{} })
		sub = s
		defer func() { s.Exits(wf.OnExit[seamEnd](s.To(seamNextOf(d), "child done"))) }()
	} else {
		wf.Yield(end, func(wf.Ctx) seamOld { return seamOld{} })
		s := wf.NewSub[seamSub, chItem, seamOld](d, wf.Typed[chItem, seamOld](kid), func(wf.Ctx) chItem { return chItem{} })
		sub = s
		defer func() { s.Exits(wf.OnExit[seamEnd](s.To(seamNextOf(d), "child done"))) }()
	}
	_ = sub
	return d
}

var seamNexts = map[*wf.Def]*wf.Agent[seamNext, eOut]{}

// seamNextOf is d's parent agent after the Sub, declared once.
func seamNextOf(d *wf.Def) *wf.Agent[seamNext, eOut] {
	if a, ok := seamNexts[d]; ok {
		return a
	}
	a := wf.NewAgent[seamNext, eOut](d)
	fin := wf.NewTerminal[seamFin](d, wf.Success)
	a.Spec(wf.PromptText("next"), wf.Retry[seamNext](wf.Const(1), a.To(fin, "x")))
	toFin := a.To(fin, "done")
	a.Route(func(eOut, wf.Ctx) wf.Edge[seamNext] { return toFin }, toFin)
	seamNexts[d] = a
	return a
}

// Cross-review 1: a settled child's result type is part of what a run keeps.
// A child that yielded Old{Value string} is replaced, under the same state
// names, by one yielding New{Value int} while the parent waits at its next
// agent: adoption is refused, naming the child's seam.
func TestAdoptRefusesAChangedChildResult(t *testing.T) {
	v1 := seamDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "s1"})
	run := h.run(v1, nil)
	ins := h.next(run)
	if len(ins) != 1 || ins[0].State != "seam-kid/seamWork" {
		t.Fatalf("first instruction %+v", ins)
	}
	if _, err := h.e.Submit(run, wf.Submission{Token: ins[0].Token, Child: ins[0].Child, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if next := h.next(run); len(next) != 1 || next[0].State != "seamNext" {
		t.Fatalf("parent at %+v, want seamNext", next)
	}
	_, err := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{seamDef(true)}, RunID: "s1", Adopt: true}).Next(run, "")
	if !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "seam-kid") || !strings.Contains(err.Error(), "Value int") {
		t.Fatalf("adopting a changed child result = %v, want refused naming the seam", err)
	}
}

// G1 cross-review: a refused resume --adopt of a stopped run (here: no
// operator named) on a changed, compatible definition is refused before
// anything is journaled: no
// migrated record, so the binary that stopped it still reads it.
func TestAgentResumeAdoptOfAStoppedRunAppendsNothing(t *testing.T) {
	v1 := migDef(false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "st1"})
	run := h.run(v1, nil)
	h.next(run)
	if err := h.e.(*wf.Engine).Stop(run, "operator"); err != nil {
		t.Fatal(err)
	}
	before := len(h.records(run))
	e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{adoptDef("old, fixed", false, false)}, RunID: "st1", Adopt: true}).(*wf.Engine)
	if err := e2.ResumeAs(run, wf.Operator{Session: "s1"}); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--operator") {
		t.Fatalf("unnamed resume --adopt = %v, want the stop refusal", err)
	}
	if n := len(h.records(run)); n != before {
		t.Fatalf("the refused resume appended %d records", n-before)
	}
	if _, err := h.e.Status(run); err != nil {
		t.Fatalf("the stopping binary cannot read the run: %v", err)
	}
}

func TestDeclaredMigrationJournalsAssetChoice(t *testing.T) {
	old := promptHashDef("old {{skillsRoot}}")
	old.Param("skillsRoot")
	next := promptHashDef("new {{skillsRoot}}")
	next.Param("skillsRoot")
	next.Migrations(wf.Migration{FromHash: old.Hash(), ToHash: next.Hash()})
	oldRoot, newRoot := t.TempDir(), t.TempDir()
	h := newHarnessOpts(t, old, nil, wf.EngineOptions{RunID: "migration-assets"})
	run := h.run(old, map[string]string{"skillsRoot": oldRoot})
	h.next(run)
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{next}, Adopt: true, Assets: newRoot}).(*wf.Engine)
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	recs := h.kind(run, wf.JournalMigrated)
	if len(recs) != 1 {
		t.Fatalf("migrations: %v", recs)
	}
	var data wf.MigratedData
	if err := json.Unmarshal(recs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Assets != newRoot {
		t.Errorf("migration lost explicit assets: %+v", data)
	}
	e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{next}}).(*wf.Engine)
	fetched, err := e.PromptFetch(run, "pfA#1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fetched.Prompt, newRoot) || strings.Contains(fetched.Prompt, oldRoot) {
		t.Fatalf("migration did not persist selected root: %s", fetched.Prompt)
	}
}
