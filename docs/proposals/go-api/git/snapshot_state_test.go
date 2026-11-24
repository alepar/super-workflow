package git_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestSnapshotStateWorktreesPortableAcrossRoots(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "external", true: "nested"}[nested], func(t *testing.T) {
			repo := newRepo(t)
			state := t.TempDir()
			if nested {
				state = filepath.Join(repo, "state")
				if err := os.MkdirAll(state, 0700); err != nil {
					t.Fatal(err)
				}
			}
			rel := "runs/r/worktrees/task"
			wt := filepath.Join(state, rel)
			sh(t, repo, "worktree", "add", "-b", "task", wt)
			writeFile(t, wt, "a.txt", "staged task\n")
			sh(t, wt, "add", "a.txt")
			writeFile(t, wt, "a.txt", "dirty task\n")
			writeFile(t, wt, "untracked", "new\n")
			writeFile(t, repo, "a.txt", "workspace dirty\n")
			before := sh(t, wt, "status", "--porcelain")
			head := sh(t, wt, "rev-parse", "HEAD")
			indexPath := sh(t, wt, "rev-parse", "--path-format=absolute", "--git-path", "index")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			s := rt().(wf.WorldSnapshotter)
			cp := t.TempDir()
			raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, StateRoot: state, Directory: cp, StateWorktrees: []string{rel}})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), repo) || strings.Contains(string(raw), state) {
				t.Fatal("metadata contains source absolute paths")
			}
			var meta struct {
				Version int
				Trees   []struct{ Root, Path string }
			}
			if err = json.Unmarshal(raw, &meta); err != nil || meta.Version != 2 || len(meta.Trees) != 2 || meta.Trees[1].Root != "state" || meta.Trees[1].Path != rel {
				t.Fatalf("metadata %s: %v", raw, err)
			}
			after, _ := os.ReadFile(indexPath)
			if string(after) != string(index) || sh(t, wt, "rev-parse", "HEAD") != head {
				t.Fatal("snapshot changed source index or HEAD")
			}
			writeFile(t, wt, "a.txt", "source later\n")
			dest := t.TempDir()
			out := filepath.Join(dest, "workspace")
			outState := filepath.Join(t.TempDir(), "state")
			if err = os.MkdirAll(outState, 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, outState, "owned-artifact", "keep\n")
			if err = s.Restore(wf.RestoreRequest{Workspace: out, StateRoot: outState, Directory: cp}, raw); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(outState, rel)
			if got := sh(t, restored, "status", "--porcelain"); got != before {
				t.Fatalf("status %q want %q", got, before)
			}
			if got := sh(t, restored, "show", ":a.txt"); got != "staged task" {
				t.Fatal("staged contents lost")
			}
			if got, _ := os.ReadFile(filepath.Join(restored, "a.txt")); string(got) != "dirty task\n" {
				t.Fatal("dirty contents lost")
			}
			writeFile(t, restored, "a.txt", "fork mutation\n")
			if got, _ := os.ReadFile(filepath.Join(wt, "a.txt")); string(got) != "source later\n" {
				t.Fatal("fork touched source")
			}
			if err = os.RemoveAll(repo); err != nil {
				t.Fatal(err)
			}
			if !nested {
				if err = os.RemoveAll(state); err != nil {
					t.Fatal(err)
				}
			}
			out2 := filepath.Join(t.TempDir(), "workspace")
			outState2 := filepath.Join(t.TempDir(), "state")
			if err = s.Restore(wf.RestoreRequest{Workspace: out2, StateRoot: outState2, Directory: cp}, raw); err != nil {
				t.Fatalf("restore without source: %v", err)
			}
			if got := sh(t, filepath.Join(outState2, rel), "rev-parse", "--path-format=absolute", "--git-common-dir"); got != filepath.Join(out2, ".git") {
				t.Fatalf("linked to wrong repository: %s", got)
			}
		})
	}
}

