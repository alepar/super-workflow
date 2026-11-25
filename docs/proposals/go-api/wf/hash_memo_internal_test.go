package wf

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// countHashWork counts structure walks and hash computations of d for the
// rest of the test.
func countHashWork(t *testing.T, d *Def) (walks, hashes *atomic.Int64) {
	t.Helper()
	walks, hashes = new(atomic.Int64), new(atomic.Int64)
	prevWalk, prevHash := onStructureWalk, onHashCompute
	onStructureWalk = func(x *Def) {
		if x == d {
			walks.Add(1)
		}
	}
	onHashCompute = func(x *Def) {
		if x == d {
			hashes.Add(1)
		}
	}
	t.Cleanup(func() { onStructureWalk, onHashCompute = prevWalk, prevHash })
	return walks, hashes
}

func TestDefinitionHashWalksEachDefOnce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rules.md"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	child := New("memo-child")
	NewCommand[pfAssetCommand](child, "bash", "{assets}/rules.md")
	d := New("memo").Assets(root)
	a := NewAgent[pfAssetAgent, string](d)
	a.Spec(PromptText("read [RULES]", Bound("RULES", "{assets}/rules.md")), RetryPolicy[pfAssetAgent]{})
	d.children = []*Def{child}
	d.freeze()
	walks, hashes := countHashWork(t, d)

	first := definitionHashAssets(d, assetRoots(d, ""))
	for range 3 {
		if got := definitionHashAssets(d, assetRoots(d, "")); got != first {
			t.Fatalf("memoised hash %s differs from first %s", got, first)
		}
		if got := d.Hash(); got != first {
			t.Fatalf("Hash() %s differs from definition hash %s", got, first)
		}
	}
	if got := hashView(Inspect(d)); got != first {
		t.Fatalf("memoised hash %s differs from a fresh Inspect hash %s", first, got)
	}
	if walks.Load() != 1 || hashes.Load() != 1 {
		t.Fatalf("walks %d, hash computations %d; want 1 and 1", walks.Load(), hashes.Load())
	}

	// Assets are read on every call: a change within the process is seen.
	if err := os.WriteFile(filepath.Join(root, "rules.md"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := definitionHashAssets(d, assetRoots(d, ""))
	if changed == first {
		t.Fatal("asset change within the process not seen")
	}
	if got := hashView(Inspect(d)); got != changed {
		t.Fatalf("hash after the change %s differs from a fresh Inspect hash %s", changed, got)
	}
	if walks.Load() != 1 || hashes.Load() != 2 {
		t.Fatalf("after the change: walks %d, hash computations %d; want 1 and 2", walks.Load(), hashes.Load())
	}

	// Another root for the same definition is hashed against its own bytes.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "rules.md"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := definitionHashAssets(d, assetRoots(d, other)); got != first {
		t.Fatalf("same bytes under another root hash %s, want %s", got, first)
	}
	if walks.Load() != 1 || hashes.Load() != 2 {
		t.Fatalf("other root: walks %d, hash computations %d; want 1 and 2", walks.Load(), hashes.Load())
	}
}

// The memoised structure is shared: a caller that fills in a view's assets
// must not change what the next caller sees.
func TestInspectStructureCallersDoNotShareMutations(t *testing.T) {
	child := New("share-child")
	NewCommand[pfAssetCommand](child, "true")
	d := New("share")
	NewCommand[pfAssetCommand](d, "true")
	d.children = []*Def{child}
	d.freeze()
	v := inspectStructure(d)
	v.Assets = map[string]string{"x": "y"}
	v.Children[0].Assets = map[string]string{"x": "y"}
	v.Children = append(v.Children, DefView{Name: "extra"})
	again := inspectStructure(d)
	if again.Assets != nil || again.Children[0].Assets != nil || len(again.Children) != 1 {
		t.Fatalf("a caller's mutation leaked into the memo: %+v", again)
	}
}

// One run's verbs, on one engine and on a second one over the same journal
// (as a later process would, but sharing this one), walk and hash the
// definition once.
func TestEngineVerbsHashTheDefinitionOnce(t *testing.T) {
	d := New("memo-run")
	ask := NewAgent[cniAsk, cniOut](d)
	done := NewTerminal[cniDone](d, Success)
	ask.Spec(PromptText("ask"), Retry[cniAsk](Const(1), ask.To(done, "gave up")))
	toDone := ask.To(done, "asked")
	ask.Route(func(cniOut, Ctx) Edge[cniAsk] { return toDone }, toDone)
	walks, hashes := countHashWork(t, d)

	j := MemJournal()
	opts := EngineOptions{} // Run hands the definition over

	e := NewEngine(j, NewAdapters(), opts).(*Engine)
	id, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, eng := range []*Engine{e, NewEngine(j, NewAdapters(), EngineOptions{Defs: []*Def{d}}).(*Engine)} {
		if _, err := eng.Next(id, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := eng.Status(id); err != nil {
			t.Fatal(err)
		}
	}
	if walks.Load() != 1 || hashes.Load() != 1 {
		t.Fatalf("walks %d, hash computations %d; want 1 and 1", walks.Load(), hashes.Load())
	}
}

// A definition not yet handed to an engine may still be under construction:
// it is walked afresh, so building after a hash changes the next hash.
func TestUnfrozenDefinitionIsNotMemoised(t *testing.T) {
	d := New("unfrozen")
	a := NewAgent[pfAssetAgent, string](d)
	a.Spec(PromptText("before"), RetryPolicy[pfAssetAgent]{})
	before := d.Hash()
	a.Spec(PromptText("after"), RetryPolicy[pfAssetAgent]{})
	after := d.Hash()
	if before == after || after != hashView(Inspect(d)) {
		t.Fatalf("hash before %s, after %s, fresh %s: a change before freezing was not seen", before, after, hashView(Inspect(d)))
	}
	NewEngine(MemJournal(), NewAdapters(), EngineOptions{Defs: []*Def{d}})
	if !d.frozen.Load() {
		t.Fatal("NewEngine did not freeze its definition")
	}
	if got := d.Hash(); got != after {
		t.Fatalf("frozen hash %s, want %s", got, after)
	}
}

// A frozen definition's own state views are walked once; an unfrozen one's
// afresh.
func TestOwnStatesWalkedOncePerFrozenDef(t *testing.T) {
	d := New("own-states")
	NewCommand[pfAssetCommand](d, "true")
	if a, b := ownStates(d), ownStates(d); &a[0] == &b[0] {
		t.Fatal("an unfrozen definition's states were kept")
	}
	d.freeze()
	a, b := ownStates(d), ownStates(d)
	if &a[0] != &b[0] {
		t.Fatal("a frozen definition's states were walked again")
	}
	if a[0].Path != "own-states/"+a[0].Name {
		t.Fatalf("path %q, want the definition's own name as prefix", a[0].Path)
	}
	if v := inspectStructure(d); &v.States[0] != &a[0] {
		t.Fatal("the structural view walked the root's states again")
	}
}
