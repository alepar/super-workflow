package git_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A branch is never read as a revision, and a base or a ref never as an
// option or a range (sw-tagk).
func TestBoundRevisionsAreTyped(t *testing.T) {
	repo := newRepo(t)
	for _, tc := range []struct{ name, branch, base string }{
		{"branch as revision", "main~1", "main"},
		{"branch reflog", "main@{1}", "main"},
		{"base as option", "feat", "--detach"},
		{"base as range", "feat", "main..HEAD"},
	} {
		path := filepath.Join(t.TempDir(), "wt")
		if _, err := rt().Apply(repo, git.WorktreeAdd(wf.KeyOf("wt"), path, tc.branch, tc.base)); err == nil {
			t.Errorf("%s: worktree_add accepted branch %q base %q", tc.name, tc.branch, tc.base)
		}
	}
	for _, ref := range []string{"main..HEAD", "-q", "HEAD:a.txt"} {
		if _, err := rt().Read(repo, git.RevParse("", ref).Desc()); err == nil || !strings.Contains(err.Error(), "not a single git revision") {
			t.Errorf("rev_parse %q = %v, want the rev refusal", ref, err)
		}
	}
	if _, err := rt().Read(repo, git.MergeBase("", "main", "--all").Desc()); err == nil {
		t.Error("merge_base accepted an option-shaped revision")
	}
	// A good recut still resets to base.
	path := filepath.Join(t.TempDir(), "wt")
	if _, err := rt().Apply(repo, git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, "feat", "main")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "a.txt", "dirty\n")
	if _, err := rt().Apply(repo, git.WorktreeAddOrRecut(wf.KeyOf("wt"), path, "feat", "main")); err != nil {
		t.Fatalf("recut: %v", err)
	}
	if got := sh(t, path, "status", "--porcelain"); got != "" {
		t.Errorf("recut left %q", got)
	}
}

// DiffNames reads names as git has them (spaces, non-ASCII, pathspec
// characters), only between two full commit names, and honours anchored
// pathspecs.
func TestDiffNames(t *testing.T) {
	repo := newRepo(t)
	base := sh(t, repo, "rev-parse", "HEAD")
	for _, name := range []string{"my file.go", "café.go", ":notes.md", "evidence/x.md", "a*b.go"} {
		if dir := filepath.Dir(name); dir != "." {
			mustMkdir(t, filepath.Join(repo, dir))
		}
		writeFile(t, repo, name, "x\n")
	}
	sh(t, repo, "add", "-A")
	sh(t, repo, "commit", "-q", "-m", "second")
	head := sh(t, repo, "rev-parse", "HEAD")
	names := func(specs ...git.Pathspec) []string {
		t.Helper()
		var out []string
		for _, p := range read(t, repo, git.DiffNames("", base, head, specs...)) {
			out = append(out, p.String())
		}
		slices.Sort(out)
		return out
	}
	if got, want := names(), []string{":notes.md", "a*b.go", "café.go", "evidence/x.md", "my file.go"}; !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
	if got := names(git.Literal(form.MustParse[form.RepoPath]("a*b.go"))); !slices.Equal(got, []string{"a*b.go"}) {
		t.Errorf("literal a*b.go = %q", got)
	}
	if got := names(git.Glob(form.MustParse[form.RepoGlob]("**")), git.ExcludeGlob(form.MustParse[form.RepoGlob]("evidence/**"))); slices.Contains(got, "evidence/x.md") || len(got) != 4 {
		t.Errorf("exclude evidence = %q", got)
	}
	// From a subdirectory the anchored pathspec still means the top level.
	if got := read(t, filepath.Join(repo, "evidence"), git.DiffNames("", base, head, git.Literal(form.MustParse[form.RepoPath]("my file.go")))); len(got) != 1 || got[0].String() != "my file.go" {
		t.Errorf("from a subdirectory = %q", got)
	}
	for _, bad := range [][2]string{{"main", head}, {base, "HEAD"}, {base, base[:12]}} {
		if _, err := rt().Read(repo, git.DiffNames("", bad[0], bad[1]).Desc()); err == nil || !strings.Contains(err.Error(), "full lowercase git object name") {
			t.Errorf("diff_names %q..%q = %v, want the commit-id refusal", bad[0], bad[1], err)
		}
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}
