// Package park is lane D pattern P14: park at a clean boundary. A
// policy-answerable gate is answered by the definition's AutonomyPolicy when
// the run is autonomous, and the rule records the road not taken in the run's
// parked list (Park) so a human can pick it up later, instead of the run
// stopping at the gate.
package park

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Choice is the gate's closed answer set.
type Choice string

// Gate answers.
const (
	Ship Choice = "ship"
	Hold Choice = "hold"
)

// Options is the typed run options of the definition.
type Options struct{ Autonomous bool }

// Marker types, one per state.
type (
	Release struct{}
	Shipped struct{}
	Held    struct{}
)

// Definition returns the park definition.
func Definition() *wf.Def {
	d := wf.New("park").Version(1)
	opts := wf.Options[Options](d)
	answers := wf.NewEnum(d, "choice", Ship, Hold)

	release := wf.NewGate[Release, Choice](d, "Ship the release now?", answers, wf.PolicyAnswerable)
	shipped := wf.NewTerminal[Shipped](d, wf.Success)
	held := wf.NewTerminal[Held](d, wf.Stopped)

	release.Answers(
		wf.On(Ship, release.To(shipped, "ship").Ends(wf.Success, "the release is approved for shipping")),
		wf.On(Hold, release.To(held, "hold: parked for a human").Ends(wf.Stopped, "Release remains held for later human sign-off")))
	d.SetPolicy(wf.Policy(
		wf.PolicyAnswer(release, Hold, wf.Field(opts, func(o *Options) *bool { return &o.Autonomous }).Is(true)).
			Park("release held for human sign-off; ship manually when ready")))
	return d
}
