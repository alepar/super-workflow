package git_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func rt() wf.AdapterRuntime { return git.Runtime(git.Config{}) }

// sh runs git in dir and returns trimmed stdout.
func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a temp repo with one commit on main and an isolated git config.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "T")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sh(t, repo, "init", "-q", "-b", "main")
	writeFile(t, repo, "a.txt", "a\n")
	sh(t, repo, "add", "a.txt")
	sh(t, repo, "commit", "-q", "-m", "first")
	return repo
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read[T any](t *testing.T, ws string, o wf.Observation[T]) T {
	t.Helper()
	raw, err := rt().Read(ws, o.Desc())
	if err != nil {
		t.Fatalf("%s: %v", o.Desc().Op, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func check(t *testing.T, ws string, e wf.ExternalEffect) wf.EffectStatus {
	t.Helper()
	st, err := rt().Check(ws, e)
	if err != nil {
		t.Fatalf("Check %s: %v", e.Op, err)
	}
	return st
}

func TestEveryDeclaredOpHasARuntime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	pats := []*regexp.Regexp{
		regexp.MustCompile(`wf\.Observe\[.*\]\("([a-z_]+)", "([a-z_]+)"`),
		regexp.MustCompile(`Adapter: "([a-z_]+)", Op: "([a-z_]+)"`),
	}
	declared := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pats {
			for _, m := range p.FindAllStringSubmatch(string(src), -1) {
				declared[m[1]+"."+m[2]] = true
			}
		}
	}
	var want []string
	for op := range declared {
		want = append(want, op)
	}
	got := rt().Ops()
	sort.Strings(want)
	sort.Strings(got)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime ops %v, declared ops %v", got, want)
	}
}

func TestReads(t *testing.T) {
	repo := newRepo(t)
	head := sh(t, repo, "rev-parse", "HEAD")
	if got := read(t, repo, git.Head(".")); got != head {
		t.Errorf("head = %q, want %q", got, head)
	}
	sh(t, repo, "branch", "side")
	writeFile(t, repo, "b.txt", "b\n")
	sh(t, repo, "add", "b.txt")
	sh(t, repo, "commit", "-q", "-m", "second")
	if got := read(t, repo, git.RevParse(repo, "main~1")); got != head {
		t.Errorf("rev_parse = %q, want %q", got, head)
	}
	if got := read(t, repo, git.MergeBase(repo, "main", "side")); got != head {
		t.Errorf("merge_base = %q, want %q", got, head)
	}
	if _, err := rt().Read(repo, git.RevParse(repo, "no-such-ref").Desc()); err == nil || errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("rev_parse of a bad ref: %v", err)
	}

	if st := read(t, repo, git.Status(repo)); !st.Clean || len(st.Paths) != 0 {
		t.Errorf("clean status = %+v", st)
	}
	writeFile(t, repo, "a.txt", "changed\n")
	writeFile(t, repo, "new.txt", "n\n")
	st := read(t, repo, git.Status(repo))
	sort.Strings(st.Paths)
	if st.Clean || !reflect.DeepEqual(st.Paths, []string{"a.txt", "new.txt"}) {
		t.Errorf("dirty status = %+v", st)
	}

	wts := read(t, repo, git.WorktreeList(repo))
	if len(wts) != 1 || wts[0].Path != repo || wts[0].Branch != "main" || wts[0].Head != sh(t, repo, "rev-parse", "HEAD") {
		t.Errorf("worktree list = %+v", wts)
	}
}

