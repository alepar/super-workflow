package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestSnapshotPortableDirtyWorld(t *testing.T) {
	repo := newRepo(t)
	sh(t, repo, "branch", "base")
	wt := filepath.Join(repo, "trees", "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	writeFile(t, repo, ".gitignore", "trees/\nignored\n")
	writeFile(t, repo, "a.txt", "staged\n")
	sh(t, repo, "add", "a.txt")
	writeFile(t, repo, "a.txt", "unstaged\n")
	writeFile(t, repo, "new", "untracked\n")
	writeFile(t, repo, "ignored", "omit\n")
	writeFile(t, wt, "a.txt", "task staged\n")
	sh(t, wt, "add", "a.txt")
	writeFile(t, wt, "a.txt", "task dirty\n")
	before := sh(t, repo, "status", "--porcelain")
	taskBefore := sh(t, wt, "status", "--porcelain")
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	head := sh(t, repo, "rev-parse", "HEAD")
	s, ok := rt().(wf.WorldSnapshotter)
	if !ok {
		t.Fatal("snapshot capability missing")
	}
	checkpoint := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint, Worktrees: []string{"trees/task"}})
	if err != nil {
		t.Fatal(err)
	}
	afterIndex, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if string(index) != string(afterIndex) {
		t.Fatal("source index changed")
	}
	if sh(t, repo, "rev-parse", "HEAD") != head || sh(t, repo, "status", "--porcelain") != before {
		t.Fatal("source changed")
	}
	if strings.Contains(string(raw), repo) {
		t.Fatal("absolute source path in metadata")
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: checkpoint}, raw); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, restored, "status", "--porcelain"); got != before {
		t.Fatalf("status %q want %q", got, before)
	}
	if got := sh(t, filepath.Join(restored, "trees/task"), "status", "--porcelain"); got != taskBefore {
		t.Fatalf("task status %q want %q", got, taskBefore)
	}
	if sh(t, restored, "show", ":a.txt") != "staged" {
		t.Fatal("staged contents lost")
	}
	if body, _ := os.ReadFile(filepath.Join(restored, "a.txt")); string(body) != "unstaged\n" {
		t.Fatal("unstaged contents lost")
	}
	if _, err := os.Stat(filepath.Join(restored, "ignored")); !os.IsNotExist(err) {
		t.Fatal("ignored content captured")
	}
	sh(t, restored, "rev-parse", "base")
}

func TestSnapshotPreflightRejectsUnsafeLayouts(t *testing.T) {
	repo := newRepo(t)
	s, ok := rt().(wf.WorldSnapshotter)
	if !ok {
		t.Fatal("snapshot capability missing")
	}
	// A requested path that git does not register and that does not exist
	// is a removed checkout, left out (TestSnapshotDropsARemovedRequestedWorktree).
	for _, path := range []string{"../outside", "/outside", "."} {
		if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Worktrees: []string{path}}); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	writeFile(t, repo, ".gitmodules", "unsupported")
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir()}); err == nil {
		t.Fatal("accepted submodules")
	}
}

func TestSnapshotNestedCheckpointExcluded(t *testing.T) {
	repo := newRepo(t)
	checkpoint := filepath.Join(repo, "state", "snapshots", "one")
	if err := os.MkdirAll(checkpoint, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, checkpoint, "large-artifact", "excluded")
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".beads"), "database", "excluded")
	s := rt().(wf.WorldSnapshotter)
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint, Exclude: []string{"state"}})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = s.Restore(wf.RestoreRequest{Workspace: target, Directory: checkpoint}, raw); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"state", ".beads"} {
		if _, err = os.Stat(filepath.Join(target, p)); !os.IsNotExist(err) {
			t.Fatalf("captured %s", p)
		}
	}
}

func TestSnapshotDetachedDeletesAndFileModes(t *testing.T) {
	repo := newRepo(t)
	sh(t, repo, "checkout", "--detach")
	writeFile(t, repo, "staged-delete", "x")
	writeFile(t, repo, "unstaged-delete", "x")
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "more")
	sh(t, repo, "rm", "staged-delete")
	if err := os.Remove(filepath.Join(repo, "unstaged-delete")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "executable", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(repo, "executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("link", filepath.Join(repo, "chain")); err != nil {
		t.Fatal(err)
	}
	before := sh(t, repo, "status", "--porcelain")
	s := rt().(wf.WorldSnapshotter)
	cp := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err = s.Restore(wf.RestoreRequest{Workspace: restored, Directory: cp}, raw); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, restored, "status", "--porcelain"); got != before {
		t.Fatalf("status %q want %q", got, before)
	}
	if sh(t, restored, "branch", "--show-current") != "" {
		t.Fatal("detached HEAD became branch")
	}
	if dest, err := os.Readlink(filepath.Join(restored, "link")); err != nil || dest != "a.txt" {
		t.Fatal("symlink lost")
	}
	if fi, err := os.Stat(filepath.Join(restored, "executable")); err != nil || fi.Mode()&0111 == 0 {
		t.Fatal("executable bit lost")
	}
}

