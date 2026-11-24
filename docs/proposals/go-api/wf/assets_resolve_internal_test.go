package wf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type (
	riA    struct{}
	riDone struct{}
)

func riDef(root string) *Def {
	d := New("assets-internal").Assets(root)
	a := NewAgent[riA, string](d)
	done := NewTerminal[riDone](d, Success)
	a.Spec(PromptText("run it", Bound("SCRIPT", "{assets}/scripts/tool")), Retry[riA](Const(1), a.To(done, "stop")))
	fin := a.To(done, "done")
	a.Route(func(string, Ctx) Edge[riA] { return fin }, fin)
	return d
}

// A resolver's confined read never reads the host's asset paths first: the
// structural inspection it builds on does no asset reads (an earlier revision final
// review, 1).
func TestResolveAssetsReadsNothingThroughTheHost(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts/tool"), []byte("echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	d := riDef(root)
	e := NewEngine(FileJournal(state), NewAdapters(), EngineOptions{Defs: []*Def{d}, Workspace: t.TempDir(), StateRoot: state,
		RunDir: func(r RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hostReads []string
	hostAssetReadHook = func(p string) { hostReads = append(hostReads, p) }
	defer func() { hostAssetReadHook = nil }()
	cfg := MainConfig{Defs: []Registration{{Def: riDef(root)}}, Binary: "super"}
	for _, args := range [][]string{{"--state-dir", state, "--run-id", string(run)}, {"--assets", root, "--state-dir", t.TempDir()}} {
		a, _, err := ResolveAssets(cfg, args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Read("scripts/tool"); err != nil {
			t.Fatal(err)
		}
	}
	if len(hostReads) > 0 {
		t.Fatalf("the resolver read through the host: %v", hostReads)
	}
}

// The served entry's metadata is the captured bytes', whatever the live
// path is now: a verified hash never classifies it as a directory (an earlier revision
// final review, 2).
func TestServedAssetStatIsTheCapturedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts/tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s := servedAsset{confinedAssets: confinedAssets{r}, rel: "scripts/tool", data: []byte("echo")}
	info, err := s.Stat("scripts/tool")
	if err != nil || !info.Mode().IsRegular() || info.Size() != 4 {
		t.Fatalf("stat %v %v, want the captured regular file", info, err)
	}
	contents, _ := assetContentsIn(dir, Inspect(riDef(dir)).States, s)
	if !strings.HasPrefix(contents["scripts/tool"], "sha256:") {
		t.Fatalf("contents %v, want the captured bytes hashed", contents)
	}
}