func TestSnapshotStateRootPreflightRefusesUnsafeLayouts(t *testing.T) {
	repo := newRepo(t)
	state := t.TempDir()
	wt := filepath.Join(state, "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	s := rt().(wf.WorldSnapshotter)
	for _, bad := range []string{"", ".", "../escape", "/absolute", "task/../task", "task/.git/nested"} {
		cp := filepath.Join(t.TempDir(), "cp")
		if _, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{bad}, Directory: cp}); err == nil {
			t.Errorf("accepted %q", bad)
		}
		if _, err := os.Stat(cp); !os.IsNotExist(err) {
			t.Fatalf("preflight wrote checkpoint for %q", bad)
		}
	}
	for _, req := range []wf.SnapshotRequest{
		{Workspace: repo, StateRoot: repo, StateWorktrees: []string{"task"}},
		{Workspace: repo, StateRoot: filepath.Dir(repo), StateWorktrees: []string{"task"}},
		{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task", "task/nested"}},
		{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task", "task"}},
	} {
		req.Directory = t.TempDir()
		if err := s.SnapshotPreflight(req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	symlinkState := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(state, symlinkState); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: symlinkState, StateWorktrees: []string{"task"}, Directory: t.TempDir()}); err == nil {
		t.Fatal("accepted symlink state root")
	}
	if err := os.Symlink("a.txt", filepath.Join(wt, "safe-link")); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Directory: t.TempDir()}); err != nil {
		t.Fatalf("rejected internal relative symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(repo, "a.txt"), filepath.Join(wt, "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Directory: t.TempDir()}); err == nil {
		t.Fatal("accepted state worktree link to source")
	}
	if err := os.Remove(filepath.Join(wt, "unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Directory: filepath.Join(wt, "checkpoint")}); err == nil {
		t.Fatal("accepted checkpoint inside state worktree")
	}

	foreign := newRepo(t)
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: filepath.Dir(foreign), StateWorktrees: []string{filepath.Base(foreign)}, Directory: t.TempDir()}); err == nil {
		t.Fatal("accepted foreign state worktree")
	}
}

func TestSnapshotRestoreStateTargetsValidatedBeforeWrites(t *testing.T) {
	repo := newRepo(t)
	state := t.TempDir()
	sh(t, repo, "worktree", "add", "-b", "task", filepath.Join(state, "task"))
	s := rt().(wf.WorldSnapshotter)
	cp := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Directory: cp})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"absolute", "traversal", "empty", "root", "unknown-root", "no-root", "git-metadata", "duplicate", "nested", "existing-target", "symlink-state", "symlink-workspace-parent", "same-root", "state-contains-workspace", "state-inside-git", "checkpoint-overlap"} {
		t.Run(mode, func(t *testing.T) {
			var meta struct {
				Version int
				Trees   []map[string]string
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			req := wf.RestoreRequest{Workspace: filepath.Join(base, "workspace"), StateRoot: filepath.Join(base, "state"), Directory: cp}
			switch mode {
			case "absolute":
				meta.Trees[1]["Path"] = "/absolute"
			case "traversal":
				meta.Trees[1]["Path"] = "../escape"
			case "empty":
				meta.Trees[1]["Path"] = ""
			case "root":
				meta.Trees[1]["Path"] = "."
			case "unknown-root":
				meta.Trees[1]["Root"] = "foreign"
			case "no-root":
				req.StateRoot = ""
			case "git-metadata":
				meta.Trees[1]["Root"] = "workspace"
				meta.Trees[1]["Path"] = ".git/task"
			case "duplicate":
				meta.Trees = append(meta.Trees, meta.Trees[1])
			case "nested":
				extra := map[string]string{}
				for k, v := range meta.Trees[1] {
					extra[k] = v
				}
				extra["Path"] = "task/nested"
				meta.Trees = append(meta.Trees, extra)
			case "existing-target":
				if err := os.MkdirAll(filepath.Join(req.StateRoot, "task"), 0700); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(req.StateRoot, "task"), "marker", "keep")
			case "symlink-state":
				if err := os.Symlink(t.TempDir(), req.StateRoot); err != nil {
					t.Fatal(err)
				}
			case "symlink-workspace-parent":
				link := filepath.Join(base, "link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatal(err)
				}
				req.Workspace = filepath.Join(link, "workspace")
			case "same-root":
				req.StateRoot = req.Workspace
			case "state-contains-workspace":
				req.StateRoot = base
			case "state-inside-git":
				req.StateRoot = filepath.Join(req.Workspace, ".git", "state")
			case "checkpoint-overlap":
				req.StateRoot = cp
			}
			bad, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Restore(req, bad); err == nil {
				t.Fatalf("accepted %s", mode)
			}
			if _, err = os.Lstat(req.Workspace); !os.IsNotExist(err) {
				t.Fatalf("restore mutated workspace before rejecting %s: %v", mode, err)
			}
			if mode == "existing-target" {
				if content, _ := os.ReadFile(filepath.Join(req.StateRoot, "task", "marker")); string(content) != "keep" {
					t.Fatal("existing target changed")
				}
			}
		})
	}
}