func TestSnapshotRejectsIntentToAddBeforeWriting(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "intent", "pending\n")
	sh(t, repo, "add", "-N", "intent")
	cp := filepath.Join(t.TempDir(), "absent")
	s := rt().(wf.WorldSnapshotter)
	if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp}); err == nil {
		t.Fatal("accepted intent-to-add index")
	}
	if _, err := os.Stat(cp); !os.IsNotExist(err) {
		t.Fatal("preflight failure mutated checkpoint")
	}
}

func TestSnapshotExcludesTrackedAdapterStateFromWorktreeOnly(t *testing.T) {
	repo := newRepo(t)
	for _, dir := range []string{".beads", "state"} {
		if err := os.Mkdir(filepath.Join(repo, dir), 0700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, dir), "config", "original\n")
	}
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "tracked adapter state")
	writeFile(t, filepath.Join(repo, ".beads"), "config", "staged\n")
	sh(t, repo, "add", ".beads/config")
	before, err := os.ReadFile(filepath.Join(repo, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	cp := t.TempDir()
	s := rt().(wf.WorldSnapshotter)
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp, Exclude: []string{"state"}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(repo, ".git/index"))
	if string(before) != string(after) {
		t.Fatal("source index mutated")
	}
	target := filepath.Join(t.TempDir(), "restored")
	if err = s.Restore(wf.RestoreRequest{Workspace: target, Directory: cp}, raw); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".beads", "state"} {
		if _, err = os.Stat(filepath.Join(target, dir)); !os.IsNotExist(err) {
			t.Fatalf("restored excluded %s", dir)
		}
	}
	if got := sh(t, target, "show", ":.beads/config"); got != "staged" {
		t.Fatalf("saved index content %q", got)
	}
	if got := sh(t, target, "show", ":state/config"); got != "original" {
		t.Fatalf("saved state index content %q", got)
	}
}

func TestSnapshotBundleOmitsEarlierSnapshotRefs(t *testing.T) {
	repo := newRepo(t)
	s := rt().(wf.WorldSnapshotter)
	first, second := t.TempDir(), t.TempDir()
	if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: first}); err != nil {
		t.Fatal(err)
	}
	refs := strings.Fields(sh(t, repo, "for-each-ref", "--format=%(refname)", "refs/workflow/snapshots/"))
	if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: second}); err != nil {
		t.Fatal(err)
	}
	heads := sh(t, repo, "bundle", "list-heads", filepath.Join(second, "repository.bundle"))
	for _, ref := range refs {
		if strings.Contains(heads, ref) {
			t.Fatalf("prior snapshot bundled: %s", heads)
		}
	}
	if !strings.Contains(heads, "refs/heads/main") || strings.Count(heads, "refs/workflow/snapshots/") != 1 {
		t.Fatalf("unexpected bundle heads: %s", heads)
	}
}

func TestSnapshotRejectsSymlinksOutsideCapturedWorld(t *testing.T) {
	for _, kind := range []string{"absolute-source", "relative-escape", "chain-escape"} {
		t.Run(kind, func(t *testing.T) {
			repo := newRepo(t)
			target := filepath.Join(repo, "a.txt")
			switch kind {
			case "relative-escape":
				target = "../outside"
			case "chain-escape":
				target = "chain"
				if err := os.Symlink("../outside", filepath.Join(repo, "chain")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			cp := filepath.Join(t.TempDir(), "checkpoint")
			if _, err := rt().(wf.WorldSnapshotter).Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp}); err == nil {
				t.Fatal("accepted escaping symlink")
			}
			if _, err := os.Stat(cp); !os.IsNotExist(err) {
				t.Fatal("preflight mutated checkpoint")
			}
		})
	}
}