func TestWorktreeAddAndCheck(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	e := git.WorktreeAdd(wf.KeyOf("wt"), path, "feat", "main")
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Fatalf("before add: %v", st)
	}
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("after add: %v", st)
	}
	if got := sh(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "feat" {
		t.Errorf("branch = %q", got)
	}
	// Same path on another branch is not done.
	if st := check(t, repo, git.WorktreeAdd(wf.KeyOf("wt"), path, "other", "main")); st != wf.EffectNotDone {
		t.Errorf("other branch: %v", st)
	}
	// An existing worktree is left untouched, even with new work in it.
	writeFile(t, path, "wip.txt", "w\n")
	if _, err := rt().Apply(repo, e); err != nil {
		t.Errorf("re-Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "wip.txt")); err != nil {
		t.Errorf("re-Apply disturbed the worktree: %v", err)
	}
	// A relative path resolves against the workspace.
	if _, err := rt().Apply(repo, git.WorktreeAdd(wf.KeyOf("rel"), "rel-wt", "feat2", "main")); err != nil {
		t.Fatalf("relative Apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "rel-wt", "a.txt")); err != nil {
		t.Errorf("relative worktree: %v", err)
	}
}

func recutAdd(t *testing.T, repo, path, branch string) error {
	t.Helper()
	_, err := rt().Apply(repo, git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, branch, "main"))
	return err
}

func TestWorktreeRecutReusesClean(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	writeFile(t, path, "b.txt", "b\n")
	sh(t, path, "add", "b.txt")
	sh(t, path, "commit", "-q", "-m", "work")
	head := sh(t, path, "rev-parse", "HEAD")
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("second add: %v", err)
	}
	if got := sh(t, path, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want the committed work %s kept", got, head)
	}
}

func TestWorktreeRecutResetsDirty(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	base := sh(t, repo, "rev-parse", "main")
	writeFile(t, path, "a.txt", "changed\n")
	writeFile(t, path, "untracked.txt", "u\n")
	e := git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, "feat", "main")
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("Check on a dirty worktree = %v, want not done", st)
	}
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("second add: %v", err)
	}
	if got := sh(t, path, "status", "--porcelain"); got != "" {
		t.Errorf("status after recut = %q, want clean", got)
	}
	if got := sh(t, path, "rev-parse", "HEAD"); got != base {
		t.Errorf("HEAD = %s, want base %s", got, base)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("Check after recut = %v, want done", st)
	}
}

func TestWorktreeAddExistingBranch(t *testing.T) {
	for _, recut := range []bool{false, true} {
		repo := newRepo(t)
		path := filepath.Join(t.TempDir(), "wt")
		sh(t, repo, "worktree", "add", "-q", "-b", "feat", path, "main")
		writeFile(t, path, "b.txt", "b\n")
		sh(t, path, "add", "b.txt")
		sh(t, path, "commit", "-q", "-m", "work")
		tip := sh(t, repo, "rev-parse", "feat")
		sh(t, repo, "worktree", "remove", path)
		e := git.WorktreeAdd(wf.KeyOf("wt"), path, "feat", "main")
		want := tip
		if recut {
			e = git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, "feat", "main")
			want = sh(t, repo, "rev-parse", "main")
		}
		if _, err := rt().Apply(repo, e); err != nil {
			t.Fatalf("recut=%v: %v", recut, err)
		}
		if got := sh(t, path, "rev-parse", "HEAD"); got != want {
			t.Errorf("recut=%v: HEAD = %s, want %s", recut, got, want)
		}
	}
}

func TestWorktreeRecutStaleDirectory(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	e := git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, "feat", "main")
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("Check with the directory gone = %v, want not done", st)
	}
	if err := recutAdd(t, repo, path, "feat"); err != nil {
		t.Fatalf("add after rm -rf: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "a.txt")); err != nil {
		t.Errorf("worktree not recreated: %v", err)
	}
}

