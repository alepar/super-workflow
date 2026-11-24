package wf_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bdadapter "github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/feature"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A fork that runs nothing stands where its source stood: the primary and
// every linked checkout show the source's git status, and every tracked
// file keeps its mode. Tracked .beads files (a tracker's hooks among them)
// live in the primary, owned by bd, and in each linked checkout, owned by
// git; both used to come back dirty (deleted, or without the exec bit).
func TestSnapshotForkOfATrackedTrackerIsClean(t *testing.T) {
	f := trackedTrackerFeature(t)
	fork, err := f.engine.ForkSnapshot(f.id, f.designSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	f.assertClean(t, fork)
}

// A rewind of a run captured on this engine is as clean: rewind restores
// through the same fork, so the integration checkout keeps its tracked
// .beads files and every mode (e2e-baseline: rewinds made before an earlier revision
// left the integration worktree without its 9 tracked .beads files).
func TestSnapshotRewindOfATrackedTrackerIsClean(t *testing.T) {
	f := trackedTrackerFeature(t)
	// Design fails blocked; the definition's Recovery gate is answered
	// keep, which stops the run: it dies there.
	offered, err := f.engine.Next(f.id, "")
	if err != nil || len(offered) != 1 || offered[0].State != "Design" {
		t.Fatalf("design offer: %+v %v", offered, err)
	}
	if _, err := f.engine.FailedAs(f.id, offered[0].Name, "rewind fixture", wf.FailBlocked); err != nil {
		t.Fatal(err)
	}
	if offered, err = f.engine.Next(f.id, ""); err != nil || len(offered) != 1 || offered[0].State != "Recovery" {
		t.Fatalf("recovery offer: %+v %v", offered, err)
	}
	if err := f.engine.Answer(f.id, "Recovery", string(feature.Keep)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.Next(f.id, ""); err != nil {
		t.Fatal(err)
	}
	fork, err := f.engine.RewindInto(f.id, "died", filepath.Join(t.TempDir(), "rewind"))
	if err != nil {
		t.Fatal(err)
	}
	f.assertClean(t, fork)
}

// trackedTrackerFeature is the feature definition over real git and bd in
// a workspace that tracks its tracker's files, offered its first Design.
type trackedFeature struct {
	engine           *wf.Engine
	id               wf.RunID
	workspace, state string
	integration      string
	designSeq        int64
	run              func(dir, bin string, args ...string) string
}

func trackedTrackerFeature(t *testing.T) trackedFeature {
	t.Helper()
	for _, bin := range []string{"git", "bd"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	workspace, state := t.TempDir(), t.TempDir()
	run := func(dir, bin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(workspace, "git", "init", "-q", "-b", "main")
	run(workspace, "git", "config", "user.name", "Snapshot Test")
	run(workspace, "git", "config", "user.email", "snapshot@example.invalid")
	run(workspace, "git", "config", "commit.gpgsign", "false")
	run(workspace, "bd", "init", "--quiet", "--prefix", "snap")
	run(workspace, "git", "config", "core.hooksPath", os.DevNull)
	if err := os.MkdirAll(filepath.Join(workspace, ".beads", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".beads", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".beads", "README.md"), []byte("tracker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(workspace, "git", "add", "-f", ".beads/config.yaml", ".beads/metadata.json", ".beads/README.md", ".beads/hooks/pre-commit")
	run(workspace, "git", "commit", "-qm", "fixture with a tracked tracker")
	adapters := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), shell.Runtime(shell.Config{}), bdadapter.Runtime(bdadapter.Config{})} {
		if err := adapters.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	def := feature.Definition()
	engine := wf.NewEngine(wf.FileJournal(state), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: workspace, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	id, err := engine.Run(def, map[string]string{"goal": "Add an exported addition function", "suite": "true", "goal-check-command": "go test -count=1 -json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	offered, err := engine.Next(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 1 || offered[0].State != "Design" {
		t.Fatalf("first offer: %+v", offered)
	}
	integration := offered[0].Workspace
	records, err := wf.ReadJournal(state, id)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, rec := range records {
		if rec.Kind == wf.JournalWorldSnapshot && rec.State == "Design" {
			seq = rec.Seq
		}
	}
	if seq == 0 {
		t.Fatal("missing Design checkpoint")
	}
	return trackedFeature{engine: engine, id: id, workspace: workspace, state: state, integration: integration, designSeq: seq, run: run}
}

// assertClean checks the restored primary and integration checkouts
// against the source's: the same git status, every tracked file present
// with its mode.
func (f trackedFeature) assertClean(t *testing.T, fork wf.SnapshotFork) {
	t.Helper()
	run, workspace, state, integration := f.run, f.workspace, f.state, f.integration
	rel, err := filepath.Rel(state, integration)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("integration checkout not under state: %q", integration)
	}
	checkouts := map[string]string{workspace: fork.Workspace, integration: filepath.Join(fork.StateDir, rel)}
	for source, restored := range checkouts {
		if got, want := run(restored, "git", "status", "--porcelain"), run(source, "git", "status", "--porcelain"); got != want {
			t.Errorf("%s: fork status %q, source %q", restored, got, want)
		}
		for _, file := range strings.Split(run(source, "git", "ls-files"), "\n") {
			src, err := os.Lstat(filepath.Join(source, file))
			if err != nil {
				t.Fatal(err)
			}
			dst, err := os.Lstat(filepath.Join(restored, file))
			if err != nil {
				t.Errorf("%s: tracked %s missing: %v", restored, file, err)
				continue
			}
			if src.Mode()&(fs.ModeType|0o111) != dst.Mode()&(fs.ModeType|0o111) {
				t.Errorf("%s: %s mode %v, source %v", restored, file, dst.Mode(), src.Mode())
			}
		}
	}
}
