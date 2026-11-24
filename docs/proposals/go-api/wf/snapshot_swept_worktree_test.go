package wf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type sweptStart struct{}
type sweptWork struct{}
type sweptEnd struct{}
type sweptDone struct{}

// sweptDef is the regression case's round end in small: a git effect adds a task
// worktree, and a later shell sweep (not a git worktree_remove effect)
// removes it before the next offer's checkpoint. The sweep runs in the
// workspace with no dir of its own, as terra2's does, so a fork can relocate
// the run.
func sweptDef(sweepArgv ...string) *wf.Def {
	d := wf.New("swept-worktree")
	start := wf.NewAgent[sweptStart, string](d)
	work := wf.NewAgent[sweptWork, string](d)
	end := wf.NewAgent[sweptEnd, string](d)
	done := wf.NewTerminal[sweptDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	start.Spec(wf.PromptText("start"), wf.Retry[sweptStart](wf.Const(1), start.To(done, "stop")))
	work.Spec(wf.PromptText("work"), wf.Retry[sweptWork](wf.Const(1), work.To(done, "stop")))
	end.Spec(wf.PromptText("end"), wf.Retry[sweptEnd](wf.Const(1), end.To(done, "stop")))
	add := start.To(work, "task workspace").With(gitadapter.WorktreeAdd(wf.KeyOf("task"), ".worktrees/task", "task", "main"))
	start.Route(func(string, wf.Ctx) wf.Edge[sweptStart] { return add }, add)
	sweep := work.To(end, "swept").With(shell.Effect(wf.KeyOf("sweep"), shell.Run(sweepArgv...)))
	work.Route(func(string, wf.Ctx) wf.Edge[sweptWork] { return sweep }, sweep)
	fin := end.To(done, "done")
	end.Route(func(string, wf.Ctx) wf.Edge[sweptEnd] { return fin }, fin)
	return d
}

// A capture after a sweep removed a run-owned worktree follows the live
// worktree list: next proceeds and the checkpoint is a restorable one without
// the removed tree. Before, next exited with "missing or symlink
// worktree" and journaled nothing.
func TestSnapshotAfterASweepRemovedATaskWorktree(t *testing.T) {
	e, run, seq, last := sweptRun(t, "git", "worktree", "remove", "--force", ".worktrees/task")
	if last.Unavailable != "" || strings.Contains(string(last.Adapters["git"]), ".worktrees/task") {
		t.Fatalf("checkpoint after the sweep: %+v", last)
	}
	// A fork from it is the swept world: no task checkout, its branch kept.
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork of the post-sweep checkpoint: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fork.Workspace, ".worktrees", "task")); !os.IsNotExist(err) {
		t.Fatalf("the fork recreated the removed checkout: %v", err)
	}
	if out, err := exec.Command("git", "-C", fork.Workspace, "rev-parse", "--verify", "refs/heads/task").CombinedOutput(); err != nil {
		t.Fatalf("the fork lost the task branch: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", fork.Workspace, "worktree", "list", "--porcelain").Output(); err != nil || strings.Count(string(out), "worktree ") != 1 {
		t.Fatalf("the fork's worktrees: %v %s", err, out)
	}
}

// A run-owned worktree that went missing any other way (deleted, still
// registered) is no checkpoint, but it never refuses next: the capture is
// journaled unavailable with why.
func TestSnapshotOfAMissingTaskWorktreeIsUnavailable(t *testing.T) {
	_, _, _, last := sweptRun(t, "rm", "-r", "-f", ".worktrees/task")
	if !strings.Contains(last.Unavailable, "missing or symlink worktree") {
		t.Fatalf("checkpoint after the deletion: %+v", last)
	}
}

// sweptRun drives sweptDef with sweep to its last offer and returns the
// engine, the run and its last checkpoint with that checkpoint's seq; every
// next must succeed.
func sweptRun(t *testing.T, sweep ...string) (*wf.Engine, wf.RunID, int64, wf.WorldSnapshotData) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	workspace, state := t.TempDir(), t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Snapshot Test")
	git("config", "user.email", "snapshot@example.invalid")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "fixture")
	adapters := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), shell.Runtime(shell.Config{})} {
		if err := adapters.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	def := sweptDef(sweep...)
	e := wf.NewEngine(wf.FileJournal(state), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: workspace, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	run, err := e.Run(def, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"start", "work", "end"} {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatalf("next before %s: %v", want, err)
		}
		if len(ins) != 1 {
			t.Fatalf("offers before %s: %+v", want, ins)
		}
		if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
			t.Fatalf("submit %s: %v", want, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".worktrees", "task")); !os.IsNotExist(err) {
		t.Fatalf("sweep left the task worktree: %v", err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var last *wf.WorldSnapshotData
	var lastSeq int64
	var sawTask bool
	for _, r := range recs {
		if r.Kind != wf.JournalWorldSnapshot {
			continue
		}
		var d wf.WorldSnapshotData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		sawTask = sawTask || strings.Contains(string(d.Adapters["git"]), ".worktrees/task")
		last, lastSeq = &d, r.Seq
	}
	if !sawTask {
		t.Fatal("no checkpoint captured the task worktree while it existed")
	}
	if last == nil {
		t.Fatal("no checkpoint")
	}
	return e, run, lastSeq, *last
}