func TestWorktreeRecutStaleCheckoutAndRealDirectory(t *testing.T) {
	repo := newRepo(t)
	// A stale checkout (.git file, unregistered) is removed and re-cut.
	stale := filepath.Join(t.TempDir(), "stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stale, ".git", "gitdir: /nonexistent\n")
	if err := recutAdd(t, repo, stale, "feat"); err != nil {
		t.Fatalf("stale checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "a.txt")); err != nil {
		t.Errorf("stale checkout not re-cut: %v", err)
	}
	// A real directory is never removed.
	real := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, real, "keep.txt", "k\n")
	if err := recutAdd(t, repo, real, "feat2"); err == nil || !strings.Contains(err.Error(), real) {
		t.Errorf("real directory: err = %v, want an error naming %s", err, real)
	}
	if _, err := os.Stat(filepath.Join(real, "keep.txt")); err != nil {
		t.Errorf("real directory disturbed: %v", err)
	}
}

func TestWorktreeAddNeverRecutsWithoutFlag(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	e := git.WorktreeAdd(wf.KeyOf("wt"), path, "feat", "main")
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "a.txt", "changed\n")
	writeFile(t, path, "untracked.txt", "u\n")
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("Check = %v, want done even when dirty", st)
	}
	for _, f := range []string{"untracked.txt"} {
		if _, err := os.Stat(filepath.Join(path, f)); err != nil {
			t.Errorf("%s lost: %v", f, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(b) != "changed\n" {
		t.Errorf("a.txt = %q, want the edit kept", b)
	}
}

func TestWorktreeRemoveRefusesDirty(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	sh(t, repo, "worktree", "add", "-q", "-b", "feat", path, "main")
	e := git.WorktreeRemove(wf.KeyOf("rm"), path)
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Fatalf("before remove: %v", st)
	}
	writeFile(t, path, "wip.txt", "w\n")
	sh(t, path, "add", "wip.txt")
	if _, err := rt().Apply(repo, e); err == nil {
		t.Fatal("removed a dirty worktree")
	}
	if _, err := os.Stat(filepath.Join(path, "wip.txt")); err != nil {
		t.Errorf("dirty work lost: %v", err)
	}
	sh(t, path, "reset", "-q")
	if err := os.Remove(filepath.Join(path, "wip.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("remove clean: %v", err)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("after remove: %v", st)
	}
	if _, err := rt().Apply(repo, e); err != nil {
		t.Errorf("remove of an absent worktree: %v", err)
	}
}

func TestMergeNoFFAndAlreadyDone(t *testing.T) {
	repo := newRepo(t)
	sh(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, repo, "t.txt", "t\n")
	sh(t, repo, "add", "t.txt")
	sh(t, repo, "commit", "-q", "-m", "topic work")
	sh(t, repo, "checkout", "-q", "main")
	topic := sh(t, repo, "rev-parse", "topic")

	e := git.Merge(wf.KeyOf("m"), repo, "topic", "", true)
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Fatalf("before merge: %v", st)
	}
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("after merge: %v", st)
	}
	if parents := strings.Fields(sh(t, repo, "log", "-1", "--format=%P")); len(parents) != 2 || parents[1] != topic {
		t.Errorf("not a --no-ff merge commit: parents %v", parents)
	}
	before := sh(t, repo, "rev-parse", "HEAD")
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if after := sh(t, repo, "rev-parse", "HEAD"); after != before {
		t.Errorf("re-merge moved HEAD")
	}

	// Without no_ff a fast-forward is allowed.
	sh(t, repo, "checkout", "-q", "-b", "ff", "topic")
	writeFile(t, repo, "f.txt", "f\n")
	sh(t, repo, "add", "f.txt")
	sh(t, repo, "commit", "-q", "-m", "ff work")
	sh(t, repo, "checkout", "-q", "topic")
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("ff"), repo, "ff", "", false)); err != nil {
		t.Fatalf("ff merge: %v", err)
	}
	if sh(t, repo, "rev-parse", "HEAD") != sh(t, repo, "rev-parse", "ff") {
		t.Error("merge without no_ff did not fast-forward")
	}
}

// conflictingBranch commits a.txt on topic and on main with different content.
func conflictingBranch(t *testing.T, repo string) {
	t.Helper()
	sh(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, repo, "a.txt", "topic\n")
	sh(t, repo, "commit", "-q", "-am", "topic edit")
	sh(t, repo, "checkout", "-q", "main")
	writeFile(t, repo, "a.txt", "main\n")
	sh(t, repo, "commit", "-q", "-am", "main edit")
}

