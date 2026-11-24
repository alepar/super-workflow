package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	bdAgent struct{}
	bdOut   struct{}
)

func boundDef(name, prompt string, ins ...wf.Input) *wf.Def {
	d := wf.New(name)
	d.Param("dir")
	a := wf.NewAgent[bdAgent, bdOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText(prompt, ins...), wf.Retry[bdAgent](wf.Const(2), a.To(bad, "x")))
	next := a.To(done, "done")
	a.Route(func(bdOut, wf.Ctx) wf.Edge[bdAgent] { return next }, next)
	return d
}

func TestBoundInputFillsBracketsAndInputsBlock(t *testing.T) {
	d := boundDef("bound", "Work in [WORKTREE] on [BRANCH]; keep [OTHER] and {{BRANCH}}.",
		wf.Bound("WORKTREE", "{workspace}/{dir}/wt"), wf.Bound("BRANCH", "task-{dir}"))
	ws := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"dir": "x1"})
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions %+v", ins)
	}
	p := ins[0].Prompt
	wt := ws + "/x1/wt"
	if !strings.Contains(p, "Work in "+wt+" on task-x1; keep [OTHER] and task-x1.") {
		t.Errorf("brackets not filled (an unnamed token must stay):\n%s", p)
	}
	// Both inputs filled a token, so the Inputs block does not repeat them.
	if strings.Contains(p, "## Inputs") {
		t.Errorf("consumed bound inputs repeated in an Inputs block:\n%s", p)
	}
	if ins[0].Inputs["WORKTREE"] != wt || ins[0].Inputs["BRANCH"] != "task-x1" {
		t.Errorf("instruction inputs %+v, want both bound inputs", ins[0].Inputs)
	}
	var view []string
	for _, s := range wf.Inspect(d).States {
		if s.Name == "bdAgent" {
			view = s.Inputs
		}
	}
	if len(view) != 2 || !strings.Contains(view[0], "WORKTREE") || !strings.Contains(view[0], "{workspace}/{dir}/wt") {
		t.Errorf("view inputs %q, want both bound inputs with their templates", view)
	}
}

func TestBoundUnresolvedIsBlocked(t *testing.T) {
	d := boundDef("bound-bad", "Go to [WHERE].", wf.Bound("WHERE", "{nosuch}/x"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, map[string]string{"dir": "x"})
	h.next(run)
	st := h.status(run)
	if st.Phase != wf.RunBlocked || !strings.Contains(st.Instances[0].Reason, "nosuch") {
		t.Fatalf("status = %+v, want blocked naming the unresolved placeholder", st)
	}
}

func boundPrompt(t *testing.T, prompt string, ins ...wf.Input) string {
	t.Helper()
	d := boundDef("bound-tok", prompt, ins...)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, map[string]string{"dir": "x1"})
	got := h.next(run)
	if len(got) != 1 {
		t.Fatalf("instructions %+v", got)
	}
	p := afterRules(t, got[0].Prompt)
	if i := strings.Index(p, "## Inputs"); i >= 0 {
		p = p[:i]
	}
	return p
}

func TestBoundFillsLongTokens(t *testing.T) {
	got := boundPrompt(t, "Review [ARTIFACT — per the caller's mode: the spec] now; [FULL TEXT of the blocker bead] and [for each child task: read it].",
		wf.Bound("ARTIFACT", "spec.md"), wf.Bound("FULL TEXT of", "the reason"), wf.Bound("for each child task", "the children"))
	if !strings.HasPrefix(got, "Review spec.md now; the reason and the children.") {
		t.Errorf("long tokens not filled:\n%s", got)
	}
}

func TestBoundLongestNameWins(t *testing.T) {
	got := boundPrompt(t, "[ROOT_SPEC_PATH] [ROOT]", wf.Bound("ROOT", "short"), wf.Bound("ROOT_SPEC_PATH", "long"))
	if !strings.HasPrefix(got, "long short") {
		t.Errorf("a name that is a prefix of another must not take its token: %q", got)
	}
	// A prefix is not a name unless a non-alphanumeric character follows it.
	if got := boundPrompt(t, "[ROOTED]", wf.Bound("ROOT", "short")); !strings.HasPrefix(got, "[ROOTED]") {
		t.Errorf("[ROOTED] must stay: %q", got)
	}
}

