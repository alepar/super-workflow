package wf_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	raA    struct{}
	raDone struct{}
)

// assetRoot is an asset root holding the script a verb runs (referenced by
// the definition's prompt) and a file the definition never references.
func assetRoot(t *testing.T, script string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"scripts/tool": script, "scripts/unreferenced": "echo no"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// assetsDef names {assets}/scripts/tool in a prompt input, as examplewf's
// GRAPH_SHAPE_SCRIPT does, so the run pins its bytes.
func assetsDef(root string) *wf.Def {
	d := wf.New("assets-verb").Assets(root)
	a := wf.NewAgent[raA, string](d)
	done := wf.NewTerminal[raDone](d, wf.Success)
	a.Spec(wf.PromptText("run it", wf.Bound("SCRIPT", "{assets}/scripts/tool")), wf.Retry[raA](wf.Const(1), a.To(done, "stop")))
	fin := a.To(done, "done")
	a.Route(func(string, wf.Ctx) wf.Edge[raA] { return fin }, fin)
	return d
}

// startRun starts d in state with the given --assets choice (empty: the
// declared root) and returns its id.
func startRun(t *testing.T, d *wf.Def, state, assets string) wf.RunID {
	t.Helper()
	e := wf.NewEngine(wf.FileJournal(state), wf.NewAdapters(), wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: t.TempDir(), StateRoot: state, Assets: assets,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// With no run, a verb runs from the definition's declared root, or from
// --assets; the verb's own arguments are left for it.
func TestResolveAssetsWithoutARun(t *testing.T) {
	declared, explicit := assetRoot(t, "echo declared"), assetRoot(t, "echo explicit")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(declared)}}, Binary: "super"}
	a, rest, err := wf.ResolveAssets(cfg, []string{"root-epic", "--state-dir", t.TempDir()})
	if err != nil || a.Root != declared || a.Pinned || strings.Join(rest, " ") != "root-epic" {
		t.Fatalf("declared: %+v %v %v", a, rest, err)
	}
	if b, err := a.Read("scripts/tool"); err != nil || string(b) != "echo declared" {
		t.Fatalf("read %q %v", b, err)
	}
	a, _, err = wf.ResolveAssets(cfg, []string{"--assets", explicit, "--state-dir", t.TempDir(), "root-epic"})
	if err != nil || a.Root != explicit || a.Pinned {
		t.Fatalf("--assets: %+v %v", a, err)
	}
}

// With a run, a verb runs the bytes the run pinned: its recorded root, even
// where the binary now declares another, and only an asset the definition
// references, unchanged since the pin. Nothing escapes the root. The bytes a
// verb gets are the ones the check hashed.
func TestResolveAssetsOfARunIsItsPin(t *testing.T) {
	pinned, declared := assetRoot(t, "echo pinned"), assetRoot(t, "echo declared")
	state := t.TempDir()
	run := startRun(t, assetsDef(declared), state, pinned)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(declared)}}, Binary: "super"}
	a, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", state, "--run-id", string(run)})
	if err != nil || a.Root != pinned || !a.Pinned || a.Run != run {
		t.Fatalf("resolve: %+v %v", a, err)
	}
	if b, err := a.Read("scripts/tool"); err != nil || string(b) != "echo pinned" {
		t.Fatalf("pinned read %q %v", b, err)
	}
	// Materialize hands a path-only verb a private copy of the verified
	// bytes: a later change to the asset never reaches it.
	p, err := a.Materialize("scripts/tool")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	if strings.HasPrefix(p, pinned) {
		t.Fatalf("materialized in the asset root: %s", p)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o500 {
		t.Fatalf("materialized %s: %v %v", p, fi.Mode(), err)
	}
	if _, err := a.Read("scripts/unreferenced"); err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("an unreferenced asset: %v", err)
	}
	for _, rel := range []string{"../escape", "/etc/passwd", "scripts/missing"} {
		if _, err := a.Read(rel); err == nil {
			t.Fatalf("%s was served", rel)
		}
	}
	if err := os.WriteFile(filepath.Join(pinned, "scripts/tool"), []byte("echo changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read("scripts/tool"); err == nil || !strings.Contains(err.Error(), "adopt") {
		t.Fatalf("changed bytes: %v", err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "echo pinned" {
		t.Fatalf("the materialized copy changed: %q %v", b, err)
	}
}

// The run is found as the driving verbs find it: <state-dir>/run-id, else
// the only run; the journal is read without its lock.
func TestResolveAssetsFindsTheOnlyRun(t *testing.T) {
	pinned := assetRoot(t, "echo pinned")
	state := t.TempDir()
	run := startRun(t, assetsDef(pinned), state, "")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(t.TempDir())}}, Binary: "super", DefaultStateDir: state}
	a, _, err := wf.ResolveAssets(cfg, nil)
	if err != nil || a.Run != run || a.Root != pinned || !a.Pinned {
		t.Fatalf("resolve: %+v %v", a, err)
	}
}