func mergeHead(repo string) bool {
	cmd := exec.Command("git", "rev-parse", "-q", "--verify", "MERGE_HEAD")
	cmd.Dir = repo
	return cmd.Run() == nil
}

func TestMergeConflictAbortsAndNextMergeWorks(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	sh(t, repo, "checkout", "-q", "-b", "other", "main")
	writeFile(t, repo, "o.txt", "o\n")
	sh(t, repo, "add", "o.txt")
	sh(t, repo, "commit", "-q", "-m", "other work")
	sh(t, repo, "checkout", "-q", "main")
	before := sh(t, repo, "rev-parse", "HEAD")

	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("c"), repo, "topic", "", true)); err == nil {
		t.Fatal("a conflicting merge returned no error")
	}
	if mergeHead(repo) {
		t.Fatal("MERGE_HEAD left behind: the worktree is wedged mid-merge")
	}
	if got := sh(t, repo, "status", "--porcelain"); got != "" {
		t.Errorf("tree not clean after abort: %q", got)
	}
	if after := sh(t, repo, "rev-parse", "HEAD"); after != before {
		t.Errorf("HEAD moved to %s", after)
	}
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("o"), repo, "other", "", true)); err != nil {
		t.Fatalf("a later unrelated merge: %v", err)
	}
}

// ownerMarker is the ownership marker Apply writes before it merges.
func ownerMarker(t *testing.T, repo string) string {
	t.Helper()
	return filepath.Join(repo, sh(t, repo, "rev-parse", "--git-path", "workflow-merge-owner"))
}

// A crash between Apply's merge and its abort leaves a conflicted merge that
// carries the effect's ownership marker: Check aborts it and answers not done.
func TestCheckMidMergeAbortsAndIsNotDone(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	e := git.Merge(wf.KeyOf("c"), repo, "topic", "", true)
	writeFile(t, filepath.Dir(ownerMarker(t, repo)), filepath.Base(ownerMarker(t, repo)), e.KeyID()+"\n")
	cmd := exec.Command("git", "merge", "--no-edit", "topic")
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatal("setup: merge did not conflict")
	}
	if !mergeHead(repo) {
		t.Fatal("setup: no MERGE_HEAD")
	}
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("mid-merge Check = %v, want not done", st)
	}
	if mergeHead(repo) {
		t.Error("Check left the tree mid-merge")
	}
	if _, err := os.Stat(ownerMarker(t, repo)); !os.IsNotExist(err) {
		t.Errorf("ownership marker left after the abort: %v", err)
	}
}

// A merge of the effect's own branch that the human started (no ownership
// marker) holds their partial resolution: Check and Apply leave it alone.
func TestForeignSameBranchMergeIsNeverAborted(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	cmd := exec.Command("git", "merge", "--no-edit", "topic")
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatal("setup: merge did not conflict")
	}
	writeFile(t, repo, "a.txt", "human resolution\n")
	e := git.Merge(wf.KeyOf("m"), repo, "topic", "", true)
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("Check = %v, want not done", st)
	}
	if !mergeHead(repo) {
		t.Fatal("Check aborted the human's merge of the same branch")
	}
	if _, err := rt().Apply(repo, e); err == nil || !strings.Contains(err.Error(), "merge in progress") {
		t.Fatalf("Apply over the human's merge: err = %v, want a refusal", err)
	}
	if body, err := os.ReadFile(filepath.Join(repo, "a.txt")); err != nil || string(body) != "human resolution\n" {
		t.Errorf("partial resolution lost: %q, %v", body, err)
	}
}

// Apply leaves no ownership marker behind, after a clean merge or an aborted one.
func TestMergeLeavesNoOwnershipMarker(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("c"), repo, "topic", "", true)); err == nil {
		t.Fatal("a conflicting merge returned no error")
	}
	if _, err := os.Stat(ownerMarker(t, repo)); !os.IsNotExist(err) {
		t.Errorf("marker left after an aborted merge: %v", err)
	}
	sh(t, repo, "checkout", "-q", "-b", "clean", "main")
	writeFile(t, repo, "c.txt", "c\n")
	sh(t, repo, "add", "c.txt")
	sh(t, repo, "commit", "-q", "-m", "clean work")
	sh(t, repo, "checkout", "-q", "main")
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("ok"), repo, "clean", "", true)); err != nil {
		t.Fatalf("clean merge: %v", err)
	}
	if _, err := os.Stat(ownerMarker(t, repo)); !os.IsNotExist(err) {
		t.Errorf("marker left after a clean merge: %v", err)
	}
}

