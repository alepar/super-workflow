// Package critique is the evaluator-optimizer loop with a termination guard
// (lane D P5, VISION E9): a drafter and a critic alternate, the loop is capped
// by MaxVisits, a stall guard stops a drafter that repeats itself, and every
// guard falls back to a human gate instead of looping.
package critique

import (
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types.
type (
	Draft    struct{}
	Critique struct{}
	Human    struct{}
	Accepted struct{}
	Dropped  struct{}
)

// Verdict is the critic's closed outcome set.
type Verdict string

// The verdicts.
const (
	Approve Verdict = "approve"
	Revise  Verdict = "revise"
)

// Decision is the human gate's answer set.
type Decision string

// The decisions.
const (
	Accept Decision = "accept"
	Drop   Decision = "drop"
)

// Text is the drafter's output.
type Text struct{ Body string }

// Review is the critic's output.
type Review struct {
	Verdict Verdict
	Notes   string
}

// Definition builds the loop.
func Definition() *wf.Def {
	d := wf.New("critique")
	rounds := d.Counter("rounds")
	verdicts := wf.NewEnum(d, "verdict", Approve, Revise)
	decisions := wf.NewEnum(d, "decision", Accept, Drop)

	draft := wf.NewAgent[Draft, Text](d)
	critique := wf.NewAgent[Critique, Review](d)
	human := wf.NewGate[Human, Decision](d, "The critic and drafter did not converge; accept the draft?", decisions, wf.Reserved)
	accepted := wf.NewTerminal[Accepted](d, wf.Success)
	dropped := wf.NewTerminal[Dropped](d, wf.Stopped)

	// Each critic round buys the drafter one more attempt; two identical
	// attempts in a row (below the attempt budget) stop it early.
	toHuman := draft.To(human, "drafter stalled or exhausted")
	draft.Spec(wf.PromptText("Draft or revise the text. Critic notes so far: {{out}}", wf.As("out", wf.Out(critique))),
		wf.Retry[Draft](wf.Const(3).Plus(wf.Count(rounds)), toHuman).NoProgressAfter(2).OnStalled(toHuman).
			OnClass(wf.Blocked, draft.To(human, "blocked")))
	draftNext := draft.To(critique, "drafted")
	draft.Route(func(Text, wf.Ctx) wf.Edge[Draft] { return draftNext }, draftNext)

	critique.Spec(wf.PromptText("Review the draft: {{out}}", wf.As("out", wf.Out(draft))),
		wf.Retry[Critique](wf.Const(2), critique.To(human, "critic unusable")).
			OnClass(wf.Blocked, critique.To(human, "blocked")))
	critique.RouteCases(wf.Cases[Critique, Review](func(r Review) Verdict { return r.Verdict }, verdicts,
		wf.On(Approve, critique.To(accepted, "approved")),
		wf.On(Revise, critique.To(draft, "revise").With(rounds.Inc())),
	))
	// Arrivals 1..5 are the five critic rounds; the fifth revise sends a sixth
	// draft, which takes this edge to the human gate unreviewed.
	wf.MaxVisits(wf.Const(5), critique.To(human, "round cap reached"))

	human.Answers(
		wf.On(Accept, human.To(accepted, "human accepts").Ends(wf.Success, "the person accepts the draft")),
		wf.On(Drop, human.To(dropped, "human drops").Ends(wf.Stopped, "the person drops the draft")),
	).Timeout(24*time.Hour, human.To(dropped, "no human answer"))
	return d
}