// What git ignores is never captured, so an escaping link or a nested
// repository inside an ignored build tree does not refuse the capture; the
// same link outside it still does.
func TestSnapshotPreflightSkipsIgnoredTrees(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", "target/\n")
	sh(t, repo, "add", ".gitignore")
	sh(t, repo, "commit", "-q", "-m", "ignore")
	if err := os.MkdirAll(filepath.Join(repo, "target", "dep", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(repo, "target", "root")); err != nil {
		t.Fatal(err)
	}
	s := rt().(wf.WorldSnapshotter)
	if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: filepath.Join(t.TempDir(), "cp")}); err != nil {
		t.Fatalf("an ignored tree refused the capture: %v", err)
	}
	if err := os.Symlink("/", filepath.Join(repo, "root")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: filepath.Join(t.TempDir(), "cp")}); err == nil {
		t.Fatal("accepted an escaping symlink outside the ignored tree")
	}
}

func TestSnapshotIgnoresInheritedGitEnvironment(t *testing.T) {
	for _, extra := range []bool{false, true} {
		t.Run(map[bool]string{false: "source-redirect", true: "all-overrides"}[extra], func(t *testing.T) {
			repo := newRepo(t)
			sh(t, repo, "checkout", "--detach")
			writeFile(t, repo, "a.txt", "checkpoint\n")
			index, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
			head, _ := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
			t.Setenv("GIT_DIR", filepath.Join(repo, ".git"))
			t.Setenv("GIT_WORK_TREE", repo)
			if extra {
				for _, name := range []string{"GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS"} {
					t.Setenv(name, filepath.Join(repo, "invalid"))
				}
				t.Setenv("GIT_CONFIG_COUNT", "1")
				t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
				t.Setenv("GIT_CONFIG_VALUE_0", repo)
			}
			s := rt().(wf.WorldSnapshotter)
			checkpoint := t.TempDir()
			raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, repo, "a.txt", "source after checkpoint\n")
			out := filepath.Join(t.TempDir(), "fork")
			if err := s.Restore(wf.RestoreRequest{Workspace: out, Directory: checkpoint}, raw); err != nil {
				t.Fatal(err)
			}
			for _, check := range []struct{ path, want string }{
				{filepath.Join(repo, "a.txt"), "source after checkpoint\n"},
				{filepath.Join(repo, ".git", "index"), string(index)},
				{filepath.Join(repo, ".git", "HEAD"), string(head)},
				{filepath.Join(out, "a.txt"), "checkpoint\n"},
			} {
				got, err := os.ReadFile(check.path)
				if err != nil || string(got) != check.want {
					t.Fatalf("%s changed or missing: %q %v", check.path, got, err)
				}
			}
		})
	}
}

