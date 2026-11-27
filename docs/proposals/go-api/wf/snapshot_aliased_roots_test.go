package wf_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	bdadapter "github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A workspace, state root and fork destination reached through a symbolic
// link above them (as macOS's /tmp and /var lead into /private) checkpoint,
// fork and resume as if named by their real paths: the engine resolves the
// roots it is handed once, and the adapters' refusal of links inside those
// roots still holds (sw-vi0h).
func TestSnapshotForkThroughAliasedRoots(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	ws, state := filepath.Join(alias, "ws"), filepath.Join(alias, "state")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Snapshot Test")
	git("config", "user.email", "snapshot@example.invalid")
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "data")
	git("commit", "-qm", "initial")

	reg := wf.NewAdapters()
	_ = reg.Register(gitadapter.Runtime(gitadapter.Config{}))
	_ = reg.Register(bdadapter.Runtime(bdadapter.Config{}))
	d := snapshotDef()
	options := func(ws, state string) wf.EngineOptions {
		return wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
			RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}
	}
	engine := wf.NewEngine(wf.FileJournal(state), reg, options(ws, state)).(*wf.Engine)
	run, err := engine.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	if seq == 0 {
		t.Fatal("no checkpoint through the aliased roots")
	}
	// The alias still names the run's workspace: a later engine over it is
	// the same run in the same place, not a relocation.
	if _, err = wf.NewEngine(wf.FileJournal(state), reg, options(ws, state)).Next(run, ""); err != nil {
		t.Fatalf("resume through the alias: %v", err)
	}

	fork, err := engine.ForkSnapshot(run, seq, filepath.Join(alias, "forks", "one"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fork.Workspace, fork.StateDir} {
		if resolved, err := filepath.EvalSymlinks(p); err != nil || resolved != p {
			t.Errorf("fork path %s resolves to %s (%v); want its real path", p, resolved, err)
		}
	}
	resumed := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, options(fork.Workspace, fork.StateDir))
	ins, err := resumed.Next(fork.Run, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) == 0 {
		t.Fatal("restored fork did not offer work")
	}
}
