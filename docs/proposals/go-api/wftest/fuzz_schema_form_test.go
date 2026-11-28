package wftest_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/form"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// fuzzFormGrammar is a patterned grammar an agent output carries.
type fuzzFormGrammar struct{}

func (fuzzFormGrammar) Name() string      { return "wftest.fuzz-form" }
func (fuzzFormGrammar) Pattern() string   { return "^R[1-9][0-9]*$" }
func (fuzzFormGrammar) Samples() []string { return []string{"R1"} }
func (fuzzFormGrammar) Check(s string) error {
	if len(s) < 2 || s[0] != 'R' || s[1] == '0' {
		return errors.New("not an R id")
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return errors.New("not an R id")
		}
	}
	return nil
}

func init() { form.Register[fuzzFormGrammar]() }

type fuzzFormAgent struct{}
type fuzzFormDone struct{}
type fuzzFormStop struct{}

type fuzzFormOut struct {
	ID   form.Of[fuzzFormGrammar]   `json:"id"`
	Refs []form.Of[fuzzFormGrammar] `json:"refs,omitempty"`
}

// An agent output with form.Of fields renders a schema that carries each
// grammar's example, and the fuzzer samples it as it samples any output:
// the annotation is ignored and the drawn values decode (sw-da9f).
func TestFuzzSamplesFormOutputs(t *testing.T) {
	build := func() *wf.Def {
		d := wf.New("form-output")
		a := wf.NewAgent[fuzzFormAgent, fuzzFormOut](d)
		done := wf.NewTerminal[fuzzFormDone](d, wf.Success)
		bad := wf.NewTerminal[fuzzFormStop](d, wf.Stopped)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzFormAgent](wf.Const(2), a.To(bad, "attempt budget exhausted")))
		edge := a.To(done, "accepted")
		a.Route(func(o fuzzFormOut, _ wf.Ctx) wf.Edge[fuzzFormAgent] {
			if o.ID.IsZero() {
				panic("a sampled output decoded with no id")
			}
			return edge
		}, edge)
		return d
	}
	for _, s := range wf.Inspect(build()).States {
		if s.Name != "fuzzFormAgent" {
			continue
		}
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(s.OutputSchema(), &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Properties["id"]["examples"] == nil {
			t.Fatalf("the form.Of field's schema has no examples annotation: %v", schema.Properties["id"])
		}
	}
	factory := func() *wftest.Sim { return wftest.New(build()) }
	for seed := int64(1); seed <= 16; seed++ {
		if got := wftest.Fuzz(factory, wftest.FuzzConfig{Seed: seed, MaxSteps: 8}); got.Finding != nil {
			t.Fatalf("seed %d: %s", seed, got)
		}
	}
}
