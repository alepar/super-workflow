package wf_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/gh"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// timedOut is the error RunTool returns for a call it killed at its timeout.
var timedOut = fmt.Errorf("%w: %w: tool: timed out after 1s", wf.ErrUnavailable, wf.ErrTimedOut)

func effectResults(t *testing.T, h *harness, run wf.RunID) []wf.EffectResultData {
	t.Helper()
	var out []wf.EffectResultData
	for _, r := range h.kind(run, wf.JournalEffectResult) {
		var rd wf.EffectResultData
		if err := json.Unmarshal(r.Data, &rd); err != nil {
			t.Fatal(err)
		}
		out = append(out, rd)
	}
	return out
}

// A keyed effect whose call timed out may have reached the world: it is
// journaled uncertain (not unavailable), retried on the Unavailable schedule,
// and the retry checks it by its Key before applying it again.
func TestTimedOutKeyedEffectIsCheckedBeforeReapply(t *testing.T) {
	d := edgeDef(false, mergeEffect(wf.KeyOf("merge", "feat")))
	start := func(t *testing.T) (*world, *harness, wf.RunID) {
		w := newWorld()
		w.applyErr = timedOut
		h := newHarness(t, d, w)
		run := h.run(d, nil)
		h.submit(run, h.next(run)[0], `{}`)
		if rs := effectResults(t, h, run); len(rs) != 1 || rs[0].Outcome != "uncertain" {
			t.Fatalf("effect results = %+v, want one uncertain", rs)
		}
		if n := len(h.kind(run, wf.JournalBackoffScheduled)); n != 1 {
			t.Fatalf("backoffs = %d, want the Unavailable schedule's first", n)
		}
		w.applyErr = nil
		return w, h, run
	}
	t.Run("it landed: done, not applied again", func(t *testing.T) {
		w, h, run := start(t)
		w.status["merge/feat"] = wf.EffectDone
		h.now = h.now.Add(time.Hour)
		h.next(run)
		if w.appliedCount() != 1 || w.checks != 1 {
			t.Fatalf("applied = %d, checks = %d; want the first apply only, then one check", w.appliedCount(), w.checks)
		}
		if rs := effectResults(t, h, run); len(rs) != 2 || rs[1].Outcome != "done" || !rs[1].OK {
			t.Fatalf("effect results = %+v, want uncertain then done", rs)
		}
		if !hasTransitionTo(h, run, "edChk") {
			t.Fatal("the edge was not taken")
		}
	})
	t.Run("it did not land: applied again", func(t *testing.T) {
		w, h, run := start(t)
		w.status["merge/feat"] = wf.EffectNotDone
		h.now = h.now.Add(time.Hour)
		h.next(run)
		if w.appliedCount() != 2 || w.checks != 1 {
			t.Fatalf("applied = %d, checks = %d; want a check, then the re-apply", w.appliedCount(), w.checks)
		}
		if !hasTransitionTo(h, run, "edChk") {
			t.Fatal("the edge was not taken")
		}
	})
	t.Run("unknown: never applied again", func(t *testing.T) {
		w, h, run := start(t)
		h.now = h.now.Add(time.Hour)
		ins := h.next(run)
		if w.appliedCount() != 1 || len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification {
			t.Fatalf("applied = %d, instructions = %+v; want manual verification and no re-apply", w.appliedCount(), ins)
		}
		if !strings.Contains(ins[0].Prompt, "merge") {
			t.Errorf("prompt = %q", ins[0].Prompt)
		}
	})
}

// A NonIdempotent effect whose call timed out is Uncertain on its retry: it
// has no check, so it is never applied blind a second time.
func TestTimedOutNonIdempotentEffectIsNeverReappliedBlind(t *testing.T) {
	d := edgeDef(false, mergeEffect(wf.NonIdempotent("synthetic: no check")))
	w := newWorld()
	w.applyErr = timedOut
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	w.applyErr = nil
	h.now = h.now.Add(time.Hour)
	ins := h.next(run)
	if w.appliedCount() != 1 || w.checks != 0 || len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification {
		t.Fatalf("applied = %d, checks = %d, instructions = %+v; want manual verification only", w.appliedCount(), w.checks, ins)
	}
}

