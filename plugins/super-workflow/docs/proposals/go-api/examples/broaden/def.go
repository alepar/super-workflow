// Package broaden is lane D pattern P10: confidence/threshold retry with
// broadened context. An agent answers and reports a confidence; a low
// confidence re-runs the question with a wider context as an extra prompt
// input, and a second low confidence escalates instead of looping.
package broaden

import "github.com/alepar/super-workflow/docs/proposals/go-api/wf"

// Confidence is the closed value set of the answer's confidence field.
type Confidence string

// Confidence values.
const (
	Confident Confidence = "confident"
	Unsure    Confidence = "unsure"
)

// AnswerOut is an agent's answer with its self-reported confidence.
type AnswerOut struct {
	Text       string
	Confidence Confidence
}

// Marker types, one per state.
type (
	Answer      struct{}
	AnswerWider struct{}
	Accepted    struct{}
	Escalated   struct{}
)

// Definition returns the broaden definition.
func Definition() *wf.Def {
	d := wf.New("broaden").Version(1)
	wider := d.Param("wider_context")
	levels := wf.NewEnum(d, "confidence", Confident, Unsure)

	first := wf.NewAgent[Answer, AnswerOut](d)
	second := wf.NewAgent[AnswerWider, AnswerOut](d)
	accepted := wf.NewTerminal[Accepted](d, wf.Success)
	escalated := wf.NewTerminal[Escalated](d, wf.Stopped)
	d.OnFailure(wf.Blocked, escalated)

	first.Spec(wf.PromptText("Answer the question from the narrow context and report your confidence."),
		wf.Retry(wf.Const(2), first.To(escalated, "answer dead twice: escalate")))
	first.RouteCases(wf.Cases[Answer, AnswerOut](func(o AnswerOut) Confidence { return o.Confidence }, levels,
		wf.On(Confident, first.To(accepted, "confident: accept")),
		wf.On(Unsure, first.To(second, "unsure: re-run with broadened context"))))

	second.Spec(wf.PromptText("The first answer was unsure: {{answer}}. Answer again using the wider context {{wider_context}}.",
		wf.Out(first), wider),
		wf.Retry(wf.Const(2), second.To(escalated, "answer dead twice: escalate")))
	second.RouteCases(wf.Cases[AnswerWider, AnswerOut](func(o AnswerOut) Confidence { return o.Confidence }, levels,
		wf.On(Confident, second.To(accepted, "confident: accept")),
		wf.On(Unsure, second.To(escalated, "still unsure: escalate").Ends("Broader context did not resolve uncertainty; outside help is required"))))
	return d
}
