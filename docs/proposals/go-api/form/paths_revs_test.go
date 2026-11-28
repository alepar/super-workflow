package form_test

import (
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
)

func accepts[G form.Grammar](t *testing.T, ok, refused []string) {
	t.Helper()
	var g G
	for _, s := range ok {
		if _, err := form.Parse[G](s); err != nil {
			t.Errorf("%s refused %q: %v", g.Name(), s, err)
		}
	}
	for _, s := range refused {
		if v, err := form.Parse[G](s); err == nil {
			t.Errorf("%s accepted %q as %q", g.Name(), s, v)
		}
	}
}

func TestRepoPath(t *testing.T) {
	t.Parallel()
	accepts[form.RepoPath](t,
		[]string{"a.go", "vendor", "docs/my spec.md", "café.go", ":notes.md", "a#b.md", "app/[id]/page.tsx", "a*b", "..foo", "a\\b"},
		[]string{"", "/abs/a.go", "./a.go", "a/./b", "a/../b", "..", ".", "../x", "a//b", "a/", "a\nb", "a\tb", "a\x00b"})
}

func TestRepoGlobAndAbsPath(t *testing.T) {
	t.Parallel()
	accepts[form.RepoGlob](t, []string{"evidence/**", "*.go", "docs/*/x-design.md"}, []string{"/abs/**", "../x/*", "./*.go", "a\nb"})
	accepts[form.AbsPath](t, []string{"/", "/work/repo", "/work/my repo/a#b.md"}, []string{"rel", "/a/../b", "/a/", "//a", "/a/./b", "/a\nb"})
}

func TestRepoPathOfAndJoin(t *testing.T) {
	t.Parallel()
	root := form.MustParse[form.AbsPath]("/work/repo")
	p, err := form.RepoPathOf(root, form.MustParse[form.AbsPath]("/work/repo/docs/x.md"))
	if err != nil || p.String() != "docs/x.md" {
		t.Fatalf("RepoPathOf = %q, %v", p, err)
	}
	if back := form.Join(root, p); back.String() != "/work/repo/docs/x.md" {
		t.Fatalf("Join = %q", back)
	}
	for _, outside := range []string{"/work/repo", "/work/repo2/x", "/work/x", "/"} {
		if _, err := form.RepoPathOf(root, form.MustParse[form.AbsPath](outside)); !errors.Is(err, form.ErrOutsideRoot) {
			t.Errorf("RepoPathOf(%q) = %v, want ErrOutsideRoot", outside, err)
		}
	}
	top := form.MustParse[form.AbsPath]("/")
	if p, err := form.RepoPathOf(top, form.MustParse[form.AbsPath]("/a/b")); err != nil || p.String() != "a/b" {
		t.Fatalf("RepoPathOf(/, /a/b) = %q, %v", p, err)
	}
}

func TestCommitIDRevAndBranchName(t *testing.T) {
	t.Parallel()
	sha1 := "eba2fc6900000000000000000000000000000000"
	accepts[form.CommitID](t, []string{sha1, sha1 + "000000000000000000000000"}, []string{"eba2fc69", "EBA2FC6900000000000000000000000000000000", sha1 + "0", "main"})
	accepts[form.Rev](t, []string{"main", "HEAD~1", "origin/main", "v1.0", sha1, "main^{commit}", "@{u}", "main@{1}"},
		[]string{"-q", "--detach", "a..b", "a...b", "HEAD:file", ":/fix", "a b", "a\nb"})
	accepts[form.BranchName](t, []string{"main", "super-auto/demo", "task-spw-1.2", "a.b"},
		[]string{"main~1", "main@{1}", "-x", "a..b", "a/", "a.", ".a", "a/.b", "a.lock", "a/b.lock/c", "@", "a b", "a:b", "a//b", "a*", "a[b", "a\\b"})
}

func TestRevRange(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]form.RevRange{
		"main..feature":  {Base: form.MustParse[form.Rev]("main"), Head: form.MustParse[form.Rev]("feature")},
		"main...feature": {Base: form.MustParse[form.Rev]("main"), Head: form.MustParse[form.Rev]("feature"), Symmetric: true},
	} {
		got, err := form.ParseRevRange(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("ParseRevRange(%q) = %+v %v (string %q), want %+v", in, got, err, got.String(), want)
		}
	}
	for _, bad := range []string{"main", "main..", "..main", "main....feature", "a..b..c", "-x..main", "main..-x", "a b..c"} {
		if r, err := form.ParseRevRange(bad); err == nil {
			t.Errorf("ParseRevRange(%q) = %+v, want refused", bad, r)
		}
	}
}
