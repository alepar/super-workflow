package wf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitWorkspace is a repository with history, a branch, a tag, a staged and
// an unstaged change and an untracked file, under an isolated git config.
func gitWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "T")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, ws, "init", "-q", "-b", "main")
	for i, body := range []string{"one\n", "two\n", "three\n"} {
		if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, ws, "add", "a.txt")
		gitT(t, ws, "commit", "-q", "-m", "c"+string(rune('0'+i)))
	}
	gitT(t, ws, "tag", "-a", "-m", "v", "v1")
	gitT(t, ws, "branch", "side", "HEAD~1")
	gitT(t, ws, "repack", "-adq")
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("staged\n"), 0o644)
	gitT(t, ws, "add", "a.txt")
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("unstaged\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "new.txt"), []byte("untracked\n"), 0o644)
	return ws
}

func gitEngine(t *testing.T, ws, state string, d *wf.Def) *wf.Engine {
	t.Helper()
	reg := wf.NewAdapters()
	if err := reg.Register(git.Runtime(git.Config{})); err != nil {
		t.Fatal(err)
	}
	return wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
}

// A checkpoint hands the repository's object files to the run's store
// instead of writing a bundle of its whole history: a later checkpoint of the
// same history stores nothing new, and a fork of it restores branches, tags,
// HEAD, the staged index and the working files.
func TestSnapshotGitObjectsDedupeAndFork(t *testing.T) {
	ws := gitWorkspace(t)
	state := t.TempDir()
	d := fanDef("git-objects", chItems(1, 2, 3), 1, wf.AllSettled())
	run, err := gitEngine(t, ws, state, d).Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int64
	storeBytes := func() int64 {
		var n int64
		filepath.Walk(filepath.Join(state, "runs", string(run), "snapshots", "store.git", "objects"), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				n += fi.Size()
			}
			return nil
		})
		return n
	}
	for i := 0; i < 3; i++ {
		ins, err := gitEngine(t, ws, state, d).Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range withoutEnded(ins) {
			if _, err := gitEngine(t, ws, state, d).Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
		}
		sizes = append(sizes, storeBytes())
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var forkAt int64
	for _, r := range recs {
		var w wf.WorldSnapshotData
		if r.Kind != wf.JournalWorldSnapshot || json.Unmarshal(r.Data, &w) != nil {
			continue
		}
		if w.Unavailable != "" {
			t.Fatalf("checkpoint %d unavailable: %s", r.Seq, w.Unavailable)
		}
		var meta struct {
			Version int
			Refs    map[string]string
		}
		if err := json.Unmarshal(w.Adapters["git"], &meta); err != nil || meta.Version != 3 || meta.Refs["refs/heads/side"] == "" || meta.Refs["refs/tags/v1"] == "" {
			t.Fatalf("git checkpoint metadata %s: %v", w.Adapters["git"], err)
		}
		forkAt = r.Seq
	}
	// The world did not change between the later checkpoints: each grew the
	// store by its own small trees and commits only, never the history again.
	if grow := sizes[2] - sizes[1]; grow > 64<<10 {
		t.Fatalf("an unchanged checkpoint grew the store by %d bytes (sizes %v)", grow, sizes)
	}
	wantHead, wantSide, wantTag := gitT(t, ws, "rev-parse", "HEAD"), gitT(t, ws, "rev-parse", "side"), gitT(t, ws, "rev-parse", "v1")
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("later\n"), 0o644)
	// A host whose git defaults to reftable restores it too (the adapter
	// runs git with the caller's GIT_ variables unset, so HOME's config).
	cfg := "[init]\n\tdefaultRefFormat = reftable\n"
	for _, p := range []string{filepath.Join(os.Getenv("HOME"), ".gitconfig"), os.Getenv("GIT_CONFIG_GLOBAL")} {
		if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fork, err := gitEngine(t, ws, state, d).ForkSnapshot(run, forkAt, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	fw := fork.Workspace
	if got := gitT(t, fw, "rev-parse", "HEAD"); got != wantHead {
		t.Fatalf("fork HEAD %s want %s", got, wantHead)
	}
	if gitT(t, fw, "rev-parse", "side") != wantSide || gitT(t, fw, "rev-parse", "v1") != wantTag {
		t.Fatal("fork lost a branch or tag")
	}
	if got := gitT(t, fw, "show", ":a.txt"); got != "staged" {
		t.Fatalf("fork index a.txt %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(fw, "a.txt")); string(b) != "unstaged\n" {
		t.Fatalf("fork a.txt %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(fw, "new.txt")); string(b) != "untracked\n" {
		t.Fatalf("fork new.txt %q", b)
	}
	if out := gitT(t, fw, "fsck", "--connectivity-only", "--no-dangling"); strings.Contains(out, "missing") {
		t.Fatalf("fork repository incomplete: %s", out)
	}
}

// A gc between captures renames the packs: the next capture stores the
// repacked history once, then dedupes again, and a fork of it restores.
func TestSnapshotGitObjectsAfterRepack(t *testing.T) {
	ws := gitWorkspace(t)
	state := t.TempDir()
	d := fanDef("git-repack", chItems(1, 2, 3), 1, wf.AllSettled())
	run, err := gitEngine(t, ws, state, d).Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	storeBytes := func() int64 {
		var n int64
		filepath.Walk(filepath.Join(state, "runs", string(run), "snapshots", "store.git", "objects"), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				n += fi.Size()
			}
			return nil
		})
		return n
	}
	step := func() {
		ins, err := gitEngine(t, ws, state, d).Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range withoutEnded(ins) {
			if _, err := gitEngine(t, ws, state, d).Submit(run, wf.Submission{Token: in.Token, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	step()
	before := gitT(t, ws, "count-objects", "-v")
	os.WriteFile(filepath.Join(ws, "b.txt"), []byte("b\n"), 0o644)
	gitT(t, ws, "add", "b.txt")
	gitT(t, ws, "commit", "-q", "-m", "b")
	gitT(t, ws, "gc", "-q", "--prune=now")
	if after := gitT(t, ws, "count-objects", "-v"); after == before {
		t.Fatal("gc changed nothing: the probe is moot")
	}
	step()
	grown := storeBytes()
	step()
	if extra := storeBytes() - grown; extra > 64<<10 {
		t.Fatalf("the capture after the repacked one grew the store by %d bytes", extra)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	for _, r := range recs {
		var w wf.WorldSnapshotData
		if r.Kind == wf.JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil {
			if w.Unavailable != "" {
				t.Fatalf("checkpoint %d unavailable: %s", r.Seq, w.Unavailable)
			}
			last = r.Seq
		}
	}
	fork, err := gitEngine(t, ws, state, d).ForkSnapshot(run, last, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, fork.Workspace, "log", "-1", "--format=%s"); got != "b" {
		t.Fatalf("fork HEAD subject %q", got)
	}
	gitT(t, fork.Workspace, "fsck", "--connectivity-only", "--no-dangling")
}