// MergeInto acts only on its target branch: a checkout left on another branch
// is neither merged into nor reported done, even when that branch already
// contains the merged branch.
func TestMergeIntoRefusesWrongBranch(t *testing.T) {
	repo := newRepo(t)
	sh(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, repo, "t.txt", "t\n")
	sh(t, repo, "add", "t.txt")
	sh(t, repo, "commit", "-q", "-m", "topic work")
	sh(t, repo, "checkout", "-q", "-b", "other") // contains topic, and is checked out
	before := sh(t, repo, "rev-parse", "HEAD")
	e := git.MergeInto(wf.KeyOf("f"), repo, "topic", "main", true)
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("Check on the wrong branch = %v, want not done", st)
	}
	if _, err := rt().Apply(repo, e); err == nil || !strings.Contains(err.Error(), "not main") {
		t.Fatalf("Apply on the wrong branch: err = %v, want a refusal naming main", err)
	}
	if after := sh(t, repo, "rev-parse", "HEAD"); after != before || sh(t, repo, "branch", "--show-current") != "other" {
		t.Errorf("the wrong-branch checkout moved: HEAD %s, branch %s", after, sh(t, repo, "branch", "--show-current"))
	}
	sh(t, repo, "checkout", "-q", "main")
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("Apply on main: %v", err)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("Check after the merge into main = %v, want done", st)
	}
}

// A merge the human left in progress (here of a different branch) holds a
// partial resolution: neither Check nor Apply may abort it.
func TestPreexistingMergeIsNeverAborted(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	sh(t, repo, "checkout", "-q", "-b", "other", "topic~0")
	writeFile(t, repo, "o.txt", "o\n")
	sh(t, repo, "add", "o.txt")
	sh(t, repo, "commit", "-q", "-m", "other work")
	sh(t, repo, "checkout", "-q", "main")
	cmd := exec.Command("git", "merge", "--no-edit", "topic")
	cmd.Dir = repo
	if err := cmd.Run(); err == nil {
		t.Fatal("setup: merge did not conflict")
	}
	writeFile(t, repo, "a.txt", "human resolution\n") // the partial resolution
	e := git.Merge(wf.KeyOf("o"), repo, "other", "", true)

	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Errorf("Check = %v, want not done", st)
	}
	if !mergeHead(repo) {
		t.Fatal("Check aborted a merge that is not the effect's branch")
	}
	if _, err := rt().Apply(repo, e); err == nil || !strings.Contains(err.Error(), "merge in progress") {
		t.Fatalf("Apply over a merge in progress: err = %v, want a refusal", err)
	}
	if !mergeHead(repo) {
		t.Fatal("Apply aborted the pre-existing merge")
	}
	body, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	if err != nil || string(body) != "human resolution\n" {
		t.Errorf("partial resolution lost: %q, %v", body, err)
	}
}

