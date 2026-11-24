package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	apAsk  struct{}
	apOut  struct{ OK bool }
	apDone struct{}
)

// pinDef is one agent whose prompt is read from {assets}/p.md, pinned to
// root (none when root is empty).
func pinDef(root string) *wf.Def {
	d := wf.New("asset-pin")
	a := wf.NewAgent[apAsk, apOut](d)
	end := wf.NewTerminal[apDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	done := a.To(end, "done")
	a.Spec(wf.PromptFile("{assets}/p.md"), wf.Retry[apAsk](wf.Const(1), done))
	a.Route(func(apOut, wf.Ctx) wf.Edge[apAsk] { return done }, done)
	if root != "" {
		d.Assets(root)
	}
	return d
}

func pinRoot(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "p.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// migratedRoots is the AssetRoots of run's last migrated record.
func migratedRoots(t *testing.T, h *harness, run wf.RunID) map[string]string {
	t.Helper()
	recs := h.kind(run, wf.JournalMigrated)
	if len(recs) == 0 {
		t.Fatal("no migrated record")
	}
	var d wf.MigratedData
	if err := json.Unmarshal(recs[len(recs)-1].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.AssetRoots
}

func onlyRoot(roots map[string]string) string {
	for _, r := range roots {
		return r
	}
	return ""
}

// An adoption onto a new definition takes that definition's pinned assets:
// its new states use the new prompts. One whose definition pins none, over a run that
// recorded some, is refused until --assets names them; never a mix.
func TestAdoptionTakesTheNewDefinitionsPinnedAssets(t *testing.T) {
	oldRoot, newRoot := pinRoot(t, "OLD prompt"), pinRoot(t, "NEW prompt")
	v1 := pinDef(oldRoot)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-adopt"})
	run := h.run(v1, nil)
	h.next(run)

	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef(newRoot)}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(run); err != nil {
		t.Fatal(err)
	}
	if got := onlyRoot(migratedRoots(t, h, run)); got != newRoot {
		t.Errorf("the adoption kept assets %s, want the new definition's %s", got, newRoot)
	}

	h2 := newHarnessOpts(t, pinDef(oldRoot), nil, wf.EngineOptions{RunID: "pin-none"})
	run2 := h2.run(pinDef(oldRoot), nil)
	h2.next(run2)
	bare := wf.NewEngine(h2.j, h2.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef("")}, Adopt: true}).(*wf.Engine)
	if _, _, err := bare.AdoptOnly(run2); err == nil || !strings.Contains(err.Error(), "--assets") {
		t.Fatalf("an adoption whose definition pins no assets = %v, want a refusal naming --assets", err)
	}
	if n := len(h2.kind(run2, wf.JournalMigrated)); n != 0 {
		t.Fatalf("the refused adoption journaled %d migrations", n)
	}
	chosen := wf.NewEngine(h2.j, h2.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef("")}, Adopt: true, Assets: newRoot}).(*wf.Engine)
	if _, _, err := chosen.AdoptOnly(run2); err != nil {
		t.Fatal(err)
	}
	if got := onlyRoot(migratedRoots(t, h2, run2)); got != newRoot {
		t.Errorf("with --assets the adoption took %s, want %s", got, newRoot)
	}
}

// A fork (here a rewind) of a run, adopted onto the new definition, takes
// that definition's pin too.
func TestForkAdoptionTakesTheNewDefinitionsPinnedAssets(t *testing.T) {
	oldRoot, newRoot := pinRoot(t, "OLD prompt"), pinRoot(t, "NEW prompt")
	v1 := pinDef(oldRoot)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-fork"})
	run := h.run(v1, nil)
	in := h.next(run)[0]
	if _, err := h.e.Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	rep, err := h.e.(*wf.Engine).Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef(newRoot)}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(rep.Run); err != nil {
		t.Fatal(err)
	}
	if got := onlyRoot(migratedRoots(t, h, rep.Run)); got != newRoot {
		t.Errorf("the fork's adoption kept assets %s, want the new definition's %s", got, newRoot)
	}
}

// An adoption with no --assets onto a definition that pins its own records
// that root as the run's top-level choice, the legacy skillsRoot binding
// included: a rebuilt engine renders the new prompt (assets pin review
// round 1).
func TestAnImplicitAdoptionPersistsTheNewTopLevelRoot(t *testing.T) {
	oldRoot, newRoot := pinRoot(t, "OLD prompt"), pinRoot(t, "NEW prompt")
	v1 := pinDef(oldRoot)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-top", Assets: oldRoot})
	run := h.run(v1, map[string]string{"skillsRoot": oldRoot})
	in := h.next(run)[0]

	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef(newRoot)}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(run); err != nil {
		t.Fatal(err)
	}
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{pinDef(newRoot)}}).(*wf.Engine)
	f, err := fresh.PromptFetch(run, in.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Prompt, "NEW prompt") {
		t.Errorf("after the adoption the prompt renders %q, want the new definition's", f.Prompt)
	}
	var started wf.RunStartedData
	_ = json.Unmarshal(h.kind(run, wf.JournalRunStarted)[0].Data, &started)
	recs := h.kind(run, wf.JournalMigrated)
	var m wf.MigratedData
	_ = json.Unmarshal(recs[len(recs)-1].Data, &m)
	if m.Assets != newRoot {
		t.Errorf("the adoption journaled Assets %q, want the new top-level root %s (so the fold moves skillsRoot too)", m.Assets, newRoot)
	}
}

