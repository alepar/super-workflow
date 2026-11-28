// Package typedexit is lane D pattern P12 and VISION E13: a typed-exit agent
// loop with limits checked before each step. The agent's output carries an
// exit drawn from a closed Enum, each exit has its own edge, and the visit
// budget is enforced before the step runs rather than discovered after it.
// The exits are submitted, limits_exceeded, format_error_repeated, blocked and
// continue (not an exit: the step did not submit, so the loop takes the next
// step after the visit check).
package typedexit

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Exit is the closed set of ways the agent's work ends.
type Exit string

// Exit values.
const (
	Submitted           Exit = "submitted"
	LimitsExceeded      Exit = "limits_exceeded"
	FormatErrorRepeated Exit = "format_error_repeated"
	Blocked             Exit = "blocked"
	// Continue is not an exit: the step did not submit; the next step runs after
	// the visit check.
	Continue Exit = "continue"
)

// StepOut is the agent's output; Exit is the typed exit.
type StepOut struct {
	Exit    Exit
	Summary string
}

// Marker types, one per state.
type (
	Work        struct{}
	Next        struct{}
	Done        struct{}
	LimitsStop  struct{}
	FormatStop  struct{}
	BlockedStop struct{}
)

// Definition returns the typedexit definition.
func Definition() *wf.Def {
	d := wf.New("typedexit").Version(1)
	exits := wf.NewEnum(d, "exit", Submitted, LimitsExceeded, FormatErrorRepeated, Blocked, Continue)

	work := wf.NewAgent[Work, StepOut](d)
	next := wf.NewCompute[Next, struct{}](d, func(wf.Ctx) (struct{}, error) { return struct{}{}, nil })
	done := wf.NewTerminal[Done](d, wf.Success)
	limits := wf.NewTerminal[LimitsStop](d, wf.Stopped)
	format := wf.NewTerminal[FormatStop](d, wf.Stopped)
	blocked := wf.NewTerminal[BlockedStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, format)

	work.Spec(wf.PromptText("Work the task; end with one typed exit."),
		wf.Retry(wf.Const(2), work.To(format, "no valid exit twice: format error repeated")))
	// An agent state has no edge to itself (lint rejects it): the continue
	// loop goes through Next, which hands control straight back to Work.
	back := next.To(work, "back to work")
	next.Route(func(struct{}, wf.Ctx) wf.Edge[Next] { return back }, back)
	work.RouteCases(wf.Cases[Work, StepOut](func(o StepOut) Exit { return o.Exit }, exits,
		wf.On(Submitted, work.To(done, "submitted")),
		wf.On(LimitsExceeded, work.To(limits, "limits exceeded").Ends("The agent reported its execution limit reached")),
		wf.On(FormatErrorRepeated, work.To(format, "format error repeated").Ends("The agent reported repeated output format failures")),
		wf.On(Blocked, work.To(blocked, "blocked").Ends("The agent reported a blocker requiring outside intervention")),
		wf.On(Continue, work.To(next, "next step"))))
	// Checked before each visit of Work: arrivals 1..8 run a step; the ninth
	// arrival (the eighth step answering "continue") takes this edge without
	// asking the agent again (at most 8 steps per run).
	wf.MaxVisits(wf.Const(8), work.To(limits, "visit cap reached before the step"))
	return d
}