func TestCommitTrailerAndCheck(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "a.txt", "changed\n")
	e := git.Commit(wf.KeyOf("c", "1"), repo, "do the thing")
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Fatalf("before commit: %v", st)
	}
	if _, err := rt().Apply(repo, e); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	msg := sh(t, repo, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "do the thing\n") || !strings.HasSuffix(msg, "\nEffect-Key: c/1") {
		t.Errorf("message = %q", msg)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("after commit: %v", st)
	}
	// A key that merely starts the same is a different key.
	if st := check(t, repo, git.Commit(wf.KeyOf("c"), repo, "x")); st != wf.EffectNotDone {
		t.Errorf("prefix key: %v", st)
	}
	// Nothing to commit is a no-op, not an error.
	head := sh(t, repo, "rev-parse", "HEAD")
	if _, err := rt().Apply(repo, git.Commit(wf.KeyOf("noop"), repo, "empty")); err != nil {
		t.Errorf("clean-tree commit: %v", err)
	}
	if sh(t, repo, "rev-parse", "HEAD") != head {
		t.Error("clean-tree commit created a commit")
	}
	// Named paths, including a new file, and only those.
	writeFile(t, repo, "new.txt", "n\n")
	writeFile(t, repo, "a.txt", "again\n")
	if _, err := rt().Apply(repo, git.Commit(wf.KeyOf("p"), repo, "paths", "new.txt")); err != nil {
		t.Fatalf("paths commit: %v", err)
	}
	if got := sh(t, repo, "show", "--name-only", "--format=", "HEAD"); got != "new.txt" {
		t.Errorf("committed %q", got)
	}
	if st := check(t, repo, git.Commit(wf.NonIdempotent("why"), repo, "x")); st != wf.EffectUnknown {
		t.Errorf("non-idempotent: %v", st)
	}
}

func TestMissingCLIUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	r := rt()
	if _, err := r.Read(t.TempDir(), git.Head(".").Desc()); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Read: %v", err)
	}
	e := git.Merge(wf.KeyOf("m"), ".", "x", "", false)
	if _, err := r.Apply(t.TempDir(), e); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Apply: %v", err)
	}
	if _, err := r.Check(t.TempDir(), e); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("Check: %v", err)
	}
}

func TestBinConfig(t *testing.T) {
	if _, err := git.Runtime(git.Config{Bin: "no-such-git-xyzzy"}).Read(t.TempDir(), git.Head(".").Desc()); !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func TestHungToolTimesOutAsUnavailable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hung")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 & wait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := git.Runtime(git.Config{Bin: bin, Timeout: 200 * time.Millisecond}).Read(t.TempDir(), git.Head(".").Desc())
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, want a prompt timeout", d)
	}
}

// mergeReport decodes the post_done_mutation an Apply result reports.
func mergeReport(t *testing.T, raw json.RawMessage) *wf.PostDoneMutationData {
	t.Helper()
	var r wf.PostDoneMutationReport
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return r.PostDoneMutation
}

