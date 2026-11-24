package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	bdadapter "github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	gitadapter "github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// v1Run is the run recorded in testdata/snapshot_v1 by the engine before the
// snapshot store: a checkpoint directory per capture (version 1).
const v1Run = wf.RunID("snapshot-test-4e49912630b5cb4a")

// A recorded version-1 checkpoint still forks: its world and run data come
// back from the checkpoint directory.
func TestSnapshotVersionOneRecordedRunStillForks(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if out, err := exec.Command("cp", "-a", filepath.Join("testdata", "snapshot_v1"), state).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	d := snapshotDef()
	ws := t.TempDir()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	recs, err := wf.ReadJournalWithRoots(state, v1Run, wf.PathRoots{Workspace: ws, State: state})
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			break
		}
	}
	fork, err := e.ForkSnapshot(v1Run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(fork.Workspace, "data")); err != nil || string(got) != "before" {
		t.Fatalf("world %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(fork.StateDir, "runs", string(v1Run), "out", "historical")); err != nil || string(got) != "historical" {
		t.Fatalf("run data %q %v", got, err)
	}
}

type (
	stWork struct{}
	stDone struct{}
	stStop struct{}
)

// twoAttemptDef is snapshotDef with three attempts: a failed first attempt is
// issued again in a later cycle, before a checkpoint of its own.
func twoAttemptDef() *wf.Def {
	d := wf.New("store-two-attempts")
	a := wf.NewAgent[stWork, string](d)
	done := wf.NewTerminal[stDone](d, wf.Success)
	stop := wf.NewTerminal[stStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("work"), wf.Retry[stWork](wf.Const(3), a.To(stop, "gave up")))
	edge := a.To(done, "done")
	a.Route(func(string, wf.Ctx) wf.Edge[stWork] { return edge }, edge)
	return d
}

// storeEngine is a file-journal engine over twoAttemptDef with snapshots on.
func storeEngine(t *testing.T) (*wf.Engine, *wf.Def, string, string) {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	_ = reg.Register(&snapshotRuntime{})
	d := twoAttemptDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	return e, d, ws, state
}

func storeCheckpoints(t *testing.T, state string, run wf.RunID) []wf.WorldSnapshotData {
	t.Helper()
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind != wf.JournalWorldSnapshot {
			continue
		}
		var d wf.WorldSnapshotData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Unavailable == "" {
			out = append(out, d)
		}
	}
	return out
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Through the whole engine, adapters and staging included: a checkpoint of
// an unchanged world in a later cycle names the same tree and adds no bytes
// to the store.
func TestStoreEngineUnchangedWorldAddsNothing(t *testing.T) {
	e, d, _, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	// The first hand-out creates run data (its output's directory); from
	// the second attempt on, nothing changes.
	if _, err := e.Fail(run, ins[0].Token, "again"); err != nil {
		t.Fatal(err)
	}
	if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	store := filepath.Join(state, "runs", string(run), "snapshots", "store.git")
	before := dirBytes(t, store)
	start := time.Now()
	if _, err := e.Fail(run, ins[0].Token, "again"); err != nil { // attempt 2 is issued in a new cycle
		t.Fatal(err)
	}
	cps := storeCheckpoints(t, state, run)
	if len(cps) < 2 {
		t.Fatalf("checkpoints %+v", cps)
	}
	last, prev := cps[len(cps)-1], cps[len(cps)-2]
	if last.Tree != prev.Tree {
		t.Errorf("an unchanged world got a new tree: %s, was %s", last.Tree, prev.Tree)
	}
	if grew := dirBytes(t, store) - before; grew != 0 {
		t.Errorf("an unchanged world added %d bytes", grew)
	}
	t.Logf("a later cycle's unchanged checkpoint: %v (the whole fail cycle)", time.Since(start))
}