// A live capture validates itself by stamping the world before and after
// (review P1 #1): the stamp moves with a commit in any captured tree, a
// staged change, a branch or tag ref, or a working file's content, and not
// with the capture's own snapshot refs.
func TestSnapshotStampSeesCommitsIndexAndRefs(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(repo, "trees", "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	writeFile(t, repo, ".gitignore", "trees/\n")
	req := func() wf.SnapshotRequest {
		return wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Worktrees: []string{"trees/task"}}
	}
	s, ok := rt().(wf.WorldStamper)
	if !ok {
		t.Fatal("stamp capability missing")
	}
	stamp := func() string {
		t.Helper()
		v, err := s.SnapshotStamp(req())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	index, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
	first := stamp()
	if _, err := rt().(wf.WorldSnapshotter).Snapshot(req()); err != nil {
		t.Fatal(err)
	}
	if again := stamp(); again != first {
		t.Fatal("the stamp moved with the capture's own refs")
	}
	writeFile(t, repo, "a.txt", "unstaged edit\n")
	if stamp() == first {
		t.Fatal("an unstaged edit did not move the stamp")
	}
	writeFile(t, wt, "scratch", "untracked\n")
	edited := stamp()
	if os.Remove(filepath.Join(wt, "scratch")) != nil || stamp() == edited {
		t.Fatal("an untracked file in a linked worktree did not move the stamp")
	}
	if after, _ := os.ReadFile(filepath.Join(repo, ".git", "index")); string(after) != string(index) {
		t.Fatal("stamping wrote the index")
	}
	writeFile(t, wt, "task.txt", "work\n")
	sh(t, wt, "add", "task.txt")
	staged := stamp()
	if staged == first {
		t.Fatal("a staged change in a captured worktree did not move the stamp")
	}
	sh(t, wt, "commit", "-m", "child commit")
	committed := stamp()
	if committed == staged {
		t.Fatal("a commit in a captured worktree did not move the stamp")
	}
	sh(t, repo, "branch", "other")
	if stamp() == committed {
		t.Fatal("a new branch did not move the stamp")
	}
}

func TestSnapshotStampSeesOnlyAllowlistedLocalConfig(t *testing.T) {
	repo := newRepo(t)
	req := wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir()}
	stamp := func() string {
		t.Helper()
		value, err := rt().(wf.WorldStamper).SnapshotStamp(req)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := stamp()
	sh(t, repo, "config", "--local", "credential.helper", "fixture")
	if stamp() != initial {
		t.Fatal("excluded credential setting changed the stamp")
	}
	sh(t, repo, "config", "--local", "user.name", "Changed Name")
	withName := stamp()
	if withName == initial {
		t.Fatal("local user.name did not change the stamp")
	}
	sh(t, repo, "config", "--local", "user.email", "changed@example.invalid")
	if stamp() == withName {
		t.Fatal("local user.email did not change the stamp")
	}
}

// The reviewer's paired-files case (review round 2, P1 #1): two captured
// checkouts each hold a.txt "old". Right after the primary tree is captured
// and before the linked one is read, a child writes primary=new then
// linked=new. The capture then holds primary=old with linked=new, a pair
// that never existed; the stamps taken around it must differ, so the engine
// records the offer unavailable instead of publishing it.
func TestSnapshotStampSeesPairedWorkingFilesChangeMidCapture(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", "trees/\n")
	writeFile(t, repo, "a.txt", "old\n")
	sh(t, repo, "add", ".gitignore", "a.txt")
	sh(t, repo, "commit", "-m", "old")
	wt := filepath.Join(repo, "trees", "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	req := func() wf.SnapshotRequest {
		return wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Worktrees: []string{"trees/task"}}
	}
	s := rt().(wf.WorldStamper)
	before, err := s.SnapshotStamp(req())
	if err != nil {
		t.Fatal(err)
	}
	defer git.SetSnapshotTreeHook(func(tree int) {
		if tree == 0 {
			writeFile(t, repo, "a.txt", "new\n")
			writeFile(t, wt, "a.txt", "new\n")
		}
	})()
	if _, err := rt().(wf.WorldSnapshotter).Snapshot(req()); err != nil {
		t.Fatal(err)
	}
	after, err := s.SnapshotStamp(req())
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("the stamps around a capture that holds a never-existing pair of files agree")
	}
}

// A linked worktree's .beads is ordinary tracked content: the bd adapter
// restores only the primary's tracker. Its tracked files, their modes and
// their working changes come back; untracked tracker files stay out.
func TestSnapshotKeepsTrackedBeadsInLinkedWorktrees(t *testing.T) {
	repo := newRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".beads", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".beads"), "config.yaml", "# defaults\n")
	writeFile(t, filepath.Join(repo, ".beads"), "README.md", "tracker\n")
	if err := os.WriteFile(filepath.Join(repo, ".beads", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, ".gitignore", "trees/\n")
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "tracked tracker files")
	wt := filepath.Join(repo, "trees", "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	// A working change to a tracked tracker file, and an untracked one.
	writeFile(t, filepath.Join(wt, ".beads"), "README.md", "changed in task\n")
	taskBefore := sh(t, wt, "status", "--porcelain")
	writeFile(t, filepath.Join(wt, ".beads"), "redirect", "../../.beads\n")
	s := rt().(wf.WorldSnapshotter)
	cp := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp, Worktrees: []string{"trees/task"}})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: cp}, raw); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(restored, "trees", "task")
	if got := sh(t, task, "status", "--porcelain"); got != taskBefore {
		t.Fatalf("linked worktree status %q, want %q", got, taskBefore)
	}
	if body, _ := os.ReadFile(filepath.Join(task, ".beads", "README.md")); string(body) != "changed in task\n" {
		t.Fatalf("working change to a tracked tracker file lost: %q", body)
	}
	if _, err := os.Lstat(filepath.Join(task, ".beads", "redirect")); !os.IsNotExist(err) {
		t.Fatal("untracked tracker file captured in a linked worktree")
	}
	info, err := os.Stat(filepath.Join(task, ".beads", "hooks", "pre-commit"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("hook's exec bit lost in the linked worktree: %v %v", info, err)
	}
}

