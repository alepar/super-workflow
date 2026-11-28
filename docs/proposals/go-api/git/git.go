// Package git is the git adapter: constructors return descriptors and Runtime
// serves them through the git CLI.
//
// Reads return wf.Observation values; writes return wf.ExternalEffect values
// whose idempotency Key is the first, positional parameter.
package git

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// StatusResult is the parsed working-tree status: Clean is true when Paths is
// empty.
type StatusResult struct {
	Clean bool
	Paths []string
}

// Worktree is one entry of `git worktree list`.
type Worktree struct{ Path, Branch, Head string }

// Head observes `git rev-parse HEAD` in worktree.
func Head(worktree string) wf.Observation[string] {
	return wf.Observe[string]("git", "head", map[string]any{"worktree": worktree})
}

// RevParse observes `git rev-parse <ref>` in worktree.
func RevParse(worktree, ref string) wf.Observation[string] {
	return wf.Observe[string]("git", "rev_parse", map[string]any{"worktree": worktree, "ref": ref})
}

// MergeBase observes `git merge-base <a> <b>` in worktree.
func MergeBase(worktree, a, b string) wf.Observation[string] {
	return wf.Observe[string]("git", "merge_base", map[string]any{"worktree": worktree, "a": a, "b": b})
}

// ResolveCommit observes `git rev-parse --verify <rev>^{commit}` in worktree:
// the full SHA of the commit rev names; the read fails when rev names no
// commit in the repository.
func ResolveCommit(worktree, rev string) wf.Observation[string] {
	return wf.Observe[string]("git", "resolve_commit", map[string]any{"worktree": worktree, "ref": rev})
}

// IsAncestor observes `git merge-base --is-ancestor <a> <b>` in worktree:
// true when a is an ancestor of, or equal to, b.
func IsAncestor(worktree, a, b string) wf.Observation[bool] {
	return wf.Observe[bool]("git", "is_ancestor", map[string]any{"worktree": worktree, "a": a, "b": b})
}

// DiffNames observes the files that differ between two commits, base and
// head (`git diff --name-only -z --no-renames <base> <head>`), named from the
// repository's top level, limited to specs when any are given. base and head
// must bind to full commit object names (form.CommitID): a review resolves
// its range once, base as the merge base, and every diff runs on that
// recorded pair, so a moving branch never changes what was reviewed. A read
// whose base or head is not a full SHA is refused.
func DiffNames(worktree, base, head string, specs ...Pathspec) wf.Observation[[]form.Of[form.RepoPath]] {
	texts := make([]string, len(specs))
	for i, s := range specs {
		texts[i] = s.String()
	}
	return wf.Observe[[]form.Of[form.RepoPath]]("git", "diff_names", map[string]any{"worktree": worktree, "a": base, "b": head, "specs": texts})
}

// Pathspec is one git pathspec, anchored at the repository's top level so it
// means the same from any directory, and saying how it matches: a literal
// file name or a glob, included or excluded. It is made only by its
// constructors.
type Pathspec struct{ s string }

// Literal matches the file p exactly: * [ ? and a leading : are part of its
// name.
func Literal(p form.Of[form.RepoPath]) Pathspec { return pathspec("top,literal", p.String()) }

// Glob matches the files g names (* within an element, ** across them).
func Glob(g form.Of[form.RepoGlob]) Pathspec { return pathspec("top,glob", g.String()) }

// ExcludeLiteral leaves out the file p.
func ExcludeLiteral(p form.Of[form.RepoPath]) Pathspec {
	return pathspec("top,exclude,literal", p.String())
}

// ExcludeGlob leaves out the files g names.
func ExcludeGlob(g form.Of[form.RepoGlob]) Pathspec { return pathspec("top,exclude,glob", g.String()) }

func pathspec(magic, p string) Pathspec {
	if p == "" {
		panic("git.Pathspec: absent path")
	}
	return Pathspec{":(" + magic + ")" + p}
}

// String is the pathspec as git reads it, for an argv or a script.
func (p Pathspec) String() string { return p.s }

// Status observes `git status --porcelain` in worktree.
func Status(worktree string) wf.Observation[StatusResult] {
	return wf.Observe[StatusResult]("git", "status", map[string]any{"worktree": worktree})
}

// WorktreeList observes `git worktree list --porcelain` in repo.
func WorktreeList(repo string) wf.Observation[[]Worktree] {
	return wf.Observe[[]Worktree]("git", "worktree_list", map[string]any{"repo": repo})
}