// A fork of an early checkpoint holds only the checkpoints its prefix names:
// none the source captured after it.
func TestStoreForkOfAnEarlyCheckpointHoldsNoLaterOnes(t *testing.T) {
	e, d, ws, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(state, run)
	var early int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			early = r.Seq
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Fail(run, ins[0].Token, "again"); err != nil {
		t.Fatal(err)
	}
	cps := storeCheckpoints(t, state, run)
	late := cps[len(cps)-1].Tree
	fork, err := e.ForkSnapshot(run, early, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	refs, _ := os.ReadDir(filepath.Join(fork.StateDir, "runs", string(run), "snapshots", "store.git", "refs", "checkpoints"))
	for _, r := range refs {
		if r.Name() == late {
			t.Fatal("the fork's store holds a later checkpoint")
		}
	}
	if len(refs) == 0 {
		t.Fatal("the fork's store holds no checkpoint")
	}
	if got, _ := os.ReadFile(filepath.Join(fork.Workspace, "data")); string(got) != "before" {
		t.Fatalf("fork world %q", got)
	}
}

// Run data in read-only directories forks and the temporary checkpoint is removed.
func TestStoreRunDataReadOnlyDirectories(t *testing.T) {
	e, d, _, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ro := filepath.Join(state, "runs", string(run), "records", "ro")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ro, "f"), []byte("kept"), 0o444); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(ro, 0o555)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(state, run)
	var seq int64
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
		}
	}
	out := filepath.Join(t.TempDir(), "fork")
	fork, err := e.ForkSnapshot(run, seq, out)
	if err != nil {
		t.Fatal(err)
	}
	forked := filepath.Join(fork.StateDir, "runs", string(run), "records", "ro")
	t.Cleanup(func() {
		_ = os.Chmod(forked, 0o755)
		_ = os.Chmod(filepath.Join(out, ".checkpoint", "run-data", "records", "ro"), 0o755)
	})
	if got, err := os.ReadFile(filepath.Join(forked, "f")); err != nil || string(got) != "kept" {
		t.Fatalf("read-only run data: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(out, ".checkpoint")); !os.IsNotExist(err) {
		t.Fatalf("the fork kept its temporary checkpoint: %v", err)
	}

}