// A tracked .beads file a linked checkout replaced with a directory is a
// deletion: the directory's untracked contents are tracker state, never
// captured.
func TestSnapshotLinkedBeadsFileReplacedByDirectory(t *testing.T) {
	repo := newRepo(t)
	if err := os.Mkdir(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, ".beads"), "README.md", "tracker\n")
	writeFile(t, filepath.Join(repo, ".beads"), "config.yaml", "# defaults\n")
	writeFile(t, repo, ".gitignore", "trees/\n")
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "tracked tracker files")
	wt := filepath.Join(repo, "trees", "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	readme := filepath.Join(wt, ".beads", "README.md")
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(readme, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, readme, "untracked", "tracker state\n")
	s := rt().(wf.WorldSnapshotter)
	cp := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: cp, Worktrees: []string{"trees/task"}})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: cp}, raw); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(restored, "trees", "task")
	if _, err := os.Lstat(filepath.Join(task, ".beads", "README.md")); !os.IsNotExist(err) {
		t.Fatalf("the replacing directory or its untracked contents were captured: %v", err)
	}
	if got := sh(t, task, "status", "--porcelain"); got != "D .beads/README.md" {
		t.Fatalf("linked status %q, want the deletion", got)
	}
	if body, _ := os.ReadFile(filepath.Join(task, ".beads", "config.yaml")); string(body) != "# defaults\n" {
		t.Fatal("the other tracked tracker file was lost")
	}
}

// A requested linked worktree that git no longer registers and whose
// directory is gone was removed (a round-end sweep's git worktree remove):
// the capture follows the live worktree list and leaves it out. One still
// registered but missing is not a removal and still fails.
func TestSnapshotDropsARemovedRequestedWorktree(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", "trees/\n")
	sh(t, repo, "add", ".")
	sh(t, repo, "commit", "-m", "ignore trees")
	for _, name := range []string{"gone", "kept", "lost"} {
		sh(t, repo, "worktree", "add", "-b", name, filepath.Join(repo, "trees", name))
	}
	sh(t, repo, "worktree", "remove", filepath.Join(repo, "trees", "gone"))
	s := rt().(wf.WorldSnapshotter)
	req := wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Worktrees: []string{"trees/gone", "trees/kept", "trees/lost"}}
	if err := s.SnapshotPreflight(req); err != nil {
		t.Fatalf("preflight refused a removed worktree: %v", err)
	}
	if _, err := s.(wf.WorldStamper).SnapshotStamp(req); err != nil {
		t.Fatalf("stamp of a removed worktree: %v", err)
	}
	raw, err := s.Snapshot(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "trees/gone") || !strings.Contains(string(raw), "trees/kept") {
		t.Fatalf("captured trees: %s", raw)
	}
	// Removed checkouts are left out only after the declared targets'
	// layout holds, in a direct capture as in the preflight (review round 1,
	// P2): duplicate, nested or overlapping absent paths are refused.
	for _, bad := range [][]string{{"trees/gone", "trees/gone"}, {"trees/gone", "trees/gone/inner"}, {"trees/kept", "trees/kept/gone"}} {
		bad := wf.SnapshotRequest{Workspace: repo, Directory: filepath.Join(t.TempDir(), "cp"), Worktrees: append(bad, "trees/lost")}
		if err := s.SnapshotPreflight(bad); err == nil {
			t.Errorf("preflight accepted %v", bad.Worktrees)
		}
		if _, err := s.Snapshot(bad); err == nil {
			t.Errorf("capture accepted %v", bad.Worktrees)
		}
		if _, err := os.Stat(bad.Directory); !os.IsNotExist(err) {
			t.Errorf("refused capture wrote %v", bad.Worktrees)
		}
	}
	if err := os.RemoveAll(filepath.Join(repo, "trees", "lost")); err != nil {
		t.Fatal(err)
	}
	req.Worktrees = []string{"trees/kept", "trees/lost"}
	if err := s.SnapshotPreflight(req); err == nil || !strings.Contains(err.Error(), "missing or symlink worktree") {
		t.Fatalf("registered but missing worktree: %v", err)
	}
}