// TestMergeCommitAndPostDoneMutation merges a SHA (the reviewed head) while
// the task ref has moved past it: only the SHA lands, and the result reports
// the moved ref; an unmoved ref reports nothing.
func TestMergeCommitAndPostDoneMutation(t *testing.T) {
	repo := newRepo(t)
	head := commitOn(t, repo, "main", "base.txt")
	sh(t, repo, "branch", "task-a", head)
	head = commitOn(t, repo, "task-a", "reviewed.txt")
	sh(t, repo, "branch", "task-b", "main")
	headB := commitOn(t, repo, "task-b", "b.txt")
	late := commitOn(t, repo, "task-a", "after-done.txt") // committed after done
	sh(t, repo, "checkout", "-q", "main")

	e := git.Merge(wf.KeyOf("m", "a"), repo, head, "task-a", true)
	if st := check(t, repo, e); st != wf.EffectNotDone {
		t.Fatalf("before merge: %v", st)
	}
	raw, err := rt().Apply(repo, e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := wf.PostDoneMutationData{TaskRef: "task-a", RefTarget: late, Commit: head}
	if got := mergeReport(t, raw); got == nil || *got != want {
		t.Fatalf("moved ref: report %+v, want %+v", got, want)
	}
	if st := check(t, repo, e); st != wf.EffectDone {
		t.Errorf("after merge: %v", st)
	}
	if parents := strings.Fields(sh(t, repo, "log", "-1", "--format=%P")); len(parents) != 2 || parents[1] != head {
		t.Errorf("merged parents %v, want the SHA %s", parents, head)
	}
	if _, err := os.Stat(filepath.Join(repo, "after-done.txt")); !os.IsNotExist(err) {
		t.Error("the post-done commit was merged")
	}

	raw, err = rt().Apply(repo, git.Merge(wf.KeyOf("m", "b"), repo, headB, "task-b", true))
	if err != nil {
		t.Fatalf("Apply b: %v", err)
	}
	if got := mergeReport(t, raw); got != nil {
		t.Errorf("unmoved ref reported %+v", got)
	}
}

// TestMergeConflictReportsPostDoneMutation: a conflicting merge still
// reports a moved task ref alongside its error.
func TestMergeConflictReportsPostDoneMutation(t *testing.T) {
	repo := newRepo(t)
	conflictingBranch(t, repo)
	head := sh(t, repo, "rev-parse", "topic")
	late := commitOn(t, repo, "topic", "late.txt")
	sh(t, repo, "checkout", "-q", "main")
	raw, err := rt().Apply(repo, git.Merge(wf.KeyOf("c"), repo, head, "topic", true))
	if err == nil {
		t.Fatal("a conflicting merge returned no error")
	}
	want := wf.PostDoneMutationData{TaskRef: "topic", RefTarget: late, Commit: head}
	if got := mergeReport(t, raw); got == nil || *got != want {
		t.Fatalf("report %+v, want %+v", got, want)
	}
}

// TestMergeRefusesNames: a task merge (task ref set) takes only a full commit
// SHA, so it never re-resolves a name; no merge takes a commit git could read
// as an option. A base absorb (empty task ref) still merges a branch name.
func TestMergeRefusesNames(t *testing.T) {
	repo := newRepo(t)
	commitOn(t, repo, "main", "base.txt")
	sh(t, repo, "branch", "task-a", "main")
	head := commitOn(t, repo, "task-a", "a.txt")
	sh(t, repo, "checkout", "-q", "main")
	before := sh(t, repo, "rev-parse", "HEAD")
	for _, c := range []string{"task-a", "HEAD", head[:12], head + "~0", "-" + head[1:]} {
		e := git.Merge(wf.KeyOf("m", c), repo, c, "task-a", true)
		if _, err := rt().Apply(repo, e); err == nil || !strings.Contains(err.Error(), "not a") {
			t.Errorf("Apply %q: err = %v, want a refusal", c, err)
		}
		if st := check(t, repo, e); st != wf.EffectNotDone {
			t.Errorf("Check %q: %v, want not done", c, st)
		}
	}
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("m", "opt"), repo, "--abort", "", true)); err == nil {
		t.Error("an option-shaped commit was merged")
	}
	if got := sh(t, repo, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD moved to %s", got)
	}
	if _, err := rt().Apply(repo, git.Merge(wf.KeyOf("absorb"), repo, "task-a", "", true)); err != nil {
		t.Fatalf("base absorb by name: %v", err)
	}
	if _, err := rt().Apply(repo, git.MergeInto(wf.KeyOf("into"), repo, "-x", "main", true)); err == nil {
		t.Error("MergeInto accepted an option-shaped branch")
	}
}

// TestMergeTaskRefRead: a deleted task ref (a plain exit 1 from rev-parse
// --quiet) reports an empty RefTarget; any other failure to read it fails the
// Apply as unavailable before merging, never as a false post_done_mutation.
func TestMergeTaskRefRead(t *testing.T) {
	repo := newRepo(t)
	commitOn(t, repo, "main", "base.txt")
	sh(t, repo, "branch", "task-a", "main")
	head := commitOn(t, repo, "task-a", "a.txt")
	sh(t, repo, "checkout", "-q", "main")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\ncase \"$*\" in *'--quiet --end-of-options task-a^{commit}'*) echo boom >&2; exit 128;; esac\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	before := sh(t, repo, "rev-parse", "HEAD")
	_, err = git.Runtime(git.Config{Bin: bin}).Apply(repo, git.Merge(wf.KeyOf("m", "a"), repo, head, "task-a", true))
	if !errors.Is(err, wf.ErrUnavailable) {
		t.Fatalf("failed ref read: err = %v, want ErrUnavailable", err)
	}
	if got := sh(t, repo, "rev-parse", "HEAD"); got != before {
		t.Fatalf("merged despite the failed ref read: HEAD %s", got)
	}

	sh(t, repo, "branch", "-D", "task-a")
	raw, err := rt().Apply(repo, git.Merge(wf.KeyOf("m", "a"), repo, head, "task-a", true))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := wf.PostDoneMutationData{TaskRef: "task-a", RefTarget: "", Commit: head}
	if got := mergeReport(t, raw); got == nil || *got != want {
		t.Fatalf("deleted ref: report %+v, want %+v", got, want)
	}
}

