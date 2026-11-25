// Package gatedchain is the gated prompt chain (lane D P1, VISION E1): an
// agent drafts, a deterministic gate validates the draft, and only then does
// the next agent run. A failed gate stops the run with a named reason.
package gatedchain

import (
	"fmt"

	"github.com/alepar/super-workflow/docs/proposals/go-api/fsx"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types; the state names derive from them.
type (
	Draft          struct{}
	Vet            struct{}
	Expand         struct{}
	SchemaRejected struct{}
	VetRejected    struct{}
	Done           struct{}
)

// Outline is the draft agent's output: where the schema-checked file is.
type Outline struct{ Path string }

// Expansion is the second agent's output.
type Expansion struct{ Path string }

// VetExit is the validator's exit-code Enum.
type VetExit int

const (
	vetOK     VetExit = 0
	vetFailed VetExit = 1
)

const outlinePath = "{run}/outline.json"

var outlineSchema = fsx.Schema{Name: "outline", Path: "schemas/outline.schema.json"}

// Definition builds the chain. wf.Main registration lives in cmd/workflow.
func Definition() *wf.Def {
	d := wf.New("gatedchain").Version(1)
	topic := d.Param("topic")
	d.RunKey(topic)
	vetExits := wf.NewEnum(d, "vet-exit", vetOK, vetFailed)

	draft := wf.NewAgent[Draft, Outline](d)
	vet := wf.NewCommand[Vet](d, "scripts/vet-outline", outlinePath)
	expand := wf.NewAgent[Expand, Expansion](d)
	done := wf.NewTerminal[Done](d, wf.Success)
	schemaBad := wf.NewTerminal[SchemaRejected](d, wf.Stopped)
	vetBad := wf.NewTerminal[VetRejected](d, wf.Stopped)

	// The schema is checked by the executor (an Observation), not by a closure
	// that shells out; the check only reads the declared result.
	draft.Spec(wf.PromptFile("prompts/draft-outline.md", topic),
		wf.Retry[Draft](wf.Const(2), draft.To(schemaBad, "outline fails schema twice")).
			OnClass(wf.Blocked, draft.To(schemaBad, "blocked")),
		wf.Checkf("outline matches schema", func(o Outline, c wf.Ctx) error {
			r, err := wf.Observed(c, fsx.SchemaCheck(o.Path, outlineSchema))
			if err != nil {
				return err
			}
			if !r.OK {
				return fmt.Errorf("outline schema: %v", r.Errors)
			}
			return nil
		}).Observes(fsx.SchemaCheck(outlinePath, outlineSchema)))
	draftNext := draft.To(vet, "outline accepted")
	draft.Route(func(Outline, wf.Ctx) wf.Edge[Draft] { return draftNext }, draftNext)

	vet.RouteCases(wf.Cases[Vet, wf.RunResult](wf.ExitAs[VetExit], vetExits,
		wf.On(vetOK, vet.To(expand, "vet passed")),
		wf.On(vetFailed, vet.To(vetBad, "vet failed").Ends("A rejected outline must not reach the expansion stage")),
	))

	expand.Spec(wf.PromptText("Expand the vetted outline into the full text: {{out}}", wf.As("out", wf.Out(draft))),
		wf.Retry[Expand](wf.Const(2), expand.To(vetBad, "expansion not accepted")).
			OnClass(wf.Blocked, expand.To(vetBad, "blocked")))
	expandNext := expand.To(done, "expanded")
	expand.Route(func(Expansion, wf.Ctx) wf.Edge[Expand] { return expandNext }, expandNext)
	return d
}
