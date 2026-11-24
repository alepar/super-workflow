package wftest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// recordingTB captures Errorf instead of failing the test, so a test can assert
// on the message a script mismatch produces.
type recordingTB struct {
	testing.TB
	msgs []string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func TestMismatchFailsWithBoth(t *testing.T) {
	d, draft, _, _ := loopDef()
	rec := &recordingTB{TB: t}
	// The engine issues DraftS first; the script asks for a state that is not open.
	tr := wftest.New(d, wftest.WithTB(rec)).Run(wftest.SubmitAt("CheckS", Draft{Verdict: Pass}), wftest.Submit(draft, Draft{Verdict: Pass}))
	if len(rec.msgs) != 1 {
		t.Fatalf("reported %d failures %q, want exactly 1", len(rec.msgs), rec.msgs)
	}
	for _, want := range []string{`SubmitAt "CheckS"`, `agent "DraftS"`} {
		if !strings.Contains(rec.msgs[0], want) {
			t.Errorf("message %q lacks %q", rec.msgs[0], want)
		}
	}
	// The run stopped at the mismatch: the second step never ran.
	// The wave Next hands the issued unit out and claims it (three records).
	wftest.AssertJournal(t, tr, wf.JournalRunStarted, wf.JournalInstructionIssued, wf.JournalHandedOut, wf.JournalAttemptStarted, wf.JournalPromptFetched)
}

func TestMismatchWithoutTBPanics(t *testing.T) {
	d, _, _, _ := loopDef()
	defer func() {
		v := recover()
		if s, _ := v.(string); !strings.Contains(s, "expected") || !strings.Contains(s, `"DraftS"`) {
			t.Fatalf("panic = %v, want a message showing the expected step and the issued instruction", v)
		}
	}()
	wftest.New(d).Run(wftest.SubmitAt("Nope", Draft{}))
}

func TestCrashAfterIntentThenResume(t *testing.T) {
	d, draft, _, _ := loopDef()
	key := bd.IssueKey("loop", "draft")
	w := wftest.NewWorld()
	wftest.EffectDone(w, "bd", "create", key)
	tr := wftest.New(d, wftest.WithWorld(w)).Run(
		wftest.Submit(draft, Draft{Verdict: Pass}),
		wftest.CrashAfterIntent("bd", "create"),
		wftest.Resume(),
	)
	// The check found the effect done, so the engine journaled a "done" result
	// instead of applying it again, and the run went on to the gate.
	intents, results := 0, 0
	for _, r := range tr.Journal() {
		switch r.Kind {
		case wf.JournalEffectIntent:
			if strings.Contains(string(r.Data), `"create"`) {
				intents++
			}
		case wf.JournalEffectResult:
			if strings.Contains(string(r.Data), `"done"`) {
				results++
			}
		}
	}
	if intents != 1 || results != 1 {
		t.Errorf("bd.create intents = %d, done results = %d, want 1 and 1", intents, results)
	}
	for _, a := range wftest.Applied(w) {
		if a.Adapter == "bd" {
			t.Errorf("bd.%s was applied; the crash and the done check mean it never was", a.Op)
		}
	}
	wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS")
}

type (
	cmdS  struct{}
	cmdOK struct{}
)

// A CrashAfterIntent that follows no action step arms the engine's first
// drive: here the Command's shell.run, which has no idempotency key, so after
// Resume the engine asks for manual verification instead of running it again.
func TestCrashAtStartThenResume(t *testing.T) {
	d := wf.New("cmd-only")
	exits := wf.NewEnum(d, "exit", Exit(0))
	cmd := wf.NewCommand[cmdS](d, "scripts/run")
	done := wf.NewTerminal[cmdOK](d, wf.Success)
	cmd.RouteCases(wf.Cases(wf.ExitAs[Exit], exits, wf.On(Exit(0), cmd.To(done, "ok"))))

	w := wftest.NewWorld()
	tr := wftest.New(d, wftest.WithWorld(w)).Run(wftest.CrashAfterIntent("shell", "run"), wftest.Resume(), wftest.SettleExit(0, ""))
	wftest.AssertJournal(t, tr,
		wf.JournalRunStarted,
		wf.JournalEffectIntent,
		wf.JournalEffectSettled,
		wf.JournalAccepted,
		wf.JournalTransition,
		wf.JournalRunEnded,
	)
	if got := wftest.Applied(w); len(got) != 0 {
		t.Errorf("applied = %v, want nothing: the command crashed before it ran and was settled, not re-run", got)
	}
}

func TestSettleManualVerificationInScript(t *testing.T) {
	for _, applied := range []bool{true, false} {
		t.Run(fmt.Sprintf("applied=%v", applied), func(t *testing.T) {
			d, draft, _, _ := loopDef()
			w := wftest.NewWorld()
			wftest.EffectUnknown(w, "bd", "create", bd.IssueKey("loop", "draft"))
			tr := wftest.New(d, wftest.WithWorld(w)).Run(
				wftest.Submit(draft, Draft{Verdict: Pass}),
				wftest.CrashAfterIntent("bd", "create"),
				wftest.Resume(),
				wftest.Settle(applied),
			)
			var settled []wf.EffectSettledData
			for _, r := range tr.Journal() {
				if r.Kind == wf.JournalEffectSettled {
					var s wf.EffectSettledData
					if err := json.Unmarshal(r.Data, &s); err != nil {
						t.Fatal(err)
					}
					settled = append(settled, s)
				}
			}
			if len(settled) != 1 || settled[0].Applied != applied {
				t.Fatalf("settled = %+v, want one settlement with applied=%v", settled, applied)
			}
			if applied {
				// Applied continues the run: Draft leaves for Check and stops at the gate.
				wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS")
			} else {
				// Not applied classes the step Blocked in the settlement record itself:
				// the edge to Check is not taken, and with no Blocked route Draft
				// takes its declared exhausted edge (C3: no halt).
				if settled[0].Refused == nil || settled[0].Refused.Plan == nil {
					t.Errorf("settlement = %+v, want the Blocked decision with a route attached", settled[0])
				}
				wftest.AssertRoute(t, tr, "DraftS", "GaveUpS")
			}
		})
	}
}

func TestSettleWithoutOpenInstructionMismatches(t *testing.T) {
	d, _, _, _ := loopDef()
	rec := &recordingTB{TB: t}
	wftest.New(d, wftest.WithTB(rec)).Run(wftest.Settle(true))
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "Settle(applied=true)") || !strings.Contains(rec.msgs[0], `agent "DraftS"`) {
		t.Fatalf("messages = %q, want one showing Settle and the issued DraftS instruction", rec.msgs)
	}
}

