package wftest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type contextPrior struct{}
type contextComputed struct{}
type contextCheck struct{}
type contextDone struct{}
type contextStopped struct{}
type contextValue struct{ Head string }

func TestCalibrationPriorResultExamples(t *testing.T) {
	for _, mode := range []string{"valid", "unused", "foreign", "duplicate", "wrong-type"} {
		t.Run(mode, func(t *testing.T) {
			factory := func() *wftest.Sim {
				d := wf.New("prior-context")
				prior := wf.NewAgent[contextPrior, contextValue](d)
				computed := wf.NewCompute[contextComputed, contextValue](d, func(wf.Ctx) (contextValue, error) { return contextValue{"computed-live"}, nil })
				checkState := wf.NewAgent[contextCheck, bool](d)
				done := wf.NewTerminal[contextDone](d, wf.Success)
				stopped := wf.NewTerminal[contextStopped](d, wf.Stopped)
				d.OnFailure(wf.Blocked, stopped)
				prior.Spec(wf.PromptText("prior"), wf.Retry[contextPrior](wf.Const(1), prior.To(stopped, "exhausted")))
				prior.Then(prior.To(computed, "compute"))
				computed.Then(computed.To(checkState, "check"))
				check := wf.Checkf("matching", func(ok bool, c wf.Ctx) error {
					if mode == "unused" {
						return nil
					}
					a, b := wf.OutputOf(c, prior), wf.ComputedOf(c, computed)
					if !ok || a.Head == "reject" || b.Head == "reject" {
						return fmt.Errorf("bad context")
					}
					return nil
				})
				target := prior
				if mode == "foreign" {
					target = wf.NewAgent[contextPrior, contextValue](wf.New("foreign"))
				}
				positive := wf.ExampleOutputOf(target, contextValue{"example"})
				if mode == "wrong-type" {
					positive.Value = json.RawMessage(`42`)
				}
				overrides := []wf.ExampleRead{positive, wf.ExampleComputedOf(computed, contextValue{"computed-example"})}
				if mode == "duplicate" {
					overrides = append(overrides, positive)
				}
				check = check.Accepts(true, overrides...).Refuses(true, "bad context", wf.ExampleOutputOf(prior, contextValue{"reject"}), wf.ExampleComputedOf(computed, contextValue{"computed-example"}))
				checkState.Spec(wf.PromptText("check"), wf.Retry[contextCheck](wf.Const(1), checkState.To(stopped, "exhausted")), check)
				checkState.Then(checkState.To(done, "accepted"))
				return wftest.New(d)
			}
			report := wftest.Calibrate(factory, wftest.CalibrationConfig{Seeds: 1, MaxSteps: 12, Valid: map[string][]json.RawMessage{"contextPrior": {json.RawMessage(`{"Head":"live"}`)}, "contextCheck": {json.RawMessage(`true`)}}})
			if mode == "valid" {
				if len(report.Problems) != 0 {
					t.Fatal(report)
				}
				for _, want := range []string{"OutputOf(contextPrior)", "ComputedOf(contextComputed)", "computed-example"} {
					if !strings.Contains(report.String(), want) {
						t.Fatalf("missing override %s: %s", want, report)
					}
				}
			} else {
				want := map[string]string{"unused": "override of contextPrior not read by check matching", "foreign": "not in definition", "duplicate": "duplicate example", "wrong-type": "example value"}[mode]
				if !strings.Contains(report.String(), want) {
					t.Fatalf("missing %q: %s", want, report)
				}
			}
		})
	}
}
