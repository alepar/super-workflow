// Package router is router / classifier dispatch (lane D P2, VISION E17a): one
// agent classifies, and a Cases table over a closed Enum sends each value down
// its own edge. The table is exhaustive, so a new Enum value without an edge is
// a lint problem rather than a silent default.
package router

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// State marker types.
type (
	Classify struct{}
	Bug      struct{}
	Feature  struct{}
	Question struct{}
	Unclear  struct{}
)

// Kind is the closed set of request kinds.
type Kind string

// The kinds.
const (
	KindBug      Kind = "bug"
	KindFeature  Kind = "feature"
	KindQuestion Kind = "question"
)

// Verdict is the classifier's output.
type Verdict struct {
	Kind   Kind
	Reason string
}

// Definition builds the router.
func Definition() *wf.Def {
	d := wf.New("router")
	kinds := wf.NewEnum(d, "kind", KindBug, KindFeature, KindQuestion)

	classify := wf.NewAgent[Classify, Verdict](d)
	bug := wf.NewTerminal[Bug](d, wf.Success)
	feature := wf.NewTerminal[Feature](d, wf.Success)
	question := wf.NewTerminal[Question](d, wf.Success)
	unclear := wf.NewTerminal[Unclear](d, wf.Stopped)
	d.OnFailure(wf.Blocked, unclear)

	classify.Spec(wf.PromptText("Classify the request as bug, feature or question."),
		wf.Retry[Classify](wf.Const(2), classify.To(unclear, "no usable classification")))
	classify.RouteCases(wf.Cases[Classify, Verdict](func(v Verdict) Kind { return v.Kind }, kinds,
		wf.On(KindBug, classify.To(bug, "bug")),
		wf.On(KindFeature, classify.To(feature, "feature")),
		wf.On(KindQuestion, classify.To(question, "question")),
	).Otherwise(classify.To(unclear, "unrecognised kind").Ends("An unknown request kind has no supported handler")))
	return d
}
