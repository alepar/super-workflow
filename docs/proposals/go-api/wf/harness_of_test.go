package wf_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	hfPre    struct{}
	hfA      struct{}
	hfB      struct{}
	hfC      struct{}
	hfAfter  struct{}
	hfDone   struct{}
	hfGaveUp struct{}
	hfOut    struct{ N int }
)

// harnessOfDef reads HarnessOf around three agents: pre before any output
// is accepted, a (the coordinator's own) and b (on the claude family) each
// in its own check, c (the coordinator's own) in its check, and after once
// all three accepted. seen keeps each read by reader. a takes retries
// attempts.
func harnessOfDef(name string, retries int, seen map[string]string) *wf.Def {
	d := wf.New(name)
	var a *wf.Agent[hfA, hfOut]
	var bb *wf.Agent[hfB, hfOut]
	var cc *wf.Agent[hfC, hfOut]
	pre := wf.NewCompute[hfPre](d, func(x wf.Ctx) (struct{}, error) {
		seen["pre"] = wf.HarnessOf(x, a)
		return struct{}{}, nil
	})
	a = wf.NewAgent[hfA, hfOut](d)
	bb = wf.NewAgent[hfB, hfOut](d)
	cc = wf.NewAgent[hfC, hfOut](d)
	after := wf.NewCompute[hfAfter](d, func(x wf.Ctx) (struct{}, error) {
		seen["after:a"], seen["after:b"], seen["after:c"] = wf.HarnessOf(x, a), wf.HarnessOf(x, bb), wf.HarnessOf(x, cc)
		return struct{}{}, nil
	})
	done := wf.NewTerminal[hfDone](d, wf.Success)
	gaveUp := wf.NewTerminal[hfGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	d.OnFailure(wf.Unavailable, gaveUp)
	pre.ThenTo(a, "start")
	a.Spec(wf.PromptText("a"), wf.Retry[hfA](wf.Const(retries), a.To(gaveUp, "gave up")), wf.Checkf("sees", func(_ hfOut, x wf.Ctx) error {
		seen["check:a"] = wf.HarnessOf(x, a)
		return nil
	}))
	a.ThenTo(bb, "next")
	bb.Spec(wf.PromptText("b"), wf.Retry[hfB](wf.Const(1), bb.To(gaveUp, "gave up")), wf.Checkf("sees", func(_ hfOut, x wf.Ctx) error {
		seen["check:b"], seen["check:b:a"] = wf.HarnessOf(x, bb), wf.HarnessOf(x, a)
		return nil
	}))
	bb.Dispatch(wf.OnFamily(wf.FamilyNamed("claude")), wf.ModelTier(wf.TierLight))
	bb.ThenTo(cc, "next")
	cc.Spec(wf.PromptText("c"), wf.Retry[hfC](wf.Const(1), cc.To(gaveUp, "gave up")), wf.Checkf("sees", func(_ hfOut, x wf.Ctx) error {
		seen["check:c"] = wf.HarnessOf(x, cc)
		return nil
	}))
	cc.ThenTo(after, "next")
	after.ThenTo(done, "done")
	return d
}

// harnessOfEngine is an engine for d whose run starts under codex, with a
// claude child harness that is available and always starts.
// It also returns a constructor of a new engine on the same journal.
func harnessOfEngine(t *testing.T, d *wf.Def) (*wf.Engine, wf.RunID, func() *wf.Engine) {
	t.Helper()
	opts := wf.EngineOptions{Harness: "codex", StateRoot: t.TempDir(),
		HeadlessProcess: &fakeProc{},
		HeadlessCheck: func(child, _ string, _ *wf.HeadlessConfig, _ string) wf.HarnessCheckedData {
			return wf.HarnessCheckedData{Available: true, Version: child + " 1.0"}
		}}
	h := newHarnessOpts(t, d, nil, opts)
	again := func() *wf.Engine {
		o := opts
		o.Defs, o.Clock = []*wf.Def{d}, func() time.Time { return h.now }
		return wf.NewEngine(h.j, h.reg, o).(*wf.Engine)
	}
	return h.e.(*wf.Engine), h.run(d, nil), again
}

// hfHandOne hands out one agent unit to a coordinator of harness co.
func hfHandOne(t *testing.T, e *wf.Engine, run wf.RunID, co wf.Coordinator) wf.Instruction {
	t.Helper()
	ins, err := e.HandOutAs(run, 1, co)
	if err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("%s hand-out = %+v, %v", co.Harness, ins, err)
	}
	return agentsOf(ins)[0]
}

