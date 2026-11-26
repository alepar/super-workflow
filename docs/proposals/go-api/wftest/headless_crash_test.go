package wftest_test

import (
	"testing"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// liveProc is a HeadlessProcess whose children start and stay alive: the
// crash scenarios exercise the records, not process liveness.
type liveProc struct{ starts *int }

func (p liveProc) Start(_, _ []string, _, _, _, _, _ string) (int, uint64, error) {
	*p.starts++
	return 4000 + *p.starts, 1, nil
}
func (liveProc) State(wf.HeadlessStartedData) wf.HeadlessState { return wf.HeadlessRunning }
func (liveProc) Stop(wf.HeadlessStartedData) error             { return nil }
func (liveProc) Sandboxed() string                             { return "" }

// crHeadlessDef is crSimpleDef's agent on the claude harness, for a codex
// coordinator.
func crHeadlessDef() *wf.Def {
	d := wf.New("cr-headless").Version(1)
	a := wf.NewAgent[crA, crOut](d)
	done := wf.NewTerminal[crDone](d, wf.Success)
	next := a.To(done, "done")
	a.Spec(wf.PromptText("do it"), wf.Retry[crA](wf.Const(2), next))
	a.Route(func(crOut, wf.Ctx) wf.Edge[crA] { return next }, next)
	a.Dispatch(wf.OnFamily(wf.FamilyNamed("claude")), wf.ModelTier(wf.TierLight))
	return d
}

func headlessRun(t *testing.T, crash bool) *drv {
	starts := 0
	state := t.TempDir()
	return newDrv(t, crash, crHeadlessDef(), nil, func(o *wf.EngineOptions) {
		o.StateRoot = state
		o.HeadlessProcess = liveProc{&starts}
		o.HeadlessCheck = func(child, coord string, _ *wf.HeadlessConfig, _ string) wf.HarnessCheckedData {
			return wf.HarnessCheckedData{Available: true, Version: child + " 1.0"}
		}
	})
}

var crCodex = wf.Coordinator{Harness: "codex", Session: "s1"}

// finishHeadless spawns the unit's claude child and finishes it as that
// child (a claude caller under the codex coordinator's session).
func (h *drv) finishHeadless(units []wf.Instruction, spawnAt bool) {
	h.t.Helper()
	e := func() *wf.Engine { return h.e.(*wf.Engine) }
	for _, in := range units {
		claim := claimOf(h.t, in)
		spawn := func() {
			_, err := e().Spawn(h.id, claim, crCodex, nil)
			h.must(err)
		}
		if spawnAt {
			h.at(wf.JournalHeadlessStarted, func() { _, _ = e().Spawn(h.id, claim, crCodex, nil) })
			if h.crash {
				// The crash came after the record: the restarted engine
				// sees the child started and running.
				if _, err := e().Spawn(h.id, claim, crCodex, nil); err == nil {
					h.t.Fatal("a second spawn of a running child was not refused")
				}
			}
		} else {
			spawn()
		}
		_, err := e().PromptFetchAs(h.id, in.Name, claim, hn.Caller{Harness: "claude", Session: "s1", Agent: "kid-" + in.Name, Is: hn.IsChild, FromEnv: true})
		h.must(err)
		_, err = e().Done(h.id, in.Name, wf.Submission{Output: []byte(`{"OK":true}`)})
		h.must(err)
	}
}

// A crash right after a segment's preflight record: the restarted engine
// keeps the check and hands the unit out on the claude harness, and the run
// finishes as uncrashed.
func init() {
	crashPoints[wf.JournalHarnessChecked] = scenario(wf.JournalHarnessChecked, headlessRun, func(h *drv) {
		var wave []wf.Instruction
		h.at(wf.JournalHarnessChecked, func() { wave, _ = h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex) })
		if h.crash {
			var err error
			wave, err = h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex)
			h.must(err)
			if len(agentUnits(wave)) == 0 {
				// The cut call had handed it out: the same session's next
				// offers it again.
				wave, err = h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex)
				h.must(err)
			}
		}
		h.finishHeadless(agentUnits(wave), false)
	})
}

// A crash right after the spawn verb's record: the child runs, a second
// spawn of it is refused, and it finishes the run as uncrashed.
func init() {
	crashPoints[wf.JournalHeadlessStarted] = scenario(wf.JournalHeadlessStarted, headlessRun, func(h *drv) {
		wave, err := h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex)
		h.must(err)
		h.finishHeadless(agentUnits(wave), true)
	})
}

// A crash between a segment's coordinator record and its preflight: the
// next call of the segment runs the preflight it missed.
func TestMissedPreflightRunsOnTheNextCall(t *testing.T) {
	h := headlessRun(t, true)
	h.atBefore(wf.JournalHarnessChecked, func() { _, _ = h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex) })
	if _, err := h.e.(*wf.Engine).HandOutAs(h.id, 1, crCodex); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range h.records() {
		if r.Kind == wf.JournalHarnessChecked {
			n++
		}
	}
	if n != 1 {
		t.Errorf("harness_checked records: %d, want 1", n)
	}
}