// A client's declared install record rides the snapshot although the world's
// git ignores it (round-1119: a world ignoring .superpowers/ lost super
// install's manifest on rewind, and install then refused its own skills). It
// comes back byte for byte and untracked; undeclared ignored content, a
// declared file that is absent, and one behind a linked directory do not.
func TestSnapshotCarriesDeclaredIgnoredFiles(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", ".superpowers/\nlinked\nreal/\n")
	sh(t, repo, "add", ".gitignore")
	sh(t, repo, "commit", "-q", "-m", "ignore")
	if err := os.MkdirAll(filepath.Join(repo, ".superpowers", "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(".superpowers", "install-manifest.json")
	body := "{\"version\":1,\"files\":{\".claude/skills/x/SKILL.md\":{\"sha256\":\"ab\",\"exec\":false}}}\n"
	writeFile(t, repo, manifest, body)
	writeFile(t, repo, ".superpowers/state/other", "not declared\n")
	if err := os.Mkdir(filepath.Join(repo, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "real/record", "behind a link\n")
	if err := os.Symlink("real", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	before := sh(t, repo, "status", "--porcelain")
	s := rt().(wf.WorldSnapshotter)
	include := []string{manifest, ".superpowers/absent.json", filepath.Join("linked", "record")}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Include: []string{"../outside"}}); err == nil {
		t.Fatal("an inclusion outside the workspace passed preflight")
	}
	checkpoint := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint, Include: include})
	if err != nil {
		t.Fatal(err)
	}
	if sh(t, repo, "status", "--porcelain") != before {
		t.Fatal("source changed")
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: checkpoint}, raw); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(restored, manifest)); err != nil || string(got) != body {
		t.Fatalf("declared ignored file restored as %q (%v), want %q", got, err, body)
	}
	if got := sh(t, restored, "status", "--porcelain"); got != before {
		t.Fatalf("restored status %q, want %q: the record must come back untracked", got, before)
	}
	if got := sh(t, restored, "ls-files", "--", manifest); got != "" {
		t.Fatalf("the declared file was restored tracked: %q", got)
	}
	for _, rel := range []string{".superpowers/state/other", ".superpowers/absent.json", "real/record"} {
		if _, err := os.Stat(filepath.Join(restored, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was carried: %v", rel, err)
		}
	}

	// The stamp sees the declared file: a capture of a world whose record
	// changed underneath it is not one instant.
	st, ok := rt().(wf.WorldStamper)
	if !ok {
		t.Skip("no stamp capability")
	}
	req := wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Include: include}
	a, err := st.SnapshotStamp(req)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, manifest, body+"changed\n")
	if b, err := st.SnapshotStamp(req); err != nil || a == b {
		t.Fatalf("stamp unchanged (%v) after the declared file changed", err)
	}
}

// Declared files are captured and restored with no git content conversion:
// a CRLF record under "*.json text eol=lf" comes back CRLF, and an
// executable one comes back executable.
func TestSnapshotDeclaredFilesKeepBytesAndMode(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", ".superpowers/\n")
	writeFile(t, repo, ".gitattributes", "*.json text eol=lf\n*.lf text eol=crlf\n")
	sh(t, repo, "add", ".gitignore", ".gitattributes")
	sh(t, repo, "commit", "-q", "-m", "attrs")
	if err := os.MkdirAll(filepath.Join(repo, ".superpowers"), 0o755); err != nil {
		t.Fatal(err)
	}
	crlf := "{\r\n  \"version\": 1\r\n}\r\n"
	writeFile(t, repo, ".superpowers/install-manifest.json", crlf)
	lf := "kept\nas lf\n" // checkout under eol=crlf would write it CRLF
	writeFile(t, repo, ".superpowers/record.lf", lf)
	tool := filepath.Join(repo, ".superpowers", "hook")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := rt().(wf.WorldSnapshotter)
	checkpoint := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint, Include: []string{".superpowers/install-manifest.json", ".superpowers/record.lf", ".superpowers/hook"}})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: checkpoint}, raw); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, ".superpowers/install-manifest.json")); string(got) != crlf {
		t.Errorf("CRLF record restored as %q, want %q", got, crlf)
	}
	if got, _ := os.ReadFile(filepath.Join(restored, ".superpowers/record.lf")); string(got) != lf {
		t.Errorf("LF record under eol=crlf restored as %q, want %q", got, lf)
	}
	if info, err := os.Stat(filepath.Join(restored, ".superpowers/hook")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("executable record restored as %v (%v)", info, err)
	}
}