// No root anywhere is an error naming --assets.
func TestResolveAssetsWithNoRoot(t *testing.T) {
	d := wf.New("no-assets")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Binary: "super"}
	if _, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "--assets") {
		t.Fatalf("no root: %v", err)
	}
}

// A run named with --run-id that has no journal is an error, never a quiet
// fall back to the declared root.
func TestResolveAssetsOfAMissingNamedRun(t *testing.T) {
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(assetRoot(t, "echo"))}}, Binary: "super"}
	if _, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", t.TempDir(), "--run-id", "nosuch"}); err == nil || !strings.Contains(err.Error(), "nosuch") {
		t.Fatalf("a missing named run: %v", err)
	}
}

// A link inside the root whose target lies outside it is never followed,
// for reading or for the pin's hash, pinned or not (an earlier revision review round 1).
func TestResolveAssetsRefusesALinkOutOfTheRoot(t *testing.T) {
	root := assetRoot(t, "echo in root")
	outside := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(outside, []byte("echo outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "scripts/tool")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "scripts/tool")); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	run := startRun(t, assetsDef(root), state, root)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(root)}}, Binary: "super"}
	for name, args := range map[string][]string{
		"pinned":   {"--state-dir", state, "--run-id", string(run)},
		"explicit": {"--assets", root, "--state-dir", t.TempDir()},
	} {
		a, _, err := wf.ResolveAssets(cfg, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, err := a.Read("scripts/tool"); err == nil {
			t.Fatalf("%s: read through the link: %q", name, b)
		}
	}
}

// Run selection is the driving verbs': several runs and no pointer, or a
// pointer to a run with no journal, is an error naming --run-id, never a
// quiet fall back to the declared root (an earlier revision review round 1).
func TestResolveAssetsRefusesAnAmbiguousOrStaleRun(t *testing.T) {
	root := assetRoot(t, "echo")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(root)}}, Binary: "super"}
	state := t.TempDir()
	startRun(t, assetsDef(root), state, "")
	e := wf.NewEngine(wf.FileJournal(state), wf.NewAdapters(), wf.EngineOptions{Defs: []*wf.Def{assetsDef(root)}, Workspace: t.TempDir(), StateRoot: state, RunID: "second",
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	if _, err := e.Run(assetsDef(root), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", state}); err == nil || !strings.Contains(err.Error(), "--run-id") {
		t.Fatalf("two runs: %v", err)
	}
	stale := t.TempDir()
	if err := os.WriteFile(filepath.Join(stale, "run-id"), []byte("gone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", stale}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a stale pointer: %v", err)
	}
}

// --assets wins before any run lookup: a missing --run-id beside it is not
// consulted (an earlier revision review round 1).
func TestResolveAssetsExplicitRootWinsFirst(t *testing.T) {
	root := assetRoot(t, "echo explicit")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(t.TempDir())}}, Binary: "super"}
	a, _, err := wf.ResolveAssets(cfg, []string{"--assets", root, "--state-dir", t.TempDir(), "--run-id", "nosuch"})
	if err != nil || a.Root != root || a.Pinned {
		t.Fatalf("explicit root: %+v %v", a, err)
	}
}

// Only a missing pointer or runs directory is absence; any other discovery
// error is an error, never a quiet fall back to the declared root (an earlier revision
// final review, 3).
func TestResolveAssetsReportsDiscoveryErrors(t *testing.T) {
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: assetsDef(assetRoot(t, "echo"))}}, Binary: "super"}
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "runs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", state}); err == nil {
		t.Fatalf("runs is a file: resolved %+v", a)
	}
	pointer := t.TempDir()
	if err := os.Mkdir(filepath.Join(pointer, "run-id"), 0o755); err != nil {
		t.Fatal(err)
	}
	if a, _, err := wf.ResolveAssets(cfg, []string{"--state-dir", pointer}); err == nil {
		t.Fatalf("an unreadable pointer: resolved %+v", a)
	}
}
