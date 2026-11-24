package wf_test

import (
	"encoding/json"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"reflect"
	"testing"
)

type examplePrior struct{}
type exampleCompute struct{}
type exampleCheck struct{}

func TestExampleContextDoesNotChangeJournalOrLaterChecks(t *testing.T) {
	d := wf.New("example-isolation")
	prior := wf.NewAgent[examplePrior, string](d)
	computed := wf.NewCompute[exampleCompute, string](d, func(wf.Ctx) (string, error) { return "live", nil })
	current := wf.NewAgent[exampleCheck, string](d)
	done, bad := twoTerminals(d)
	d.OnFailure(wf.Blocked, bad)
	prior.Spec(wf.PromptText("prior"), wf.Retry[examplePrior](wf.Const(1), prior.To(bad, "exhausted")))
	prior.Then(prior.To(computed, "compute"))
	computed.Then(computed.To(current, "check"))
	current.Spec(wf.PromptText("check"), wf.Retry[exampleCheck](wf.Const(1), current.To(bad, "exhausted")), wf.Checkf("matches", func(out string, c wf.Ctx) error {
		if wf.OutputOf(c, prior) != out || wf.ComputedOf(c, computed) != out {
			return fmt.Errorf("different")
		}
		return nil
	}))
	current.Then(current.To(done, "done"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first := h.next(run)[0]
	h.submit(run, first, `"live"`)
	in := h.next(run)[0]
	before := h.records(run)
	refusals, err := h.e.(*wf.Engine).CheckOutput(run, in.Name, json.RawMessage(`"example"`), "matches", wf.ExampleOutputOf(prior, "example"), wf.ExampleComputedOf(computed, "example"))
	if err != nil || len(refusals) != 0 {
		t.Fatalf("override refused: %v %v", refusals, err)
	}
	refusals, err = h.e.(*wf.Engine).CheckOutput(run, in.Name, json.RawMessage(`"live"`), "matches")
	if err != nil || len(refusals) != 0 {
		t.Fatalf("later check changed: %v %v", refusals, err)
	}
	if !reflect.DeepEqual(before, h.records(run)) {
		t.Fatal("example wrote journal")
	}
}
