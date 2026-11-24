package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func probeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func runGitWritable(dir, harness string) *wf.Refusal {
	return git.WritableCheck().Run(wf.PreflightContext{Workspace: dir, Harness: harness})
}

// assertProbeClean fails if the probe left a ref or a worktrees dir behind.
func assertProbeClean(t *testing.T, dir string, worktreesExisted bool) {
	t.Helper()
	if refs := gitIn(t, dir, "for-each-ref", "refs/wf/"); refs != "" {
		t.Errorf("probe ref left behind: %q", refs)
	}
	_, err := os.Stat(filepath.Join(dir, ".git", "worktrees"))
	if got := err == nil; got != worktreesExisted {
		t.Errorf("worktrees dir exists = %v, want %v", got, worktreesExisted)
	}
}

func TestGitWritablePassesAndCleansUp(t *testing.T) {
	dir := probeRepo(t)
	if r := runGitWritable(dir, "claude"); r != nil {
		t.Fatalf("refused a writable repo: %+v", r)
	}
	assertProbeClean(t, dir, false)
}

func TestGitWritableKeepsExistingWorktreesDir(t *testing.T) {
	dir := probeRepo(t)
	// A real linked worktree: git prunes a hand-made entry as stale.
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, dir, "worktree", "add", "-q", "--detach", linked)
	wt := filepath.Join(dir, ".git", "worktrees")
	before, err := os.ReadDir(wt)
	if err != nil || len(before) != 1 {
		t.Fatalf("fixture: worktrees dir = %v, %v", before, err)
	}
	if r := runGitWritable(dir, "claude"); r != nil {
		t.Fatalf("refused a writable repo: %+v", r)
	}
	assertProbeClean(t, dir, true)
	after, _ := os.ReadDir(wt)
	if len(after) != 1 || after[0].Name() != before[0].Name() {
		t.Errorf("worktrees dir entries = %v, want exactly %v", after, before)
	}
}

func TestGitWritableLinkedWorktreeUsesCommonDir(t *testing.T) {
	dir := probeRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, dir, "worktree", "add", "-q", "--detach", linked)
	if r := runGitWritable(linked, "claude"); r != nil {
		t.Fatalf("refused a writable linked worktree: %+v", r)
	}
	if refs := gitIn(t, dir, "for-each-ref", "refs/wf/"); refs != "" {
		t.Errorf("probe ref left behind: %q", refs)
	}
	entries, err := os.ReadDir(filepath.Join(dir, ".git", "worktrees"))
	if err != nil || len(entries) != 1 {
		t.Errorf("common worktrees dir = %v, %v; want only the linked worktree's entry", entries, err)
	}
}

func TestGitWritableRefusesNotARepo(t *testing.T) {
	r := runGitWritable(t.TempDir(), "claude")
	if r == nil {
		t.Fatal("accepted a directory that is not a git repo")
	}
	if !strings.Contains(r.Cause, "git rev-parse --git-common-dir") {
		t.Errorf("cause %q does not carry the git error", r.Cause)
	}
	if r.Remedy != wf.GitWritableRemedy("claude") {
		t.Errorf("remedy %q is not the shared remedy", r.Remedy)
	}
}

func TestGitWritableRefusesReadOnlyRefs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := probeRepo(t)
	refs := filepath.Join(dir, ".git", "refs")
	if err := os.Chmod(refs, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(refs, 0o755) })
	r := runGitWritable(dir, "codex")
	if r == nil {
		t.Fatal("accepted a repo whose refs dir is read-only")
	}
	if !strings.Contains(r.Cause, "update-ref") {
		t.Errorf("cause %q does not carry the update-ref error", r.Cause)
	}
	if r.Remedy != wf.GitWritableRemedy("codex") {
		t.Errorf("remedy %q is not the codex shared remedy", r.Remedy)
	}
}

// A failure after the ref was created must still delete the ref.
func TestGitWritableCleansRefWhenWorktreesStepFails(t *testing.T) {
	dir := probeRepo(t)
	// A file where the worktrees dir belongs: stat succeeds, creating a file in it fails.
	if err := os.WriteFile(filepath.Join(dir, ".git", "worktrees"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := runGitWritable(dir, "claude")
	if r == nil {
		t.Fatal("accepted a repo whose worktrees path is not a directory")
	}
	if refs := gitIn(t, dir, "for-each-ref", "refs/wf/"); refs != "" {
		t.Errorf("probe ref left behind after a later failure: %q", refs)
	}
}

// A fresh repo with no commits has no HEAD to point a ref at; the probe must
// not mistake that for an unwritable .git.
func TestGitWritablePassesUnbornHead(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	if r := runGitWritable(dir, "claude"); r != nil {
		t.Fatalf("refused a writable repo with an unborn HEAD: %+v", r)
	}
	assertProbeClean(t, dir, false)
}

// Concurrent probes share <common-dir>/worktrees: none may refuse a writable
// repo because another made or removed that dir under it.
func TestGitWritableConcurrentProbes(t *testing.T) {
	dir := probeRepo(t)
	errs := make(chan *wf.Refusal, 16)
	for i := 0; i < cap(errs); i++ {
		go func() { errs <- runGitWritable(dir, "claude") }()
	}
	for i := 0; i < cap(errs); i++ {
		if r := <-errs; r != nil {
			t.Errorf("concurrent probe refused a writable repo: %+v", r)
		}
	}
	if refs := gitIn(t, dir, "for-each-ref", "refs/wf/"); refs != "" {
		t.Errorf("probe ref left behind: %q", refs)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, ".git", "worktrees")); err == nil && len(entries) != 0 {
		t.Errorf("probe files left behind: %v", entries)
	}
}

// An inherited GIT_DIR must not redirect the probe: a workspace that is not a
// repository is refused even when GIT_DIR names a writable repository.
func TestGitWritableIgnoresInheritedGitDir(t *testing.T) {
	other := probeRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	if r := runGitWritable(t.TempDir(), "claude"); r == nil {
		t.Fatal("probed the repository GIT_DIR names instead of the workspace")
	}
	if refs := gitIn(t, other, "for-each-ref", "refs/wf/"); refs != "" {
		t.Errorf("probe wrote into the GIT_DIR repository: %q", refs)
	}
}

// The probe needs nothing outside the repository: a missing TMPDIR does not
// fail a writable repo.
func TestGitWritableNeedsNoTempDir(t *testing.T) {
	dir := probeRepo(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if r := runGitWritable(dir, "claude"); r != nil {
		t.Fatalf("refused a writable repo with TMPDIR missing: %+v", r)
	}
	assertProbeClean(t, dir, false)
}