func TestWorktreeLinksMoveWithRepository(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(repo, "worktrees", "task")
	if _, err := rt().Apply(repo, git.WorktreeAdd(wf.KeyOf("move"), path, "task", "main")); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	got := sh(t, filepath.Join(moved, "worktrees", "task"), "rev-parse", "--show-toplevel")
	if got != filepath.Join(moved, "worktrees", "task") {
		t.Fatalf("worktree points outside moved repo: %s", got)
	}
	list := sh(t, moved, "worktree", "list", "--porcelain")
	if strings.Contains(list, repo) {
		t.Fatalf("worktree back-reference stayed absolute: %s", list)
	}
}

func TestRepairLegacyWorktreeLinksAfterMove(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(repo, "worktrees", "legacy")
	sh(t, repo, "worktree", "add", "-b", "legacy", path, "main")
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	repair, ok := rt().(interface{ RelocateWorkspace(string, string) error })
	if !ok {
		t.Fatal("git runtime cannot repair relocated links")
	}
	if err := repair.RelocateWorkspace(repo, moved); err != nil {
		t.Fatal(err)
	}
	got := sh(t, filepath.Join(moved, "worktrees", "legacy"), "rev-parse", "--show-toplevel")
	if got != filepath.Join(moved, "worktrees", "legacy") {
		t.Fatal(got)
	}
	again := filepath.Join(t.TempDir(), "again")
	if err := os.Rename(moved, again); err != nil {
		t.Fatal(err)
	}
	sh(t, filepath.Join(again, "worktrees", "legacy"), "status", "--porcelain")
}

func TestRepairMovedLinkedWorkspace(t *testing.T) {
	repo := newRepo(t)
	workspace := filepath.Join(repo, "worktrees", "legacy")
	sh(t, repo, "worktree", "add", "-b", "legacy", workspace, "main")
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(moved, "worktrees", "legacy")
	repair := rt().(interface{ RelocateWorkspace(string, string) error })
	if err := repair.RelocateWorkspace(workspace, target); err != nil {
		t.Fatal(err)
	}
	sh(t, target, "status", "--porcelain")
}

func TestRelocatePlainWorkspaceWithGitRegistered(t *testing.T) {
	repair := rt().(interface{ RelocateWorkspace(string, string) error })
	if err := repair.RelocateWorkspace("/old/plain", t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// An engine started from a git hook inherits GIT_DIR and GIT_WORK_TREE for
// the hook's repository. The adapter's commit still lands in the workspace's
// repository and writes nothing into the one those variables name (sw-j721).
func TestAdapterIgnoresInheritedRepositoryEnvironment(t *testing.T) {
	repo := newRepo(t)
	other := newRepo(t)
	otherHead := sh(t, other, "rev-parse", "HEAD")
	otherRefs := sh(t, other, "for-each-ref")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	writeFile(t, repo, "a.txt", "changed\n")
	if _, err := rt().Apply(repo, git.Commit(wf.KeyOf("c"), repo, "in the workspace")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		os.Unsetenv(k)
	}
	if msg := sh(t, repo, "log", "-1", "--format=%s"); msg != "in the workspace" {
		t.Errorf("workspace head = %q", msg)
	}
	if head, refs := sh(t, other, "rev-parse", "HEAD"), sh(t, other, "for-each-ref"); head != otherHead || refs != otherRefs {
		t.Errorf("the inherited GIT_DIR's repository changed: head %s refs %q", head, refs)
	}
}
