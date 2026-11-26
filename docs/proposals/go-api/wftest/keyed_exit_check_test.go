package wftest_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	keWork struct{}
	keOut  struct{}
	keDone struct{}
	keBad  struct{}
)

// keyedEdgeLoopDef carries a keyed shell.run on an agent's edge; a failed
// effect blocks the edge and loops back to the agent, which takes it again.
func keyedEdgeLoopDef() (*wf.Def, *wf.Agent[keWork, keOut]) {
	d := wf.New("keyed-exit-loop")
	a := wf.NewAgent[keWork, keOut](d)
	done := wf.NewTerminal[keDone](d, wf.Success)
	bad := wf.NewTerminal[keBad](d, wf.Stopped)
	a.Spec(wf.PromptText("Work."), wf.Retry[keWork](wf.Const(3), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(a, "effect failed, try again")))
	a.Then(a.To(done, "done").With(shell.Effect(wf.KeyOf("check"), shell.Run("scripts/check"))))
	return d, a
}

// sw-yvoy: a keyed command scripted to exit non-zero is not done for the
// world's check, as the real shell adapter answers, so the next traversal of
// its edge runs the command again rather than skipping it as done.
func TestKeyedCommandScriptedNonZeroExitIsReapplied(t *testing.T) {
	d, a := keyedEdgeLoopDef()
	w := wftest.NewWorld()
	wftest.ExitOnce(w, 1, "scripts/check")
	tr := wftest.New(d, wftest.WithWorld(w)).Run(wftest.Submit(a, keOut{}), wftest.Submit(a, keOut{}))
	wftest.AssertRoute(t, tr, "keWork", "keWork", "keDone")
	runs := 0
	for _, e := range wftest.Applied(w) {
		if e.Adapter == "shell" && e.Op == "run" {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("shell.run applied %d times, want 2 (the failed run, then the re-run)", runs)
	}
	if st, err := w.Check("", wf.ExternalEffect{Adapter: "shell", Op: "run", Key: wf.KeyOf("check")}); err != nil || st != wf.EffectDone {
		t.Fatalf("check after the passing re-run = %v, %v; want done", st, err)
	}
}
