package wftest_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type (
	handoffOpts struct{ Handoff bool }
	handoffStop struct{}
)

// handoffFactory is draft -> audit -> done, where the Handoff option sends
// the audited draft to a Stopped terminal for a person to pick up instead.
// marked declares that hand-off with Ends(reason).
func handoffFactory(marked bool, with ...wftest.Option) func() *wftest.Sim {
	return func() *wftest.Sim {
		d := wf.New("handoff")
		opts := wf.Options[handoffOpts](d)
		handoff := wf.Field(opts, func(o *handoffOpts) *bool { return &o.Handoff }).Is(true)
		draft := wf.NewAgent[variantDraft, bool](d)
		audit := wf.NewAgent[variantAudit, bool](d)
		done := wf.NewTerminal[variantDone](d, wf.Success)
		pending := wf.NewTerminal[handoffStop](d, wf.Stopped)
		stop := wf.NewGate[fuzzStop, fuzzVerdict](d, "continue?", wf.NewEnum(d, "answer", fuzzVerdict("yes")), wf.Reserved)
		stop.Answers(wf.On(fuzzVerdict("yes"), stop.To(done, "approved").Ends(wf.Success, "the person approves completion")))
		d.OnFailure(wf.Blocked, stop)
		draft.Spec(wf.PromptText("draft"), wf.Retry[variantDraft](wf.Const(1), draft.To(stop, "exhausted")))
		audit.Spec(wf.PromptText("audit"), wf.Retry[variantAudit](wf.Const(1), audit.To(stop, "exhausted")))
		draft.Then(draft.To(audit, "drafted"))
		toPerson := audit.To(pending, "audited: hand-off pending").When(handoff)
		if marked {
			toPerson = toPerson.Ends("a person decides before the run goes on")
		}
		_ = toPerson
		audit.Then(audit.To(done, "audited"))
		return wftest.New(d, with...)
	}
}

// An options-guarded hand-off declared with Ends is a sanctioned end for an
// all-good run under the variant that opens it (sw-l6xo).
func TestCheckLivenessHonoursEndsDeclaredStopUnderVariant(t *testing.T) {
	got := wftest.Check(handoffFactory(true), variantConfig(map[string]any{"auto": handoffOpts{}, "handoff": handoffOpts{Handoff: true}}))
	if len(got.Problems) != 0 {
		t.Fatalf("Ends-declared hand-off reported: %v", got.Problems)
	}
	if got.Coverage.Ends["stopped"] == 0 {
		t.Fatalf("no seed reached the hand-off: %v", got.Coverage.Ends)
	}
}

// The same all-good stop without the declaration is still a liveness
// finding. Check reports it as stop-unmarked lint before simulating, so the
// run is fuzzed directly under the hand-off options.
func TestLivenessFindsUndeclaredStopUnderOptions(t *testing.T) {
	for _, marked := range []bool{true, false} {
		factory := handoffFactory(marked, wftest.WithOptions(handoffOpts{Handoff: true}))
		corpus := variantConfig(nil).Progress
		got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, Liveness: true, Valid: corpus, Progress: corpus})
		if got.Status.Terminal != "handoffStop" {
			t.Fatalf("marked=%v: run did not reach the hand-off: %s", marked, got)
		}
		if marked != (got.Finding == nil) || (!marked && got.Finding.Rule != "liveness") {
			t.Fatalf("marked=%v: %s", marked, got)
		}
	}
}