func TestStoreRunDataPreservesScratchSymlinks(t *testing.T) {
	e, d, _, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(state, "runs", string(run), "records", "scratch", "venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(external, "outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	targets := map[string]string{
		"python3":  "python3.12",
		"absolute": external,
		"dangling": "../../../../missing",
	}
	if err := os.WriteFile(filepath.Join(bin, "python3.12"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range targets {
		if err := os.Symlink(target, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	fork := forkRunDataCheckpoint(t, e, state, run, nil)
	restored := filepath.Join(fork.StateDir, "runs", string(run), "records", "scratch", "venv", "bin")
	for name, want := range targets {
		if got, err := os.Readlink(filepath.Join(restored, name)); got != want || err != nil {
			t.Errorf("%s target = %q, %v; want %q", name, got, err, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(restored, "python3.12")); string(got) != "binary" || err != nil {
		t.Fatalf("adjacent regular file = %q, %v", got, err)
	}
	if info, err := os.Lstat(filepath.Join(external, "outside")); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		t.Fatalf("outside target's FIFO changed: %v, %v", info, err)
	}
}

func TestStoreRunDataOmitsUnsupportedScratchFile(t *testing.T) {
	e, d, _, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(state, "runs", string(run), "records", "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(scratch, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "kept"), []byte("captured"), 0o600); err != nil {
		t.Fatal(err)
	}
	fork := forkRunDataCheckpoint(t, e, state, run, []string{"run-data/records/scratch/pipe (unsupported file p---------)"})
	restored := filepath.Join(fork.StateDir, "runs", string(run), "records", "scratch")
	if _, err := os.Lstat(filepath.Join(restored, "pipe")); !os.IsNotExist(err) {
		t.Errorf("pipe restored: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(restored, "kept")); string(got) != "captured" || err != nil {
		t.Fatalf("adjacent file = %q, %v", got, err)
	}
}

func forkRunDataCheckpoint(t *testing.T, e *wf.Engine, state string, run wf.RunID, omitted []string) wf.SnapshotFork {
	t.Helper()
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var seq int64
	var last wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			if err := json.Unmarshal(r.Data, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	if seq == 0 || last.Unavailable != "" {
		t.Fatalf("capture unavailable: %+v", last)
	}
	if !slices.Equal(last.Omitted, omitted) {
		t.Fatalf("checkpoint omissions %q, want %q", last.Omitted, omitted)
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err = wf.ReadJournal(fork.StateDir, run)
	if err != nil {
		t.Fatal(err)
	}
	var rewound wf.RewoundData
	for _, r := range recs {
		if r.Kind == wf.JournalRewound {
			if err := json.Unmarshal(r.Data, &rewound); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(rewound.Omitted, omitted) {
		t.Fatalf("rewind omissions %q, want %q", rewound.Omitted, omitted)
	}
	return fork
}

// Through the engine with the real git and bd adapters: a checkpoint of an
// unchanged world in a later cycle stores no tracker bytes again (its bd
// tree is the same) and adds only what the git adapter writes anew each
// time (a bundle under a fresh ref).
func TestStoreEngineUnchangedWorldRealGitAndBd(t *testing.T) {
	for _, bin := range []string{"git", "bd"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s unavailable", bin)
		}
	}
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	command := func(dir, bin string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
		}
		return out
	}
	command(ws, "git", "init", "-q", "-b", "main")
	command(ws, "git", "config", "user.name", "Snapshot Test")
	command(ws, "git", "config", "user.email", "snapshot@example.invalid")
	command(ws, "bd", "init", "--quiet", "--prefix", "snap")
	command(ws, "git", "add", "-f", ".beads/config.yaml", ".beads/metadata.json")
	command(ws, "git", "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-qm", "tracker configuration")
	for i := 0; i < 20; i++ {
		command(ws, "bd", "create", fmt.Sprintf("issue %d", i), "--json")
	}
	state := t.TempDir()
	reg := wf.NewAdapters()
	for _, adapter := range []wf.AdapterRuntime{gitadapter.Runtime(gitadapter.Config{}), bdadapter.Runtime(bdadapter.Config{})} {
		if err := reg.Register(adapter); err != nil {
			t.Fatal(err)
		}
	}
	d := twoAttemptDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	if _, err := e.Fail(run, ins[0].Token, "again"); err != nil {
		t.Fatal(err)
	}
	if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v %v", ins, err)
	}
	store := filepath.Join(state, "runs", string(run), "snapshots", "store.git")
	before := dirBytes(t, store)
	start := time.Now()
	if _, err := e.Fail(run, ins[0].Token, "again"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	cps := storeCheckpoints(t, state, run)
	if len(cps) < 2 {
		t.Fatalf("checkpoints %+v", cps)
	}
	last, prev := cps[len(cps)-1], cps[len(cps)-2]
	bdTree := func(tree string) string {
		out, err := exec.Command("git", "--git-dir", store, "rev-parse", tree+":bd").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if bdTree(last.Tree) != bdTree(prev.Tree) {
		t.Error("an unchanged tracker got a new tree")
	}
	grew := dirBytes(t, store) - before
	world := dirBytes(t, filepath.Join(ws, ".beads"))
	t.Logf("real git+bd, unchanged world: the whole fail cycle %v; store grew %d bytes (tracker %d bytes)", took, grew, world)
	if grew > 64<<10 {
		t.Errorf("an unchanged world added %d bytes", grew)
	}
}

// A nested worktree (.git file) and a nested repository (.git directory)
// in run data are captured as plain files without their .git entries,
// which the checkpoint lists as omitted, and a fork restores the rest
// (an earlier revision, the regression case: a roast scout's scratch worktree made git fast-import
// die on a .git path and every later capture unavailable).
func TestStoreRunDataNestedRepositoriesOmitTheirGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	e, d, _, state := storeEngine(t)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(state, "runs", string(run), "records", "scratch")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	repo := filepath.Join(scratch, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("in the repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", ".")
	git(repo, "commit", "-qm", "x")
	git(repo, "worktree", "add", "-q", "-b", "wt", filepath.Join(scratch, "wt"))
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(state, run)
	var seq int64
	var last wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
			if err := json.Unmarshal(r.Data, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	if last.Unavailable != "" {
		t.Fatalf("capture unavailable: %s", last.Unavailable)
	}
	want := []string{"run-data/records/scratch/repo/.git", "run-data/records/scratch/wt/.git"}
	if strings.Join(last.Omitted, ",") != strings.Join(want, ",") {
		t.Fatalf("omitted %q, want %q", last.Omitted, want)
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	forked := filepath.Join(fork.StateDir, "runs", string(run), "records", "scratch")
	for _, f := range []string{"repo/a.txt", "wt/a.txt"} {
		if got, err := os.ReadFile(filepath.Join(forked, f)); err != nil || string(got) != "in the repo\n" {
			t.Fatalf("%s: %q %v", f, got, err)
		}
	}
	for _, g := range []string{"repo/.git", "wt/.git"} {
		if _, err := os.Lstat(filepath.Join(forked, g)); !os.IsNotExist(err) {
			t.Fatalf("%s restored: %v", g, err)
		}
	}
}
