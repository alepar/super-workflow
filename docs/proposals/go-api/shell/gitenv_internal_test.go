package shell

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A workflow command run with a hostile inherited GIT_DIR acts on its
// workspace's repository, inline and through the detached runner; identity
// and config variables still arrive (sw-itkk).
func TestACommandIgnoresAnInheritedRepositorySelector(t *testing.T) {
	ws, other := gitRepo(t), gitRepo(t)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_AUTHOR_NAME", "Fixture Author")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	argv := []string{"sh", "-c", `git rev-parse --show-toplevel; echo "author=$GIT_AUTHOR_NAME count=$GIT_CONFIG_COUNT"; echo "dir=${GIT_DIR-unset} tree=${GIT_WORK_TREE-unset} index=${GIT_INDEX_FILE-unset}"`}
	want := ws + "\nauthor=Fixture Author count=0\ndir=unset tree=unset index=unset\n"

	raw, err := Runtime(Config{}).Read(ws, Observe(Run(argv...)).Desc())
	if err != nil {
		t.Fatal(err)
	}
	var inline wf.RunResult
	if err := json.Unmarshal(raw, &inline); err != nil {
		t.Fatal(err)
	}
	if inline.Exit != 0 || inline.Stdout != want {
		t.Errorf("inline: %+v, want stdout %q", inline, want)
	}
	if res := runArgv(argv, ws, filepath.Join(t.TempDir(), "full"), ""); res.Exit != 0 || res.Stdout != want {
		t.Errorf("detached runner: %+v, want stdout %q", res, want)
	}
}

// gitRepo initializes a repository in a fresh directory and returns its
// resolved path, as git rev-parse --show-toplevel prints it.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return dir
}
