package wftest_test

import (
	"slices"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// crashPoints holds one scenario per journal record kind. Each runs a small
// definition that writes the kind, kills the engine right after the write,
// resumes, and asserts the run reaches the terminal an uncrashed run reaches.
var crashPoints = map[wf.JournalKind]func(t *testing.T){}

// readOnlyKinds are the record kinds the engine still reads from older
// journals and no longer writes, so no write of them can crash.
var readOnlyKinds = map[wf.JournalKind]bool{wf.JournalSessionStopped: true, wf.JournalSessionCleared: true}

// TestEveryKindHasCrashPoint ranges wf.JournalPayloads, the set runState.apply's
// switch covers (wf's TestEveryJournalKindHasPayload keeps the two equal), and
// fails naming each kind with no crash point.
func TestEveryKindHasCrashPoint(t *testing.T) {
	var missing []string
	for k := range wf.JournalPayloads {
		if crashPoints[k] == nil && !readOnlyKinds[k] {
			missing = append(missing, k.String())
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("record kinds with no crash point: %v", missing)
	}
	for k := range crashPoints {
		if _, ok := wf.JournalPayloads[k]; !ok {
			t.Errorf("crash point for %s, which is no journal kind", k)
		}
	}
}

func TestCrashPoints(t *testing.T) {
	for k, f := range crashPoints {
		t.Run(k.String(), f)
	}
}

// simScenario builds a Sim run: the definition, its options and the script. With
// crash false it is the uncrashed reference; with crash true the script carries
// the CrashAt step and the Resume that restarts the engine.
type simScenario func(t *testing.T, crash bool) (*wf.Def, []wftest.Option, []wftest.Step)

// simPoint runs sc crashed and uncrashed and requires the same terminal and
// retries spent, and that the crash fired at kind.
func simPoint(kind wf.JournalKind, sc simScenario) func(t *testing.T) {
	return func(t *testing.T) {
		t.Helper()
		run := func(crash bool) *wftest.Trace {
			d, opts, steps := sc(t, crash)
			return wftest.New(d, append(opts, wftest.WithTB(t))...).Run(steps...)
		}
		want, got := run(false), run(true)
		if c := got.Crashes(); len(c) == 0 || c[0] != kind {
			t.Fatalf("crash fired at %v, want %s", c, kind)
		}
		if got.Terminal() == "" {
			t.Errorf("after a crash at %s the run did not end", kind)
		}
		if got.Terminal() != want.Terminal() {
			t.Errorf("after a crash at %s the terminal is %q, want %q", kind, got.Terminal(), want.Terminal())
		}
		if got.RetriesSpent() != want.RetriesSpent() {
			t.Errorf("after a crash at %s %d retries were spent, want %d", kind, got.RetriesSpent(), want.RetriesSpent())
		}
	}
}

// before crashes the engine's next drive at kind, resumes it and runs tail.
func before(crash bool, kind wf.JournalKind, tail ...wftest.Step) []wftest.Step {
	if !crash {
		return tail
	}
	return append([]wftest.Step{wftest.CrashAt(kind), wftest.Resume()}, tail...)
}

// after runs head, crashes the engine at kind within its cycle and resumes it.
func after(crash bool, kind wf.JournalKind, head ...wftest.Step) []wftest.Step {
	if !crash {
		return head
	}
	return append(head, wftest.CrashAt(kind), wftest.Resume())
}

// loopWorld answers the loop definition's CI wait.
func loopWorld() *wftest.World {
	w := wftest.NewWorld()
	wftest.Set(w, git.Head("."), "abc123")
	return w
}

// loopScenario runs loopDef autonomously; script receives the draft agent and
// builds the steps, and approve is the human gate.
func loopScenario(auto bool, world func(*wftest.World), script func(crash bool, draft *wf.Agent[DraftS, Draft], approve *wf.Gate[ApproveS, Yes]) []wftest.Step) simScenario {
	return func(t *testing.T, crash bool) (*wf.Def, []wftest.Option, []wftest.Step) {
		d, draft, approve, _ := loopDef()
		w := loopWorld()
		if world != nil {
			world(w)
		}
		return d, []wftest.Option{wftest.WithOptions(Opts{Autonomous: auto}), wftest.WithWorld(w)}, script(crash, draft, approve)
	}
}

// killedOnce is a runner whose first launch dies with no result file, as when
// the machine goes down under the detached process; the relaunch is the
// FakeRunner's scripted Present result.
type killedOnce struct {
	*wftest.FakeRunner
	reported bool
}

func (r *killedOnce) Status(p wf.DetachedProcess, req wf.DetachedRequest) (wf.DetachedReport, error) {
	if !r.reported {
		r.reported = true
		return wf.DetachedReport{Status: wf.DetachedDead}, nil
	}
	return r.FakeRunner.Status(p, req)
}

// detachedScenario crashes the engine right after effect_started, with the
// detached runner killed: recovery finds no result file and an empty process
// group, so it launches the command again and the run ends as it would have.
func detachedScenario(kind wf.JournalKind) simScenario {
	return func(t *testing.T, crash bool) (*wf.Def, []wftest.Option, []wftest.Step) {
		fr := wftest.NewFakeRunner()
		fr.SetStatus(0, wf.DetachedPresent, wf.RunResult{})
		var runner wf.EffectRunner = fr
		if crash {
			runner = &killedOnce{FakeRunner: fr}
			t.Cleanup(func() {
				if n := len(fr.Started()); n != 2 {
					t.Errorf("the command was launched %d times, want 2: the killed runner's command runs again", n)
				}
			})
		}
		return detachedDef(time.Hour), []wftest.Option{wftest.WithEffectRunner(runner)},
			before(crash, kind, wftest.Resume(), wftest.Resume())
	}
}

func init() {
	pass := Draft{Verdict: Pass}
	for _, k := range []wf.JournalKind{wf.JournalRunStarted, wf.JournalInstructionIssued, wf.JournalHandedOut, wf.JournalAttemptStarted, wf.JournalPromptFetched} {
		crashPoints[k] = simPoint(k, loopScenario(true, nil, func(crash bool, draft *wf.Agent[DraftS, Draft], _ *wf.Gate[ApproveS, Yes]) []wftest.Step {
			return before(crash, k, wftest.Submit(draft, pass))
		}))
	}
	for _, k := range []wf.JournalKind{wf.JournalSubmitted, wf.JournalAccepted, wf.JournalEffectIntent, wf.JournalEffectResult, wf.JournalTransition,
		wf.JournalAutoAnswered, wf.JournalObserved, wf.JournalCompletedByObservation, wf.JournalRunEnded} {
		crashPoints[k] = simPoint(k, loopScenario(true, nil, func(crash bool, draft *wf.Agent[DraftS, Draft], _ *wf.Gate[ApproveS, Yes]) []wftest.Step {
			return after(crash, k, wftest.Submit(draft, pass))
		}))
	}
	// A refusal spends the attempt (refused, C5) and the next attempt's
	// output is accepted.
	crashPoints[wf.JournalRefused] = simPoint(wf.JournalRefused, loopScenario(true, nil, func(crash bool, draft *wf.Agent[DraftS, Draft], _ *wf.Gate[ApproveS, Yes]) []wftest.Step {
		return append(after(crash, wf.JournalRefused, wftest.Submit(draft, Draft{Verdict: Fail})), wftest.Submit(draft, pass))
	}))
	crashPoints[wf.JournalGateAnswered] = simPoint(wf.JournalGateAnswered, loopScenario(false, nil, func(crash bool, draft *wf.Agent[DraftS, Draft], approve *wf.Gate[ApproveS, Yes]) []wftest.Step {
		return append([]wftest.Step{wftest.Submit(draft, pass)}, after(crash, wf.JournalGateAnswered, wftest.Answer(approve, Yes("yes")))...)
	}))
	// The bd.create outcome is unknown after the first crash, so the run waits
	// for a manual settlement; the crash at effect_settled follows it.
	crashPoints[wf.JournalEffectSettled] = simPoint(wf.JournalEffectSettled, func(t *testing.T, crash bool) (*wf.Def, []wftest.Option, []wftest.Step) {
		d, draft, _, _ := loopDef()
		w := loopWorld()
		steps := []wftest.Step{wftest.Submit(draft, pass)}
		if crash {
			wftest.EffectUnknown(w, "bd", "create", bd.IssueKey("loop", "draft"))
			steps = append(steps, wftest.CrashAfterIntent("bd", "create"), wftest.Resume())
			steps = append(steps, after(true, wf.JournalEffectSettled, wftest.Settle(true))...)
		}
		return d, []wftest.Option{wftest.WithOptions(Opts{Autonomous: true}), wftest.WithWorld(w)}, steps
	})
	crashPoints[wf.JournalEffectStarted] = simPoint(wf.JournalEffectStarted, detachedScenario(wf.JournalEffectStarted))
}