// failLoopDef routes a failed edge effect back to the agent, so the next
// submission takes the same edge, and the same keyed effect, again.
func failLoopDef(e wf.Effect) *wf.Def {
	d := wf.New("fail-loop")
	a := wf.NewAgent[eA, eOut](d)
	done, bad := twoTerminals(d)
	a.Spec(wf.PromptText("p"), wf.Retry[eA](wf.Const(3), a.To(bad, "gave up")).OnClass(wf.Blocked, a.To(a, "effect failed, try again")))
	next := a.To(done, "done").With(e)
	a.Then(next)
	return d
}

// A keyed effect that failed (it ran and reported an error, so it may still
// have taken effect) is checked before a later traversal applies it again.
func TestFailedKeyedEffectIsCheckedBeforeReapply(t *testing.T) {
	d := failLoopDef(mergeEffect(wf.KeyOf("merge", "feat")))
	w := newWorld()
	w.failOp["git.merge"] = true
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	if rs := effectResults(t, h, run); len(rs) != 1 || rs[0].Outcome != "failed" {
		t.Fatalf("effect results = %+v, want one failed", rs)
	}
	w.failOp["git.merge"] = false
	w.status["merge/feat"] = wf.EffectDone
	h.submit(run, h.next(run)[0], `{}`)
	if w.appliedCount() != 1 || w.checks != 1 {
		t.Fatalf("applied = %d, checks = %d; want one check instead of a second apply", w.appliedCount(), w.checks)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
}

// An unavailable effect never ran, so its retry applies without a check.
func TestUnavailableKeyedEffectIsNotChecked(t *testing.T) {
	d := edgeDef(false, mergeEffect(wf.KeyOf("merge", "feat")))
	w := newWorld()
	w.applyErr = wf.ErrUnavailable
	h := newHarness(t, d, w)
	run := h.run(d, nil)
	h.submit(run, h.next(run)[0], `{}`)
	w.applyErr = nil
	h.now = h.now.Add(time.Hour)
	h.next(run)
	if w.appliedCount() != 2 || w.checks != 0 {
		t.Fatalf("applied = %d, checks = %d; want a plain re-apply", w.appliedCount(), w.checks)
	}
}

// resultWorld answers checks with the effect's result (wf.CheckResultRuntime).
type resultWorld struct{ *world }

func (w resultWorld) CheckResultAt(ws string, _ wf.EffectContext, e wf.ExternalEffect) (wf.EffectStatus, json.RawMessage, error) {
	st, err := w.Check(ws, e)
	return st, json.RawMessage(`{"Number":7}`), err
}

// A check that finds the effect done reports what it found, and the Engine
// journals it on the done result: after a crash and after a timeout alike.
func TestDoneCheckJournalsItsResult(t *testing.T) {
	d, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))
	t.Run("crash recovery", func(t *testing.T) {
		w := newWorld()
		w.status["merge/feat"] = wf.EffectDone
		c := newCrashHarness(t, d, w, wf.JournalEffectResult)
		run := c.run(d, nil)
		c.crashAt(run, `{}`)
		c.reg = wf.NewAdapters()
		if err := c.reg.Register(resultWorld{w}); err != nil {
			t.Fatal(err)
		}
		c.resume()
		c.next(run)
		var rd wf.EffectResultData
		_ = json.Unmarshal(c.kind(run, wf.JournalEffectResult)[0].Data, &rd)
		if rd.Outcome != "done" || string(rd.Result) != `{"Number":7}` {
			t.Fatalf("effect_result = %+v, want done with the check's result", rd)
		}
	})
	t.Run("after a timeout", func(t *testing.T) {
		w := newWorld()
		w.applyErr = timedOut
		ed := edgeDef(false, mergeEffect(wf.KeyOf("merge", "feat")))
		h := newHarness(t, ed, resultWorld{w})
		run := h.run(ed, nil)
		h.submit(run, h.next(run)[0], `{}`)
		w.status["merge/feat"] = wf.EffectDone
		h.now = h.now.Add(time.Hour)
		h.next(run)
		if rs := effectResults(t, h, run); len(rs) != 2 || string(rs[1].Result) != `{"Number":7}` {
			t.Fatalf("effect results = %+v, want done with the check's result", rs)
		}
	})
}