func TestBoundFillsNestedToken(t *testing.T) {
	got := boundPrompt(t, "Pass [The blocked task's brief (the [plan file name] section)] on.", wf.Bound("The blocked task's", "/b/brief.md"))
	if !strings.HasPrefix(got, "Pass /b/brief.md on.") {
		t.Errorf("nested token not filled as one: %q", got)
	}
}

func TestBoundLeavesOtherBracketsAlone(t *testing.T) {
	got := boundPrompt(t, "[Must-fix] [ ] [x] [--stalled <phase>] [K] [SEV]", wf.Bound("K", "3"))
	if !strings.HasPrefix(got, "[Must-fix] [ ] [x] [--stalled <phase>] 3 [SEV]") {
		t.Errorf("only the named token may change: %q", got)
	}
	// A value is not rescanned.
	if got := boundPrompt(t, "[A] [B]", wf.Bound("A", "[B]"), wf.Bound("B", "b")); !strings.HasPrefix(got, "[B] b") {
		t.Errorf("a value was rescanned: %q", got)
	}
}

func TestBoundValueDoesNotBecomePromptSource(t *testing.T) {
	const value = "watchedSignals() = {os.Interrupt}; []int{1}; {{unknown}}"
	d := boundDef("bound-literal", "[DATA] | {{dir}} | [other {{dir}}]", wf.Bound("DATA", "{dir}"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, map[string]string{"dir": value})
	got := h.next(run)
	want := value + " | " + value + " | [other " + value + "]"
	if len(got) != 1 || !strings.Contains(got[0].Prompt, want) {
		t.Fatalf("instructions %+v, want literal text %q", got, want)
	}
}

// An input no token names (a template file the agent reads names it) stays in
// the Inputs block: that is the only place its value reaches the agent.
func TestUnconsumedInputStaysInTheInputsBlock(t *testing.T) {
	d := boundDef("bound-file", "Read the template and follow it with the values below.",
		wf.Bound("WORKTREE", "{workspace}/{dir}/wt"))
	ws := t.TempDir()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: ws})
	run := h.run(d, map[string]string{"dir": "x1"})
	ins := h.next(run)
	if len(ins) != 1 || !strings.Contains(ins[0].Prompt, "## Inputs") || !strings.Contains(ins[0].Prompt, "### WORKTREE\n\n"+ws+"/x1/wt") {
		t.Fatalf("an unconsumed input left the Inputs block: %+v", ins)
	}
}

// A prompt that consumes one input and not another lists only the second,
// under one Inputs header; a case-insensitive {{token}} and a prefix-matched
// bracket token both count as consuming.
func TestInputsBlockListsOnlyTheUnconsumed(t *testing.T) {
	p := boundPromptFull(t, "Work in [WORKTREE — the task's own] on {{branch}}.",
		wf.Bound("WORKTREE", "{workspace}/wt"), wf.Bound("BRANCH", "task-{dir}"), wf.Bound("NOTE", "keep {dir}"))
	if strings.Count(p, "## Inputs") != 1 || strings.Contains(p, "### WORKTREE") || strings.Contains(p, "### BRANCH") || !strings.Contains(p, "### NOTE\n\nkeep x1") {
		t.Fatalf("Inputs block, want NOTE alone:\n%s", p)
	}
}

func boundPromptFull(t *testing.T, prompt string, ins ...wf.Input) string {
	t.Helper()
	d := boundDef("bound-mixed", prompt, ins...)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Workspace: t.TempDir()})
	run := h.run(d, map[string]string{"dir": "x1"})
	got := h.next(run)
	if len(got) != 1 {
		t.Fatalf("instructions %+v", got)
	}
	return got[0].Prompt
}