// finish lets a child of harness h under co's session fetch and finish u.
func hfFinish(t *testing.T, e *wf.Engine, run wf.RunID, u wf.Instruction, h string, co wf.Coordinator) {
	t.Helper()
	if u.Spawn != "" {
		if _, err := e.Spawn(run, claimToken(t, u), co, nil); err != nil {
			t.Fatalf("spawn of %s: %v", u.Name, err)
		}
	}
	if _, err := e.PromptFetchAs(run, u.Name, claimToken(t, u), segChild(h, co.Session, "kid-"+u.Name)); err != nil {
		t.Fatalf("%s child of %s: %v", h, u.Name, err)
	}
	if _, err := e.Done(run, u.Name, wf.Submission{Output: json.RawMessage(`{"N":1}`)}); err != nil {
		t.Fatalf("%s child's done of %s: %v", h, u.Name, err)
	}
}

func hfWantSeen(t *testing.T, seen map[string]string, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got, ok := seen[k]; !ok || got != v {
			t.Errorf("HarnessOf read by %s = %q (read: %v), want %q", k, got, ok, v)
		}
	}
}

// HarnessOf names the coordinator's harness for its own unit, the child
// harness for a unit on another family, and "" before any accepted output.
// After a resume on another coordinator harness, the earlier outputs still
// name the harness that produced them.
func TestHarnessOfAcrossFamiliesAndAResume(t *testing.T) {
	seen := map[string]string{}
	d := harnessOfDef("harness-of", 1, seen)
	e, run, again := harnessOfEngine(t, d)
	codex := wf.Coordinator{Harness: "codex", Session: "c1"}

	hfFinish(t, e, run, hfHandOne(t, e, run, codex), "codex", codex)
	b := hfHandOne(t, e, run, codex)
	if b.Spawn == "" {
		t.Fatalf("b was not dispatched to another harness: %+v", b)
	}
	hfFinish(t, e, run, b, "claude", codex)
	hfWantSeen(t, seen, map[string]string{"pre": "", "check:a": "codex", "check:b": "claude", "check:b:a": "codex"})

	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	// The rest runs on a new engine over the same journal: what it reads
	// is folded from the records.
	e = again()
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	claude := wf.Coordinator{Harness: "claude", Session: "cl"}
	hfFinish(t, e, run, hfHandOne(t, e, run, claude), "claude", claude)
	if _, err := e.HandOutAs(run, 1, claude); err != nil {
		t.Fatal(err)
	}
	hfWantSeen(t, seen, map[string]string{"check:c": "claude", "after:a": "codex", "after:b": "claude", "after:c": "claude"})
	st, err := e.Status(run)
	if err != nil || st.Outcome != wf.Success {
		t.Fatalf("status = %+v, %v; want the run to succeed", st, err)
	}

}

// A retried attempt on another harness: the failed attempt sets nothing,
// and the accepted one, handed out after a resume on claude, names claude.
func TestHarnessOfARetryOnAnotherHarness(t *testing.T) {
	seen := map[string]string{}
	d := harnessOfDef("harness-of-retry", 2, seen)
	e, run, _ := harnessOfEngine(t, d)
	codex := wf.Coordinator{Harness: "codex", Session: "c1"}
	first := hfHandOne(t, e, run, codex)
	if _, err := e.PromptFetchAs(run, first.Name, claimToken(t, first), segChild("codex", "c1", "kid-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Failed(run, first.Name, "could not finish"); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["check:a"]; ok {
		t.Fatalf("a failed attempt ran a's check: %v", seen)
	}
	if err := e.Stop(run, "move"); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	claude := wf.Coordinator{Harness: "claude", Session: "cl"}
	retry := hfHandOne(t, e, run, claude)
	if retry.Attempt != 2 {
		t.Fatalf("the retry is attempt %d, want 2", retry.Attempt)
	}
	hfFinish(t, e, run, retry, "claude", claude)
	b := hfHandOne(t, e, run, claude)
	hfFinish(t, e, run, b, "claude", claude)
	hfWantSeen(t, seen, map[string]string{"pre": "", "check:a": "claude", "check:b:a": "claude", "check:b": "claude"})
}