// Child agent states are qualified "child-def/State"; a bare marker name
// addresses them by suffix, and a partial element does not.
type (
	fanS       struct{}
	fanOK      struct{}
	fanStopped struct{}
	subW       struct{}
	subOK      struct{}
)

type item struct{ N int }
type res struct{}

func fanDef() *wf.Def {
	cd := wf.New("c-item")
	w := wf.NewAgent[subW, Draft](cd)
	ok := wf.NewTerminal[subOK](cd, wf.Success)
	toOK := w.To(ok, "ok")
	w.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[subW](wf.Const(1), toOK))
	w.Route(func(Draft, wf.Ctx) wf.Edge[subW] { return toOK }, toOK)
	wf.Yield(ok, func(wf.Ctx) res { return res{} })
	child := wf.Typed[item, res](cd)

	d := wf.New("fan")
	m := wf.NewMap[fanS, item, string, res](d, wf.FromValues(item{1}, item{2}), func(i item) string { return fmt.Sprint(i.N) }, child)
	done := wf.NewTerminal[fanOK](d, wf.Success)
	stopped := wf.NewTerminal[fanStopped](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllOK(), m.To(done, "met"), m.To(stopped, "partial"))
	return d
}

func TestSubmitAtMatchesQualifiedPath(t *testing.T) {
	out := Draft{Verdict: Pass}
	tr := wftest.New(fanDef()).Run(wftest.SubmitAt("subW", out), wftest.SubmitAt("c-item/subW", out))
	wftest.AssertTerminal(t, tr, wf.NewTerminal[fanOK](wf.New("x"), wf.Success))
	var visits int
	for _, s := range tr.Route() {
		if s == "c-item/subW" {
			visits++
		}
	}
	if visits != 2 {
		t.Errorf("route = %v, want both map items to arrive at c-item/subW", tr.Route())
	}

	rec := &recordingTB{TB: t}
	wftest.New(fanDef(), wftest.WithTB(rec)).Run(wftest.SubmitAt("ubW", out))
	if len(rec.msgs) != 1 {
		t.Fatalf("messages = %q, want one mismatch: a path matches whole elements only", rec.msgs)
	}
}