func TestSnapshotRestoreV1WorkspaceMetadata(t *testing.T) {
	repo := newRepo(t)
	sh(t, repo, "worktree", "add", "-b", "task", filepath.Join(repo, "trees/task"))
	writeFile(t, repo, ".gitignore", "trees/\n")
	s := rt().(wf.WorldSnapshotter)
	cp := t.TempDir()
	raw, err := s.Snapshot(wf.SnapshotRequest{Workspace: repo, Worktrees: []string{"trees/task"}, Directory: cp})
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Version int
		Trees   []map[string]string
	}
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta.Version = 1
	for _, tree := range meta.Trees {
		delete(tree, "Root")
	}
	raw, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "workspace")
	if err = s.Restore(wf.RestoreRequest{Workspace: out, Directory: cp}, raw); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, filepath.Join(out, "trees/task"), "branch", "--show-current"); got != "task" {
		t.Fatalf("v1 linked checkout lost: %s", got)
	}
}

func TestSnapshotSymlinksRespectRelocatableStateBoundary(t *testing.T) {
	repo := newRepo(t)
	state := filepath.Join(repo, "state")
	wt := filepath.Join(state, "task")
	sh(t, repo, "worktree", "add", "-b", "task", wt)
	if err := os.Symlink("state/task/a.txt", filepath.Join(repo, "state-link")); err != nil {
		t.Fatal(err)
	}
	s := rt().(wf.WorldSnapshotter)
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Directory: t.TempDir()}); err == nil {
		t.Fatal("accepted link across roots that relocate independently")
	}
	if err := os.Remove(filepath.Join(repo, "state-link")); err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(repo, "trees/ordinary")
	sh(t, repo, "worktree", "add", "-b", "ordinary", ordinary)
	if err := os.Symlink("../../a.txt", filepath.Join(ordinary, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotPreflight(wf.SnapshotRequest{Workspace: repo, StateRoot: state, StateWorktrees: []string{"task"}, Worktrees: []string{"trees/ordinary"}, Directory: t.TempDir()}); err != nil {
		t.Fatalf("rejected relative link within workspace: %v", err)
	}
}

func TestSnapshotRestoreIgnoresHostInitTemplate(t *testing.T) {
	repo := newRepo(t)
	snapshotter := rt().(wf.WorldSnapshotter)
	checkpoint := t.TempDir()
	raw, err := snapshotter.Snapshot(wf.SnapshotRequest{Workspace: repo, Directory: checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "LocalConfig") {
		t.Fatal("fixture unexpectedly carries a local identity")
	}

	// A v1 checkpoint predates local config and used implicit workspace roots.
	var legacy struct {
		Version int
		Trees   []map[string]string
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.Version = 1
	for _, tree := range legacy.Trees {
		delete(tree, "Root")
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	template := t.TempDir()
	writeFile(t, template, "config", "[user]\n\tname = Host Template\n\temail = template@example.invalid\n[credential]\n\thelper = template-fixture\n")
	// The adapter strips GIT_CONFIG_* overrides, so give its git process a
	// real global config inside the fixture's isolated XDG config directory.
	configDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	sh(t, repo, "config", "--file", filepath.Join(configDir, "config"), "init.templateDir", template)

	for _, tc := range []struct {
		name     string
		metadata json.RawMessage
	}{
		{name: "unset local identity", metadata: raw},
		{name: "legacy metadata", metadata: legacyRaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "workspace")
			if err := snapshotter.Restore(wf.RestoreRequest{Workspace: out, Directory: checkpoint}, tc.metadata); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(sh(t, out, "config", "--local", "--no-includes", "--list"), "\n") {
				if strings.HasPrefix(line, "user.") || strings.HasPrefix(line, "credential.") {
					t.Errorf("restore imported host template setting %q", line)
				}
			}
		})
	}
}
