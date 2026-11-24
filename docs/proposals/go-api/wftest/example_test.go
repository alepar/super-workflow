package wftest_test

import (
	"errors"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type Verdict string

const (
	Pass Verdict = "pass"
	Fail Verdict = "fail"
)

type Draft struct{ Verdict Verdict }

type Exit int

type Yes string

type Opts struct{ Autonomous bool }

// Marker types, one per state.
type (
	DraftS   struct{}
	CheckS   struct{}
	ApproveS struct{}
	CIS      struct{}
	DoneS    struct{}
	GaveUpS  struct{}
)

// loopDef is the small local definition the Examples run against: agent Draft
// (routes on its Verdict; a check rejects a failing draft, and repeated
// rejections stall into GaveUp), command Check (exit Cases), policy-answerable
// gate Approve, a Wait over git.Head, and terminals Done and GaveUp.
func loopDef() (*wf.Def, *wf.Agent[DraftS, Draft], *wf.Gate[ApproveS, Yes], *wf.Terminal[DoneS]) {
	d := wf.New("loop").Version(1)
	opts := wf.Options[Opts](d)
	verdicts := wf.NewEnum(d, "verdict", Pass, Fail)
	exits := wf.NewEnum(d, "check-exit", Exit(0), Exit(1))
	answers := wf.NewEnum(d, "answer", Yes("yes"))

	draft := wf.NewAgent[DraftS, Draft](d)
	check := wf.NewCommand[CheckS](d, "scripts/check")
	approve := wf.NewGate[ApproveS](d, "Ship it?", answers, wf.PolicyAnswerable)
	ci := wf.NewWait[CIS](d, git.Head("."), "head moved", func(h string) bool { return h != "" })
	done := wf.NewTerminal[DoneS](d, wf.Success)
	gaveUp := wf.NewTerminal[GaveUpS](d, wf.Stalled)

	toCheck := draft.To(check, "drafted").With(bd.Create(bd.IssueKey("loop", "draft"), bd.Issue{Title: "draft"}))
	giveUp := draft.To(gaveUp, "gave up")
	draft.Spec(wf.PromptText("write the draft"),
		wf.Retry(wf.Const(3), giveUp).NoProgressAfter(2).OnStalled(giveUp),
		wf.Checkf("verdict-pass", func(o Draft, _ wf.Ctx) error {
			if o.Verdict != Pass {
				return errors.New("draft verdict is fail")
			}
			return nil
		}))
	draft.RouteCases(wf.Cases(func(o Draft) Verdict { return o.Verdict }, verdicts,
		wf.On(Pass, toCheck), wf.On(Fail, giveUp)))

	check.RouteCases(wf.Cases(wf.ExitAs[Exit], exits,
		wf.On(Exit(0), check.To(approve, "ok")),
		wf.On(Exit(1), check.To(gaveUp, "fail"))))

	approve.Answers(wf.On(Yes("yes"), approve.To(ci, "yes")))
	ci.Ready(ci.To(done, "ready"))

	d.SetPolicy(wf.Policy(wf.PolicyAnswer(approve, Yes("yes"), wf.Field(opts, func(o *Opts) *bool { return &o.Autonomous }).Is(true))))
	return d, draft, approve, done
}

func Example_successPath() {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	_ = tr.Route()
}

func Example_rejectedThenAccepted() {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d).Run(
		wftest.Submit(draft, Draft{Verdict: Fail}),
		wftest.Submit(draft, Draft{Verdict: Pass}),
	)
	_ = tr.Journal()
}

func Example_noProgressStalled() {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d).Run(wftest.SubmitTimes(draft, 2, Draft{Verdict: Fail}))
	_ = tr.Terminal()
}

func Example_crashBetweenIntentAndResult() {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.EffectUnknown(w, "bd", "create", bd.IssueKey("loop", "draft"))
	tr := wftest.New(d, wftest.WithWorld(w)).Run(
		wftest.Submit(draft, Draft{Verdict: Pass}),
		wftest.CrashAfterIntent("bd", "create"),
		wftest.Resume(),
	)
	_ = tr.Route()
}

func Example_gateAnsweredByPolicy() {
	d, draft, _, _ := loopDef()
	tr := wftest.New(d, wftest.WithOptions(Opts{Autonomous: true})).Run(
		wftest.Submit(draft, Draft{Verdict: Pass}),
	)
	_ = tr.Journal()
}

func Example_staleObservation() {
	d, draft, _, _ := loopDef()
	w := wftest.NewWorld()
	wftest.Stale(w, git.Head("."))
	tr := wftest.New(d, wftest.WithWorld(w)).Run(wftest.Submit(draft, Draft{Verdict: Pass}))
	_ = tr.Route()
}
