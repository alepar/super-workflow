package form

import (
	"errors"
	"path"
	"strings"
)

// Paths have one type per representation. A RepoPath is relative to a
// repository's top level; an AbsPath is absolute. Neither converts to the
// other without the base it needs (RepoPathOf, Join), so comparing the two
// does not compile. Both are lexical: symlink spellings of one directory are
// unified where a root enters the program, not here.

// RepoPath is a clean, slash-separated path relative to a repository's top
// level, as git names a file: not empty, not absolute, no "." or ".."
// element, no empty element, no control byte. Characters git gives meaning
// to in a pathspec (* ? [ ] and a leading :) are part of the name: a
// RepoPath goes to git through a literal pathspec, never as a bare
// argument.
type RepoPath struct{}

// ErrNotRepoPath is the refusal of a string that is not a RepoPath.
var ErrNotRepoPath = errors.New("not a clean repository-relative path")

// pathMax bounds a path value, which an agent's output may carry.
const pathMax = 4096

func (RepoPath) Name() string { return "repo-path" }

func (RepoPath) Check(s string) error {
	if !cleanRelative(s) || hasControl(s) {
		return ErrNotRepoPath
	}
	return nil
}

// Pattern is what an agent's schema shows: a subset of the format that is
// easy to sample. Check stays the authority.
func (RepoPath) Pattern() string { return `^[A-Za-z0-9_@+~\[\]-][A-Za-z0-9_.@+~/\[\]-]*$` }

func (RepoPath) Samples() []string {
	return []string{"vendor", "docs/superpowers/specs/x-design.md", "app/[id]/page.tsx", "my file.go", "café.go", ":notes.md", "a#b.md"}
}

// RepoGlob is a git glob pattern relative to a repository's top level: *
// within an element, ** across elements, ? and [...]. It has the form of a
// RepoPath and is used only through a glob pathspec.
type RepoGlob struct{}

// ErrNotRepoGlob is the refusal of a string that is not a RepoGlob.
var ErrNotRepoGlob = errors.New("not a clean repository-relative glob")

func (RepoGlob) Name() string { return "repo-glob" }

func (RepoGlob) Check(s string) error {
	if !cleanRelative(s) || hasControl(s) {
		return ErrNotRepoGlob
	}
	return nil
}

func (RepoGlob) Samples() []string {
	return []string{"evidence/**", "docs/superpowers/specs/*-design.md", "vendor/**", "*.go"}
}

// AbsPath is a clean absolute path: it starts with "/", has no "." or ".."
// element, no empty element and no control byte.
type AbsPath struct{}

// ErrNotAbsPath is the refusal of a string that is not an AbsPath.
var ErrNotAbsPath = errors.New("not a clean absolute path")

func (AbsPath) Name() string { return "abs-path" }

func (AbsPath) Check(s string) error {
	if len(s) > pathMax || !path.IsAbs(s) || path.Clean(s) != s || hasControl(s) {
		return ErrNotAbsPath
	}
	return nil
}

func (AbsPath) Samples() []string { return []string{"/", "/work/repo", "/work/my repo/a#b.md"} }

// ErrOutsideRoot is RepoPathOf's refusal of a path that is not below its root.
var ErrOutsideRoot = errors.New("path is not below the root")

// RepoPathOf is p relative to root, the repository's top level. It refuses p
// when it is root itself or not below it.
func RepoPathOf(root, p Of[AbsPath]) (Of[RepoPath], error) {
	r, s := root.String(), p.String()
	if r == "" || s == "" {
		return Of[RepoPath]{}, ErrEmpty
	}
	prefix := r
	if prefix != "/" {
		prefix += "/"
	}
	if !strings.HasPrefix(s, prefix) || s == r {
		return Of[RepoPath]{}, &Error{Grammar: RepoPath{}.Name(), Value: s, Err: ErrOutsideRoot}
	}
	return Parse[RepoPath](s[len(prefix):])
}

// Join is p below root.
func Join(root Of[AbsPath], p Of[RepoPath]) Of[AbsPath] {
	if root.IsZero() || p.IsZero() {
		panic("form.Join: absent root or path")
	}
	return Of[AbsPath]{path.Join(root.String(), p.String())}
}

// cleanRelative reports a non-empty, clean, relative, slash-separated path
// with no "." or ".." element.
func cleanRelative(s string) bool {
	return len(s) <= pathMax && !path.IsAbs(s) && path.Clean(s) == s && s != "." && s != ".." && !strings.HasPrefix(s, "../")
}

// hasControl reports an ASCII or C1 control byte, which no line or terminal
// format carries back unchanged.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

func init() {
	Register[RepoPath]()
	Register[RepoGlob]()
	Register[AbsPath]()
}
