// Package escalation is an escalation ladder (lane D P9, VISION E17b): a cheap
// agent tries first; when its attempts run out or its tier is unavailable the
// strong agent takes over and receives what the cheap one produced, including
// the checks that failed.
package escalation

import (
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types.
type (
	Cheap  struct{}
	Strong struct{}
	Done   struct{}
	GaveUp struct{}
)

// Status is the closed set of attempt results.
type Status string

// The statuses.
const (
	Fixed Status = "fixed"
	Stuck Status = "stuck"
)

// Attempt is either tier's output.
type Attempt struct {
	Status Status
	Patch  string
	Tests  []string // failing test names, empty when green
}

// Definition builds the ladder.
func Definition() *wf.Def {
	d := wf.New("escalation")
	statuses := wf.NewEnum(d, "status", Fixed, Stuck)

	cheap := wf.NewAgent[Cheap, Attempt](d)
	strong := wf.NewAgent[Strong, Attempt](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	gaveUp := wf.NewTerminal[GaveUp](d, wf.Stopped)

	toStrong := cheap.To(strong, "escalate to the strong tier")
	cheap.Spec(wf.PromptText("Fix the failing tests with a minimal patch."),
		wf.Retry[Cheap](wf.Const(2), toStrong).
			OnClass(wf.Refused, cheap.To(strong, "cheap tier refused")).
			OnClass(wf.Unavailable, cheap.To(strong, "cheap tier unavailable")).
			OnClass(wf.Blocked, cheap.To(strong, "blocked")),
		wf.Checkf("tests green", func(a Attempt, c wf.Ctx) error {
			if len(a.Tests) > 0 {
				return fmt.Errorf("attempt %d: failing tests %v", c.Attempt(), a.Tests)
			}
			return nil
		}))
	cheap.RouteCases(wf.Cases[Cheap, Attempt](func(a Attempt) Status { return a.Status }, statuses,
		wf.On(Fixed, cheap.To(done, "fixed")),
		wf.On(Stuck, cheap.To(strong, "cheap tier stuck")),
	))

	strong.Spec(wf.PromptText("The cheap tier failed. Its attempt and failing checks: {{out}}. Fix it properly.", wf.As("out", wf.Out(cheap))),
		wf.Retry[Strong](wf.Const(2), strong.To(gaveUp, "strong tier exhausted")).
			OnClass(wf.Blocked, strong.To(gaveUp, "blocked")))
	strong.RouteCases(wf.Cases[Strong, Attempt](func(a Attempt) Status { return a.Status }, statuses,
		wf.On(Fixed, strong.To(done, "fixed")),
		wf.On(Stuck, strong.To(gaveUp, "strong tier stuck").Ends("The strongest configured tier cannot complete the task")),
	))
	return d
}