func TestExitScriptsCommand(t *testing.T) {
	run := func(t *testing.T, script func(w *wftest.World)) *wftest.Trace {
		d, draft, _, _ := loopDef()
		w := wftest.NewWorld()
		script(w)
		return wftest.New(d, wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	}
	t.Run("default exit 0 reaches the gate", func(t *testing.T) {
		wftest.AssertRoute(t, run(t, func(*wftest.World) {}), "DraftS", "CheckS", "ApproveS")
	})
	t.Run("exit 1 gives up", func(t *testing.T) {
		tr := run(t, func(w *wftest.World) { wftest.Exit(w, 1, "scripts/check") })
		wftest.AssertRoute(t, tr, "DraftS", "CheckS", "GaveUpS")
	})
	t.Run("a prefix that does not match leaves exit 0", func(t *testing.T) {
		tr := run(t, func(w *wftest.World) { wftest.Exit(w, 1, "scripts/other") })
		wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS")
	})
}

func TestExitOnceFailsOnlyTheFirstMatch(t *testing.T) {
	w := wftest.NewWorld()
	wftest.ExitOnce(w, 1, "scripts/check")
	apply := func() int {
		raw, err := w.Apply("", wf.ExternalEffect{Adapter: "shell", Op: "run", Args: map[string]any{"argv": []string{"scripts/check"}}})
		if err != nil {
			t.Fatal(err)
		}
		var rr wf.RunResult
		if err := json.Unmarshal(raw, &rr); err != nil {
			t.Fatal(err)
		}
		return rr.Exit
	}
	if first, second := apply(), apply(); first != 1 || second != 0 {
		t.Errorf("exits %d then %d, want 1 then 0", first, second)
	}
}

func TestOnReadStatefulWorld(t *testing.T) {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	// The head read by the CI wait starts empty (not ready) and becomes set once
	// the world has applied the bd.create edge effect.
	created := false
	wftest.OnApply(w, func(e wf.ExternalEffect) { created = created || (e.Adapter == "bd" && e.Op == "create") })
	reads := 0
	wftest.OnRead(w, func(d wf.ObservationDesc) (json.RawMessage, bool) {
		if d.Adapter != "git" || d.Op != "head" {
			return nil, false
		}
		reads++
		if created {
			return json.RawMessage(`"abc"`), true
		}
		return json.RawMessage(`""`), true
	})
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	if !created || reads == 0 {
		t.Fatalf("created = %v, reads = %d, want the hooks to have run", created, reads)
	}
	wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS", "CIS", "DoneS")
	if got := wftest.Applied(w); len(got) < 2 || got[0].Adapter != "bd" {
		t.Errorf("applied = %v, want bd.create first, then the check command", got)
	}
}

func TestWorldWithoutValueFailsTheRead(t *testing.T) {
	// A read the world has no value for is a failed read, not a silent zero: the
	// CI wait never completes, so the run stops at it with no terminal.
	d, draft, _, _ := loopDef()
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true})).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	wftest.AssertRoute(t, tr, "DraftS", "CheckS", "ApproveS", "CIS")
	if tr.Terminal() != "" {
		t.Errorf("terminal = %q, want none", tr.Terminal())
	}
}

