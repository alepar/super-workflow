package git

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest/hostile"
)

// hostileRepo is a fresh repository with one commit on main and an
// isolated git config.
func hostileRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hostileGit(t, repo, "init", "-q", "-b", "main")
	hostileWrite(t, filepath.Join(repo, "a.txt"), "a\n")
	hostileGit(t, repo, "add", "a.txt")
	hostileGit(t, repo, "commit", "-q", "-m", "first")
	return repo
}

func hostileGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %q: %v", args, err)
	}
	return string(out)
}

func hostileWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hostileFileName reports whether v can name one file in a directory.
func hostileFileName(v string) bool {
	return v != "" && v != "." && v != ".." && !strings.ContainsAny(v, "/\x00")
}

func hostileJSON(v string) string {
	if utf8.ValidString(v) {
		return v
	}
	b, _ := json.Marshal(v)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

// No ref is named by a corpus value, so a read of one must fail; one that
// answers has read the value as something else (an option, a revision
// expression).
func TestHostileRefReadsRefuse(t *testing.T) {
	repo := hostileRepo(t)
	r := Runtime(Config{})
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			for _, d := range []wf.ObservationDesc{
				RevParse("", in.Value).Desc(),
				ResolveCommit("", in.Value).Desc(),
				MergeBase("", in.Value, "HEAD").Desc(),
				MergeBase("", "HEAD", in.Value).Desc(),
			} {
				if out, err := r.Read(repo, d); err == nil {
					t.Errorf("%s(%q) answered %s, want a refusal", d.Op, in.Value, out)
				}
			}
			for _, d := range []wf.ObservationDesc{IsAncestor("", in.Value, "HEAD").Desc(), IsAncestor("", "HEAD", in.Value).Desc()} {
				if out, err := r.Read(repo, d); err == nil && string(out) == "true" {
					t.Errorf("is_ancestor %v answered true for a value that names no commit", d.Args)
				}
			}
		})
	}
}

// A commit message must be recorded as given, and Check must find the
// commit by its key trailer.
func TestHostileCommitMessageRoundTrip(t *testing.T) {
	repo := hostileRepo(t)
	r := Runtime(Config{})
	for i, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			hostileWrite(t, filepath.Join(repo, "a.txt"), strings.Repeat("x", i+1))
			e := Commit(wf.KeyOf("msg", in.Name), "", in.Value)
			if _, err := r.Apply(repo, e); err != nil {
				t.Skipf("refused: %v", err)
			}
			body := hostileGit(t, repo, "log", "-1", "--format=%B")
			body = strings.TrimRight(body, "\n")
			msg, ok := strings.CutSuffix(body, "\n\n"+trailer+e.KeyID())
			if !ok {
				msg, ok = strings.CutSuffix(body, trailer+e.KeyID())
			}
			if want := hostileJSON(in.Value); !ok || msg != want {
				t.Errorf("message %q recorded as %q", want, body)
			}
			if st, err := r.Check(repo, e); err != nil || st != wf.EffectDone {
				t.Errorf("Check after commit = %v, %v; want done", st, err)
			}
		})
	}
}

// Commit's paths name files, not patterns: a path with glob characters
// commits that one file, not every file the pattern matches.
func TestHostileCommitPathsAreLiteral(t *testing.T) {
	repo := hostileRepo(t)
	r := Runtime(Config{})
	glob := strings.NewReplacer("*", "zz", "?", "Q", "[ab]", "a", "[", "x", "]", "y")
	for i, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := hostileJSON(in.Value)
			if !hostileFileName(v) {
				t.Skip("not a file name")
			}
			hostileWrite(t, filepath.Join(repo, v), "v\n")
			hostileGit(t, repo, "add", "--", ":(literal)"+v)
			hostileGit(t, repo, "commit", "-q", "-m", "add "+in.Name)
			decoy := glob.Replace(v)
			files := []string{v}
			if decoy != v {
				hostileWrite(t, filepath.Join(repo, decoy), "d\n")
				hostileGit(t, repo, "add", "--", ":(literal)"+decoy)
				hostileGit(t, repo, "commit", "-q", "-m", "decoy "+in.Name)
				files = append(files, decoy)
			}
			for _, f := range files {
				hostileWrite(t, filepath.Join(repo, f), strings.Repeat("y", i+2))
			}
			if _, err := r.Apply(repo, Commit(wf.KeyOf("paths", in.Name), "", "change", v)); err != nil {
				t.Skipf("refused: %v", err)
			}
			out := hostileGit(t, repo, "show", "--name-only", "-z", "--format=", "HEAD")
			got := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
			if !slices.Equal(got, []string{v}) {
				t.Errorf("Commit(paths %q) committed %q", v, got)
			}
			// Leave nothing staged or modified for the next input.
			hostileGit(t, repo, "add", "-A")
			hostileGit(t, repo, "commit", "-q", "--allow-empty", "-m", "settle "+in.Name)
		})
	}
}

