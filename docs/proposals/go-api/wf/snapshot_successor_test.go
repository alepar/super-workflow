package wf_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type succSnapStart struct{}
type succSnapWork struct{}
type succSnapDone struct{}

// succSnapDef is a keyed run whose first step adds a task worktree that
// outlives the run, as examplewf's integration worktree does.
func succSnapDef() *wf.Def {
	d := wf.New("succ-snapshot")
	d.RunKey(d.Param("topic"))
	start := wf.NewAgent[succSnapStart, string](d)
	work := wf.NewAgent[succSnapWork, string](d)
	done := wf.NewTerminal[succSnapDone](d, wf.Success)
	d.OnFailure(wf.Blocked, done)
	start.Spec(wf.PromptText("start"), wf.Retry[succSnapStart](wf.Const(1), start.To(done, "stop")))
	work.Spec(wf.PromptText("work"), wf.Retry[succSnapWork](wf.Const(1), work.To(done, "stop")))
	add := start.To(work, "task workspace").With(gitadapter.WorktreeAdd(wf.KeyOf("task"), ".worktrees/task", "task", "main"))
	start.Route(func(string, wf.Ctx) wf.Edge[succSnapStart] { return add }, add)
	fin := work.To(done, "done")
	work.Route(func(string, wf.Ctx) wf.Edge[succSnapWork] { return fin }, fin)
	return d
}

// snapshots are the world_snapshot records of run, with their seqs.
func snapshots(t *testing.T, state string, run wf.RunID) []wf.WorldSnapshotData {
	t.Helper()
	out, _ := snapshotSeqs(t, state, run)
	return out
}

func snapshotSeqs(t *testing.T, state string, run wf.RunID) ([]wf.WorldSnapshotData, []int64) {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.WorldSnapshotData
	var seqs []int64
	for _, r := range recs {
		if r.Kind != wf.JournalWorldSnapshot {
			continue
		}
		var d wf.WorldSnapshotData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out, seqs = append(out, d), append(seqs, r.Seq)
	}
	return out, seqs
}

// startedWorktrees is run's recorded inherited worktree set, nil when it
// records none.
func startedWorktrees(t *testing.T, state string, run wf.RunID) *[]string {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var d wf.RunStartedData
	if err := json.Unmarshal(recs[0].Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Worktrees
}

// driveFork runs next, submit and next on the fork with a fresh engine and
// returns the fork's checkpoints.
func driveFork(t *testing.T, adapters *wf.Adapters, def *wf.Def, fork wf.SnapshotFork) []wf.WorldSnapshotData {
	t.Helper()
	fe := wf.NewEngine(wf.FileJournal(fork.StateDir), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(run)) },
	}).(*wf.Engine)
	ins, err := fe.Next(fork.Run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("fork next: %+v %v", ins, err)
	}
	if _, err := fe.Submit(fork.Run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatalf("fork submit: %v", err)
	}
	if _, err := fe.Next(fork.Run, ""); err != nil {
		t.Fatalf("fork next: %v", err)
	}
	return snapshots(t, fork.StateDir, fork.Run)
}

// the regression case successor (example-run.2): every capture failed with
// "snapshot git preflight: git snapshot: unsupported layout: unrequested
// nested repository". The predecessor's integration worktree, which the
// successor carries on in, was a .git the successor's own journal never
// added. A successor's checkpoint captures the worktrees its predecessor
// chain added and did not remove; a genuine nested repository is still
// refused.
func TestSuccessorSnapshotsThePredecessorsWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	workspace := t.TempDir()
	state := filepath.Join(workspace, ".superpowers", "state", "succ") // inside the workspace, as examplewf's
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
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte(".worktrees/\n.superpowers/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "fixture")
	adapters := wf.NewAdapters()
	if err := adapters.Register(gitadapter.Runtime(gitadapter.Config{})); err != nil {
		t.Fatal(err)
	}
	def := succSnapDef()
	e := wf.NewEngine(wf.FileJournal(state), adapters, wf.EngineOptions{
		Defs: []*wf.Def{def}, Workspace: workspace, StateRoot: state, Snapshots: true,
		RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
	}).(*wf.Engine)
	params := map[string]string{"topic": "x"}
	first, err := e.Run(def, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"start", "work"} {
		ins, err := e.Next(first, "")
		if err != nil || len(ins) != 1 {
			t.Fatalf("next before %s: %+v %v", want, ins, err)
		}
		if _, err := e.Submit(first, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
			t.Fatalf("submit %s: %v", want, err)
		}
	}
	if st, err := e.Status(first); err != nil || st.Phase != wf.RunEnded {
		t.Fatalf("first run: %+v %v, want ended", st.Phase, err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".worktrees", "task", ".git")); err != nil {
		t.Fatalf("the predecessor's worktree is gone: %v", err)
	}

	second, err := e.Run(def, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("run on the ended run attached %s", first)
	}
	if _, err := e.Next(second, ""); err != nil {
		t.Fatalf("successor next: %v", err)
	}
	snaps := snapshots(t, state, second)
	if len(snaps) == 0 {
		t.Fatal("the successor took no checkpoint")
	}
	for _, s := range snaps {
		if s.Unavailable != "" {
			t.Fatalf("successor checkpoint unavailable: %s", s.Unavailable)
		}
	}
	if !strings.Contains(string(snaps[0].Adapters["git"]), ".worktrees/task") {
		t.Errorf("the successor's checkpoint does not carry the predecessor's worktree: %s", snaps[0].Adapters["git"])
	}
	// The set is the successor's own record: a fork carries it in the
	// copied prefix, and a capture reads no other run's journal.
	if wt := startedWorktrees(t, state, second); wt == nil || len(*wt) != 1 || filepath.Base((*wt)[0]) != "task" {
		t.Fatalf("successor run_started Worktrees = %v, want the predecessor's .worktrees/task", wt)
	}
	_, seqs := snapshotSeqs(t, state, second)
	fork, err := e.ForkSnapshot(second, seqs[0], filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	for _, s := range driveFork(t, adapters, def, fork) {
		if s.Unavailable != "" {
			t.Fatalf("a fresh capture in the fork: %s", s.Unavailable)
		}
	}
	rewound, err := e.RewindInto(second, strconv.FormatInt(seqs[0], 10), filepath.Join(t.TempDir(), "rewind"))
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	for _, s := range driveFork(t, adapters, def, rewound) {
		if s.Unavailable != "" {
			t.Fatalf("a fresh capture after the rewind: %s", s.Unavailable)
		}
	}

	// A genuine nested repository in the product tree is still refused.
	nested := filepath.Join(workspace, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", nested, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init nested: %v %s", err, out)
	}
	ins, err := e.Next(second, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("successor next: %+v %v", ins, err)
	}
	if _, err := e.Submit(second, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := e.Next(second, ""); err != nil {
		t.Fatalf("successor next: %v", err)
	}
	snaps = snapshots(t, state, second)
	if last := snaps[len(snaps)-1]; !strings.Contains(last.Unavailable, "unrequested nested repository") {
		t.Errorf("a nested repository in the product tree was not refused: %+v", last)
	}
}
