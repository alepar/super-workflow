package git_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestObservations(t *testing.T) {
	for _, c := range []struct {
		d      wf.ObservationDesc
		op, rt string
	}{
		{git.Head("w").Desc(), "head", "string"},
		{git.RevParse("w", "HEAD").Desc(), "rev_parse", "string"},
		{git.MergeBase("w", "a", "b").Desc(), "merge_base", "string"},
		{git.Status("w").Desc(), "status", "git.StatusResult"},
		{git.WorktreeList("r").Desc(), "worktree_list", "[]git.Worktree"},
	} {
		if c.d.Adapter != "git" || c.d.Op != c.op || c.d.Result != c.rt {
			t.Errorf("%s = %+v", c.op, c.d)
		}
	}
}

func TestEffects(t *testing.T) {
	k := wf.KeyOf("k")
	for op, e := range map[string]wf.ExternalEffect{
		"worktree_add":    git.WorktreeAdd(k, "p", "br", "base"),
		"worktree_remove": git.WorktreeRemove(k, "p"),
		"merge":           git.Merge(k, "w", "br", "task-x", true),
		"commit":          git.Commit(k, "w", "msg", "a.go"),
	} {
		if e.Adapter != "git" || e.Op != op || e.Key == nil {
			t.Errorf("%s = %+v", op, e)
		}
	}
}

func TestMergeCarriesTaskRef(t *testing.T) {
	e := git.Merge(wf.KeyOf("k"), "w", "abc123", "task-x", true)
	if got := e.Args["task_ref"]; got != "task-x" {
		t.Errorf("task_ref = %v, want task-x", got)
	}
	if got := e.Args["commit"]; got != "abc123" {
		t.Errorf("commit = %v, want abc123", got)
	}
	if got := git.MergeInto(wf.KeyOf("k"), "w", "br", "main", true).Args["task_ref"]; got != "" {
		t.Errorf("MergeInto task_ref = %v, want empty", got)
	}
}