// What is at a declared path when the capture reads it decides alone: a
// directory (its ignored descendants included) or a leaf link is not
// carried, and the stamp reads by the same rule, so content behind a linked
// directory never moves it.
func TestSnapshotDeclaredPathOnlyAsARegularFile(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, ".gitignore", ".superpowers/\nelsewhere/\n")
	sh(t, repo, "add", ".gitignore")
	sh(t, repo, "commit", "-q", "-m", "ignore")
	for _, d := range []string{".superpowers/manifest.json", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, repo, ".superpowers/manifest.json/inside", "ignored descendant\n")
	writeFile(t, repo, "elsewhere/target", "linked target\n")
	if err := os.Symlink(filepath.Join("..", "elsewhere", "target"), filepath.Join(repo, ".superpowers", "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "elsewhere"), filepath.Join(repo, ".superpowers", "dirlink")); err != nil {
		t.Fatal(err)
	}
	include := []string{".superpowers/manifest.json", ".superpowers/leaf", ".superpowers/dirlink/target"}
	s := rt().(wf.WorldSnapshotter)
	checkpoint := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint, Include: include})
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := s.Restore(wf.RestoreRequest{Workspace: restored, Directory: checkpoint}, raw); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append(include, ".superpowers/manifest.json/inside") {
		if _, err := os.Lstat(filepath.Join(restored, rel)); !os.IsNotExist(err) {
			t.Errorf("%s was carried (%v)", rel, err)
		}
	}
	st := rt().(wf.WorldStamper)
	req := wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Include: include}
	a, err := st.SnapshotStamp(req)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "elsewhere/target", "changed behind the link\n")
	if b, err := st.SnapshotStamp(req); err != nil || a != b {
		t.Errorf("content behind a link moved the stamp (%v)", err)
	}
}

// Preflight applies git's path rules to every declared path's components.
func TestSnapshotPreflightRefusesUnsafeInclusions(t *testing.T) {
	repo := newRepo(t)
	s := rt().(wf.WorldSnapshotter)
	for _, rel := range []string{"", ".", "..", "../x", "/abs", "a/../b", "a//b", ".git/x", "a/.git/b", ".GIT/x", "a/.Git", ".git./x", ".git /x", "git~1/x", "GIT~1", ".git:stream/x", "a\\b",
		// git's HFS-ignorable code points inside .git (is_hfs_dotgit)
		".\u200cgit/x", ".g\u200dit/x", ".gi\u200et", "\u200f.git", ".git\u202a/x", ".\u202bgit", ".git\u202c", ".\u202dgit", ".\u202egit", ".\u206agit", ".\u206bgit", ".\u206cgit", ".\u206dgit", ".\u206egit", ".\u206fgit", "\ufeff.git/x", ".G\u200cIT/x"} {
		if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Include: []string{rel}}); err == nil {
			t.Errorf("inclusion %q passed preflight", rel)
		}
	}
	for _, rel := range []string{".superpowers/install-manifest.json", "a/b.git", ".gitignored", "github/x"} {
		if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Include: []string{rel}}); err != nil {
			t.Errorf("inclusion %q refused: %v", rel, err)
		}
	}
}

// racingStore is a SnapshotStore whose PutDir runs race first, as a repack
// or prune racing the capture would, and then takes nothing.
type racingStore struct{ race func() }

func (s racingStore) PutDir(rel, src string, skip func(string) bool) error {
	s.race()
	return nil
}

// A repack that replaces object files the capture listed refuses the
// capture instead of recording a checkpoint short of objects; one with no
// race takes the object files and leaves no snapshot refs behind.
func TestSnapshotObjectsRefuseARacingRepack(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo, "b.txt", "b\n")
	sh(t, repo, "add", "b.txt")
	sh(t, repo, "commit", "-q", "-m", "second") // loose objects a repack packs and prunes
	s := rt().(wf.WorldSnapshotter)
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Store: racingStore{func() {}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"Version":3`) {
		t.Fatalf("metadata %s", raw)
	}
	if refs := sh(t, repo, "for-each-ref", "refs/workflow/snapshots/"); refs != "" {
		t.Fatalf("snapshot refs left in the source: %s", refs)
	}
	_, err = s.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: t.TempDir(), Store: racingStore{func() {
		sh(t, repo, "repack", "-a", "-d", "-q")
		sh(t, repo, "prune-packed")
	}}})
	if err == nil || !strings.Contains(err.Error(), "objects changed during capture") {
		t.Fatalf("a racing repack = %v", err)
	}
}