func TestUnavailableRead(t *testing.T) {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.Set(w, git.Head("."), "abc")
	wftest.Unavailable(w, git.Head("."))
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	for _, r := range tr.Journal() {
		if r.Kind == wf.JournalObserved {
			var o wf.ObservedData
			if err := json.Unmarshal(r.Data, &o); err != nil {
				t.Fatal(err)
			}
			if !o.Unavailable {
				t.Errorf("observed %+v, want unavailable", o)
			}
			return
		}
	}
	t.Fatal("no observed record: the wait never read git.head")
}

type (
	preCmdS  struct{}
	preAgent struct{}
	preOut   struct{ Note string }
	preDone  struct{}
)

// A CrashAfterIntent folded into a Submit is armed for the Submit's own cycle.
// The Next that issues the step's instruction runs the Command's shell.run
// first; armed before that Next, the crash fired there and the Submit was
// dropped with no failure reported.
func TestFoldedCrashArmsAfterTheStepMatches(t *testing.T) {
	d := wf.New("pre-cmd")
	exits := wf.NewEnum(d, "exit", Exit(0))
	cmd := wf.NewCommand[preCmdS](d, "scripts/run")
	a := wf.NewAgent[preAgent, preOut](d)
	done := wf.NewTerminal[preDone](d, wf.Success)
	cmd.RouteCases(wf.Cases(wf.ExitAs[Exit], exits, wf.On(Exit(0), cmd.To(a, "ran"))))
	toDone := a.To(done, "answered")
	a.Spec(wf.PromptText("answer"), wf.Retry(wf.Const(1), toDone))
	a.Route(func(preOut, wf.Ctx) wf.Edge[preAgent] { return toDone }, toDone)

	rec := &recordingTB{TB: t}
	w := wftest.NewWorld()
	tr := wftest.New(d, wftest.WithTB(rec), wftest.WithWorld(w)).Run(
		wftest.Submit(a, preOut{Note: "n"}),
		wftest.CrashAfterIntent("shell", "run"),
		wftest.Resume(),
	)
	if len(rec.msgs) != 0 {
		t.Errorf("reported %q, want nothing: the step matched and ran", rec.msgs)
	}
	wftest.AssertRoute(t, tr, "preCmdS", "preAgent", "preDone")
	if tr.Terminal() != "preDone" {
		t.Errorf("terminal = %q, want preDone: the Submit was dropped", tr.Terminal())
	}
}

// Without Cap the Sim plays waves: no heartbeat record and no paged sessions.
// Next hands the unit out and claims it under session "wave".
func TestWaveModeUnchanged(t *testing.T) {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.Set(w, git.Head("."), "abc123")
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	wftest.AssertJournal(t, tr,
		wf.JournalRunStarted, wf.JournalInstructionIssued,
		wf.JournalHandedOut, wf.JournalAttemptStarted, wf.JournalPromptFetched,
		wf.JournalSubmitted, wf.JournalAccepted,
		wf.JournalEffectIntent, wf.JournalEffectResult, wf.JournalTransition,
		wf.JournalEffectIntent, wf.JournalEffectResult, wf.JournalAccepted, wf.JournalTransition,
		wf.JournalAutoAnswered, wf.JournalTransition,
		wf.JournalObserved, wf.JournalCompletedByObservation, wf.JournalTransition, wf.JournalRunEnded)
	if got := tr.Sessions(); len(got) != 0 {
		t.Errorf("sessions = %v, want none without Cap", got)
	}
}

func TestPagedStepWithoutCapFails(t *testing.T) {
	d, _, _, _ := loopDef()
	rec := &recordingTB{TB: t}
	wftest.New(d, wftest.WithTB(rec)).Run(wftest.NewCoordinator())
	if len(rec.msgs) != 1 || !strings.Contains(rec.msgs[0], "needs Cap") {
		t.Errorf("messages = %q, want one saying the step needs Cap", rec.msgs)
	}
}
