package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// WritableCheck is the git-writable preflight check: the run's repo must
// accept the writes a task worktree needs.
//
// Owns: the real-operation .git write probe: a probe object written, a
// throwaway ref to it created and deleted, then a file created and removed
// under <common-dir>/worktrees/.
// Does not: check permission bits, run `git worktree add`, or word the fix;
// the remedy is wf.GitWritableRemedy, shared with the bootstrap notes.
// Extend by: nothing; another host fact is another wf.PreflightCheck.
func WritableCheck() wf.PreflightCheck {
	return wf.PreflightCheck{Name: "git-writable", Run: func(c wf.PreflightContext) *wf.Refusal {
		if err := probeGitWritable(c.Workspace); err != nil {
			return &wf.Refusal{Cause: err.Error(), Remedy: wf.GitWritableRemedy(c.Harness)}
		}
		return nil
	}}
}

// repoSelectors are the variables that point git at a repository other than
// the one -C names: the list `git rev-parse --local-env-vars` prints, plus
// the GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n pairs GIT_CONFIG_COUNT enables.
var repoSelectors = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// probeUnset is repoSelectors plus every inherited GIT_CONFIG_KEY_/VALUE_
// name, so the probe tests the workspace's repository and never one an
// inherited GIT_DIR names.
func probeUnset() []string {
	unset := append([]string{}, repoSelectors...)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			unset = append(unset, name)
		}
	}
	return unset
}

// probeGit runs git in dir through wf.RunTool, with hooks disabled and the
// repository selectors removed from its environment, returning stdout, and on
// failure an error carrying git's own message.
func probeGit(dir string, args ...string) (string, error) {
	res, err := wf.RunTool(wf.Tool{Bin: "git", Args: append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...), Dir: dir, UnsetEnv: probeUnset()})
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if res.Exit != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("exit %d", res.Exit)
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// probeSeq keeps probe ref names unique within one process.
var probeSeq atomic.Int64

// probeGitWritable runs the probe in workspace. Every created thing is removed
// before it returns, whether or not a later step failed. It needs no HEAD (an
// unborn repo passes): the ref points at a probe blob it writes itself. The
// probe blob has fixed content, so repeated probes leave at most one loose
// object, which gc prunes once nothing points at it.
func probeGitWritable(workspace string) (err error) {
	common, err := probeGit(workspace, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(workspace, common)
	}

	// The empty blob: RunTool gives git no stdin, so --stdin reads nothing.
	// It is a fixed object and needs no file outside the repository.
	obj, err := probeGit(workspace, "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("refs/wf/probe-%d-%d-%d", os.Getpid(), time.Now().UnixNano(), probeSeq.Add(1))
	if _, err := probeGit(workspace, "update-ref", ref, obj); err != nil {
		return err
	}
	defer func() {
		if _, derr := probeGit(workspace, "update-ref", "-d", ref); derr != nil {
			err = errors.Join(err, derr)
		}
	}()
	return probeWorktreesDir(filepath.Join(common, "worktrees"))
}

// probeWorktreesDir creates and removes a uniquely named file in dir, making
// dir first when it is missing. Concurrent probes share dir: one that finds it
// gone mid-probe makes it again, and a probe removes dir only when it made dir
// itself and dir is empty again; an existing or non-empty dir is never removed.
func probeWorktreesDir(dir string) error {
	for attempt := 0; ; attempt++ {
		made := false
		if err := os.Mkdir(dir, 0o755); err == nil {
			made = true
		} else if !os.IsExist(err) {
			return err
		}
		f, err := os.CreateTemp(dir, "probe-")
		if err != nil {
			if os.IsNotExist(err) && attempt < 3 {
				continue // another probe removed dir between our Mkdir and CreateTemp
			}
			return err
		}
		name := f.Name()
		cerr := f.Close()
		if rerr := os.Remove(name); rerr != nil {
			cerr = errors.Join(cerr, rerr)
		}
		if made {
			// Fails harmlessly when another probe's file or a real worktree
			// entry is there now; that dir is then left in place.
			if rerr := os.Remove(dir); rerr != nil && !os.IsNotExist(rerr) && !errors.Is(rerr, syscall.ENOTEMPTY) && !errors.Is(rerr, syscall.EEXIST) {
				cerr = errors.Join(cerr, rerr)
			}
		}
		return cerr
	}
}