// An adoption onto roots that lack an asset a state needs is refused, and
// the journal is unchanged (assets pin review round 1).
func TestAnAdoptionOntoRootsMissingAnAssetIsRefused(t *testing.T) {
	oldRoot := pinRoot(t, "OLD prompt")
	empty := t.TempDir()
	v1 := pinDef(oldRoot)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-missing"})
	run := h.run(v1, nil)
	h.next(run)
	n := len(h.records(run))
	for _, opts := range []wf.EngineOptions{
		{Defs: []*wf.Def{pinDef(empty)}, Adopt: true},
		{Defs: []*wf.Def{pinDef("")}, Adopt: true, Assets: empty},
	} {
		e := wf.NewEngine(h.j, h.reg, opts).(*wf.Engine)
		if _, _, err := e.AdoptOnly(run); err == nil || !strings.Contains(err.Error(), "p.md") {
			t.Errorf("an adoption onto roots without p.md: %v, want a refusal naming it", err)
		}
		if got := len(h.records(run)); got != n {
			t.Errorf("the refused adoption changed the journal: %d records, was %d", got, n)
		}
	}
}

type (
	npAsk  struct{}
	npSub  struct{}
	npDone struct{}
	nkWork struct{}
	nkEnd  struct{}
)

// nestDef is a parent agent, then a Sub running a child definition. The
// parent's prompt is {assets}/p.md when parentFile, else inline text; the
// parent is pinned to parentRoot and the child to childRoot (none when
// empty).
func nestDef(parentRoot, childRoot string, parentFile bool) *wf.Def {
	kid := wf.New("asset-kid")
	w := wf.NewAgent[nkWork, apOut](kid)
	kend := wf.NewTerminal[nkEnd](kid, wf.Success)
	kid.OnFailure(wf.Blocked, kend)
	toK := w.To(kend, "done")
	w.Spec(wf.PromptText("kid"), wf.Retry[nkWork](wf.Const(1), toK))
	w.Route(func(apOut, wf.Ctx) wf.Edge[nkWork] { return toK }, toK)
	wf.Yield(kend, func(wf.Ctx) chRes { return chRes{} })
	if childRoot != "" {
		kid.Assets(childRoot)
	}
	d := wf.New("asset-parent")
	a := wf.NewAgent[npAsk, apOut](d)
	end := wf.NewTerminal[npDone](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	s := wf.NewSub[npSub, chItem, chRes](d, wf.Typed[chItem, chRes](kid), func(wf.Ctx) chItem { return chItem{} })
	s.Exits(wf.OnExit[nkEnd](s.To(end, "child done")))
	toSub := a.To(s, "go")
	prompt := wf.PromptText("parent")
	if parentFile {
		prompt = wf.PromptFile("{assets}/p.md")
	}
	a.Spec(prompt, wf.Retry[npAsk](wf.Const(1), a.To(end, "gave up")))
	a.Route(func(apOut, wf.Ctx) wf.Edge[npAsk] { return toSub }, toSub)
	if parentRoot != "" {
		d.Assets(parentRoot)
	}
	return d
}

// An implicit adoption that would lose the run's recorded top-level choice
// (the new definition pins only its child) is refused, naming --assets,
// and journals nothing; with --assets it goes ahead (assets pin review
// round 2).
func TestAnImplicitAdoptionNeverDropsTheRecordedTopLevelRoot(t *testing.T) {
	parentRoot, childRoot := pinRoot(t, "PARENT prompt"), pinRoot(t, "KID")
	v1 := nestDef(parentRoot, childRoot, true)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-lose", Assets: ""})
	run := h.run(v1, map[string]string{"skillsRoot": parentRoot})
	h.next(run)
	n := len(h.records(run))
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{nestDef("", childRoot, false)}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(run); err == nil || !strings.Contains(err.Error(), "--assets") {
		t.Fatalf("an adoption losing the top-level root = %v, want a refusal naming --assets", err)
	}
	if got := len(h.records(run)); got != n {
		t.Fatalf("the refused adoption changed the journal: %d records, was %d", got, n)
	}
	chosen := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{nestDef("", childRoot, false)}, Adopt: true, Assets: parentRoot}).(*wf.Engine)
	if _, _, err := chosen.AdoptOnly(run); err != nil {
		t.Fatalf("with --assets: %v", err)
	}
}

// Every definition's assets are checked, an unpinned one against no root:
// a parent that now reads {assets}/p.md while only its child is pinned is
// refused before journaling (assets pin review round 2).
func TestAnAdoptionChecksTheAssetsOfAnUnpinnedParent(t *testing.T) {
	childRoot := pinRoot(t, "KID")
	v1 := nestDef("", childRoot, false)
	h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "pin-parent"})
	run := h.run(v1, nil)
	h.next(run)
	n := len(h.records(run))
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{nestDef("", childRoot, true)}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(run); err == nil || !strings.Contains(err.Error(), "p.md") {
		t.Fatalf("an adoption whose unpinned parent reads {assets}/p.md = %v, want a refusal naming p.md", err)
	}
	if got := len(h.records(run)); got != n {
		t.Fatalf("the refused adoption changed the journal: %d records, was %d", got, n)
	}
}
