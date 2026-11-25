// Package tournament is a seeded judge panel (lane D P7, VISION E11): a Map of
// judges scores the candidates and the join is a quorum, so one lost judge does
// not decide the outcome. The seed is a run Param passed to every judge as a
// prompt input, which makes the panel reproducible.
package tournament

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types (parent, then child).
type (
	Judges    struct{}
	Decided   struct{}
	NoQuorum  struct{}
	Judge     struct{}
	JudgeDone struct{}
	JudgeLost struct{}
)

// Vote is one judge's typed result.
type Vote struct {
	Candidate string
	Score     int
}

// Ballot is the judge agent's output.
type Ballot struct {
	Candidate string
	Score     int
}

func judgeChild() wf.Child[bd.Issue, Vote] {
	d := wf.New("tournament-judge")
	// The child declares the same seed Param by name; the executor binds both
	// from the one run parameter.
	seed := d.Param("seed")
	j := wf.NewAgent[Judge, Ballot](d)
	ok := wf.NewTerminal[JudgeDone](d, wf.Success)
	lost := wf.NewTerminal[JudgeLost](d, wf.Stopped)
	d.OnFailure(wf.Blocked, lost)
	j.Spec(wf.PromptText("Judge candidate {{item}} with sampling seed {{seed}}.", wf.ChildInput(), seed),
		wf.Retry[Judge](wf.Const(2), j.To(lost, "judge produced no ballot")))
	jNext := j.To(ok, "ballot cast")
	j.Route(func(Ballot, wf.Ctx) wf.Edge[Judge] { return jNext }, jNext)
	wf.Yield(ok, func(wf.Ctx) Vote { return Vote{} })
	wf.Yield(lost, func(wf.Ctx) Vote { return Vote{} })
	return wf.Typed[bd.Issue, Vote](d)
}

// Definition builds the panel.
func Definition() *wf.Def {
	d := wf.New("tournament")
	d.RunKey(d.Param("seed"))
	judges := wf.NewMap[Judges, bd.Issue, string, Vote](d,
		wf.FromObservation(bd.ListByLabel("candidate")),
		func(i bd.Issue) string { return i.ID },
		judgeChild())
	decided := wf.NewTerminal[Decided](d, wf.Success)
	noQuorum := wf.NewTerminal[NoQuorum](d, wf.Stopped)

	// Parallel(3) is a cost limit, not harness capacity: at most three judges
	// run at once, so once two of them agree the quorum joins and no further
	// judge is ever spawned.
	judges.Parallel(3).Join(wf.Quorum(2),
		judges.To(decided, "two of three judges voted"),
		judges.To(noQuorum, "quorum not reached").Ends("Insufficient judge results to choose a winner"))
	return d
}