// WorktreeAdd creates a worktree at path on a new branch cut from base. An
// existing branch is reused as it is: the worktree is added on its tip.
//
// Idempotent by key. already_done: path is already listed by `git worktree
// list` on branch; the adapter then leaves it untouched (it never recuts or
// resets an existing one).
func WorktreeAdd(key wf.Key, path, branch, base string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "worktree_add", Args: map[string]any{"path": path, "branch": branch, "base": base}, Key: key}
}

// WorktreeAddOrRecut is WorktreeAdd for the workspace of a unit that can be
// reissued: an existing clean worktree on branch is reused; one holding
// partial work (uncommitted changes) is re-cut from base. It never fails on
// an existing branch or directory.
//
// Idempotent by key. already_done: as WorktreeAdd, and the worktree is clean
// and its directory exists.
func WorktreeAddOrRecut(key wf.Key, path, branch, base string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "worktree_add", Args: map[string]any{"path": path, "branch": branch, "base": base, "recut": true}, Key: key}
}

// WorktreeRemove removes the worktree at path. It never passes --force: a
// worktree with uncommitted work fails the effect instead of losing the work.
//
// Idempotent. already_done: path is absent from `git worktree list`.
func WorktreeRemove(key wf.Key, path string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "worktree_remove", Args: map[string]any{"path": path}, Key: key}
}

// Merge merges commit into the branch checked out in worktree (noFF adds
// --no-ff). It never rebases: history is merged, never rewritten (invariant
// 11). With taskRef set (a task merge, passing the reviewed {head}) commit
// must be a full commit SHA, so the merge lands exactly the commit HeadCheck
// accepted and never re-resolves a name; with taskRef empty (a base absorb,
// MergeInto) it may be any name git resolves to a commit, such as a branch.
// Apply refuses a commit that is not a full SHA where one is required, and
// any commit starting with "-".
//
// taskRef names the task ref (task-{id}) the merged commit stands for. Before
// merging, Apply compares the ref's target with commit; when they differ (the
// agent moved the ref after it reported done) its result carries a
// wf.PostDoneMutationReport, on a clean and on a conflicting merge alike, and
// the engine journals post_done_mutation. Only commit is merged. Empty means
// no comparison.
//
// Idempotent. already_done: `git merge-base --is-ancestor <commit> HEAD`
// succeeds in worktree, so the commit is already contained.
func Merge(key wf.Key, worktree, commit, taskRef string, noFF bool) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "merge", Args: map[string]any{"worktree": worktree, "commit": commit, "task_ref": taskRef, "no_ff": noFF}, Key: key}
}

// MergeInto is Merge into a checkout the run does not own (the human's main
// checkout at the finish): it acts only when the branch checked out in
// worktree is into, so a checkout left on another branch is never merged into.
//
// Idempotent. already_done: into is checked out and `git merge-base
// --is-ancestor <branch> HEAD` succeeds; on any other branch it is not done
// and Apply fails.
func MergeInto(key wf.Key, worktree, branch, into string, noFF bool) wf.ExternalEffect {
	e := Merge(key, worktree, branch, "", noFF)
	e.Args["into"] = into
	return e
}

// Commit commits paths (all tracked changes when none are given) in worktree
// with message.
//
// Idempotent by key. already_done: the adapter finds a commit on HEAD whose
// message carries the key trailer; otherwise a clean tree means there is
// nothing to commit and the effect is a no-op.
//
// Each path names one file, matched literally: a * or [ in a path (often a
// bound value) is part of its name. CommitGlob matches patterns.
func Commit(key wf.Key, worktree, message string, paths ...string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "commit", Args: map[string]any{"worktree": worktree, "message": message, "paths": paths}, Key: key}
}

// CommitGlob is Commit with git glob patterns (* within a path component, **
// across them) in place of literal paths: it commits every changed file a
// pattern matches. A pattern that matches no file fails the effect, as git
// add does. Use it only for a pattern the definition writes; a value bound
// into it is matched as a pattern too.
func CommitGlob(key wf.Key, worktree, message string, patterns ...string) wf.ExternalEffect {
	return wf.ExternalEffect{Adapter: "git", Op: "commit", Args: map[string]any{"worktree": worktree, "message": message, "globs": patterns}, Key: key}
}