// Status reports an untracked file under its own name.
func TestHostileStatusPathsRoundTrip(t *testing.T) {
	r := Runtime(Config{})
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := hostileJSON(in.Value)
			if !hostileFileName(v) {
				t.Skip("not a file name")
			}
			repo := hostileRepo(t)
			hostileWrite(t, filepath.Join(repo, v), "v\n")
			raw, err := r.Read(repo, Status("").Desc())
			if err != nil {
				t.Skipf("refused: %v", err)
			}
			var st StatusResult
			if err := json.Unmarshal(raw, &st); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(st.Paths, []string{v}) {
				t.Errorf("status of untracked %q = paths %q", v, st.Paths)
			}
		})
	}
}

// A worktree added at a path lists at that path, and its effect checks done.
func TestHostileWorktreeListRoundTrip(t *testing.T) {
	repo := hostileRepo(t)
	r := Runtime(Config{})
	if err := os.Mkdir(filepath.Join(repo, "wts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			v := hostileJSON(in.Value)
			if !hostileFileName(v) {
				t.Skip("not a file name")
			}
			rel := filepath.Join("wts", v)
			e := WorktreeAdd(wf.KeyOf("wt", in.Name), rel, "b"+strings.Repeat("x", i), "HEAD")
			if _, err := r.Apply(repo, e); err != nil {
				t.Skipf("refused: %v", err)
			}
			raw, err := r.Read(repo, WorktreeList("").Desc())
			if err != nil {
				t.Fatalf("worktree_list: %v", err)
			}
			var list []Worktree
			_ = json.Unmarshal(raw, &list)
			found := false
			for _, w := range list {
				found = found || w.Path == filepath.Join(repo, rel)
			}
			if !found {
				t.Errorf("worktree at %q is not listed: %+v", filepath.Join(repo, rel), list)
			}
			if st, err := r.Check(repo, e); err != nil || st != wf.EffectDone {
				t.Errorf("Check after worktree_add = %v, %v; want done", st, err)
			}
		})
	}
}

// A path in a definition (a worktree, a worktree_add path) is the author's
// choice: a relative one resolves against the workspace as written, ".."
// included, and is not confined to it.
func TestHostileDefinitionPathsResolveAsWritten(t *testing.T) {
	ws := t.TempDir()
	for _, in := range hostile.Corpus() {
		t.Run(in.Name, func(t *testing.T) {
			want := filepath.Join(ws, in.Value)
			switch {
			case in.Value == "" || in.Value == ".":
				want = ws
			case filepath.IsAbs(in.Value):
				want = in.Value
			}
			if p := resolve(ws, in.Value); p != want {
				t.Errorf("relative path %q resolves to %q, want %q", in.Value, p, want)
			}
		})
	}
}

// CommitGlob matches its patterns as globs, the way a definition that
// commits a family of files asks for; Commit with the same text commits
// only a file of that literal name.
func TestCommitGlobMatchesPatterns(t *testing.T) {
	// An inherited pathspec mode must not change how either form matches.
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	repo := hostileRepo(t)
	r := Runtime(Config{})
	if err := os.Mkdir(filepath.Join(repo, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"specs/a-design.md", "specs/b-design.md", "specs/notes.md", "specs/*-design.md"} {
		hostileWrite(t, filepath.Join(repo, f), "v\n")
	}
	hostileGit(t, repo, "add", "-A")
	hostileGit(t, repo, "commit", "-q", "-m", "seed")
	for _, f := range []string{"specs/a-design.md", "specs/b-design.md", "specs/notes.md", "specs/*-design.md"} {
		hostileWrite(t, filepath.Join(repo, f), "changed\n")
	}
	committed := func() []string {
		out := hostileGit(t, repo, "show", "--name-only", "-z", "--format=", "HEAD")
		got := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
		slices.Sort(got)
		return got
	}
	if _, err := r.Apply(repo, Commit(wf.KeyOf("literal"), "", "literal", "specs/*-design.md")); err != nil {
		t.Fatal(err)
	}
	if got := committed(); !slices.Equal(got, []string{"specs/*-design.md"}) {
		t.Errorf("Commit committed %q, want only the file named specs/*-design.md", got)
	}
	if _, err := r.Apply(repo, CommitGlob(wf.KeyOf("glob"), "", "glob", "specs/*-design.md")); err != nil {
		t.Fatal(err)
	}
	if got := committed(); !slices.Equal(got, []string{"specs/a-design.md", "specs/b-design.md"}) {
		t.Errorf("CommitGlob committed %q, want the two design files", got)
	}
}

// A CommitGlob pattern that matches no file fails, as git add does.
func TestCommitGlobWithoutMatchFails(t *testing.T) {
	repo := hostileRepo(t)
	if _, err := Runtime(Config{}).Apply(repo, CommitGlob(wf.KeyOf("none"), "", "none", "nowhere/*.md")); err == nil {
		t.Error("CommitGlob of a pattern matching nothing succeeded")
	}
}