// applyOnly is a runtime with no At forms: it cannot be told a run is
// sandboxed.
type applyOnly struct{ applied, checked int }

func (*applyOnly) Ops() []string { return []string{"remote.post"} }
func (*applyOnly) Read(string, wf.ObservationDesc) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (r *applyOnly) Apply(string, wf.ExternalEffect) (json.RawMessage, error) {
	r.applied++
	return json.RawMessage(`{}`), nil
}
func (r *applyOnly) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	r.checked++
	return wf.EffectNotDone, nil
}

// forkedRun runs d to its end as "src" with reg, rewinds it to before its
// one agent step and finishes the fork. It returns the fork and its status.
func forkedRun(t *testing.T, d *wf.Def, reg *wf.Adapters) (fork string, status string) {
	t.Helper()
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: d}}}
	e := newCLIEnv(t)
	must := func(args ...string) string {
		t.Helper()
		var o, er bytes.Buffer
		if code := fixtureCLI(cfg, args, &o, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return o.String()
	}
	finish := func(run string) {
		t.Helper()
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(must("next", run, "1", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next %s: %v", run, err)
		}
		must("dev", "started", run, ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", e.state)
		must("done", run, ins[0].Name, "--output", writeAllocated(t, ins[0].Output, `{"OK":true}`), "--state-dir", e.state)
	}
	must("run", d.Name(), "--run-id", "src", "--state-dir", e.state, "--workspace", e.ws)
	finish("src")
	fork = strings.TrimSpace(must("dev", "rewind", "src", "--to", "died", "--state-dir", e.state))
	finish(fork)
	return fork, must("status", fork, "--state-dir", e.state)
}

func sandboxDef(name string, e wf.ExternalEffect) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[sbAsk, clAnswer](d)
	done := wf.NewTerminal[sbDone](d, wf.Success)
	stop := wf.NewTerminal[sbStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[sbAsk](wf.Const(2), a.To(stop, "gave up")))
	next := a.To(done, "ok").With(e)
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[sbAsk] { return next }, next)
	return d
}

// A rewound run never performs a remote effect through a runtime that cannot
// be told it is sandboxed: the effect is refused, named, and the run stops
// Blocked. Its source applied the effect as usual.
func TestSandboxedRunRefusesRuntimeWithoutApplyAt(t *testing.T) {
	rt := &applyOnly{}
	reg := wf.NewAdapters()
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	d := sandboxDef("sandbox-refuse", wf.ExternalEffect{Adapter: "remote", Op: "post", Args: map[string]any{"body": "x"}, Key: wf.KeyOf("post")})
	_, status := forkedRun(t, d, reg)
	if rt.applied != 1 {
		t.Errorf("applied = %d, want 1: the source's only", rt.applied)
	}
	for _, want := range []string{"terminal sbStop", "remote.post refused", "sandboxed", "ApplyAt"} {
		if !strings.Contains(status, want) {
			t.Errorf("fork status lacks %q:\n%s", want, status)
		}
	}
}

// The step-1 finding of sw-luwj, as a regression: a rewound run re-ran the
// real gh issue create, because gh took no context and the sandbox flag was
// dropped. The fork now refuses it and stops Blocked; the stub gh runs once.
func TestRewoundRunNeverCreatesAGitHubIssue(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "gh")
	script := "#!/bin/sh\necho \"$1 $2\" >> '" + log + "'\nif [ \"$2\" = list ]; then echo '[]'; else echo https://example.invalid/o/r/issues/7; fi\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(gh.Runtime(gh.Config{Bin: stub})); err != nil {
		t.Fatal(err)
	}
	d := sandboxDef("gh-rewind", gh.IssueCreate(wf.KeyOf("synthetic-issue"), "o/r", "t", "b"))
	_, status := forkedRun(t, d, reg)
	b, _ := os.ReadFile(log)
	if got := strings.Count(string(b), "issue create"); got != 1 {
		t.Errorf("gh issue create ran %d times, want once (the source):\n%s", got, b)
	}
	for _, want := range []string{"terminal sbStop", "gh.issue_create refused", "cannot be sandboxed"} {
		if !strings.Contains(status, want) {
			t.Errorf("fork status lacks %q:\n%s", want, status)
		}
	}
}
