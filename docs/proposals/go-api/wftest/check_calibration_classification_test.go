package wftest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestCheckLegacyOnlyCorpusIsAdvisory(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("legacy")
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedDone](d, wf.Success)
		d.OnFailure(wf.Blocked, end)
		a.Spec(wf.PromptText("answer"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")))
		a.Then(a.To(end, "done"))
		return New(d)
	}
	cfg := CheckConfig{MaxRuns: 2, CalibrationConfig: CalibrationConfig{Seeds: 1, MaxSteps: 8, Corpus: []wf.CorpusEntry{{State: "corpusStart", Output: json.RawMessage(`true`)}, {State: "corpusStart", Output: json.RawMessage(`false`)}}}}
	report := Check(factory, cfg)
	if len(report.RealProblems) != 0 || len(report.Advisories) != 2 || len(report.Calibration.ContextUnknown) != 2 {
		t.Fatalf("legacy data treated as failure: %s", report)
	}
	if !strings.Contains(report.String(), "unknown calibration contexts: 2") {
		t.Fatal("missing advisory count")
	}
}

func TestCheckUnreachedCalibrationIsCoverage(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("unreached-calibration")
		wf.NewTerminal[repeatedDone](d, wf.Success)
		a := wf.NewAgent[corpusStart, bool](d)
		end := wf.NewTerminal[repeatedStop](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")))
		a.Then(a.To(end, "done"))
		return New(d)
	}
	report := check(factory, CheckConfig{MaxRuns: 1, CalibrationConfig: CalibrationConfig{Seeds: 1}}, false)
	if len(report.RealProblems) != 0 || !strings.Contains(strings.Join(report.CoverageProblems, "\n"), "calibration never reached") {
		t.Fatalf("unreached state treated as observed failure: %s", report)
	}
	if len(report.Calibration.Problems) != 1 {
		t.Fatal("standalone calibration lost strict verdict")
	}
}

func TestRecordedCalibrationKeepsObservedFailuresReal(t *testing.T) {
	for _, mode := range []string{"legacy", "version", "incomplete", "refused", "panic", "malformed-output"} {
		t.Run(mode, func(t *testing.T) {
			d := wf.New("recorded-classification")
			a := wf.NewAgent[corpusStart, bool](d)
			end := wf.NewTerminal[repeatedDone](d, wf.Success)
			check := wf.Checkf("valid", func(value bool, _ wf.Ctx) error {
				if mode == "panic" {
					panic("check failed")
				}
				if !value {
					return fmt.Errorf("not valid")
				}
				return nil
			})
			a.Spec(wf.PromptText("work"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")), check)
			a.Then(a.To(end, "done"))
			entry := wf.CorpusEntry{State: "corpusStart", Output: json.RawMessage(`false`), Context: &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "valid"}}}}
			switch mode {
			case "legacy":
				entry.Context = nil
			case "version":
				entry.Context.Version = 99
			case "incomplete":
				entry.Context.Checks[0].Incomplete = []string{"artifact read"}
			case "malformed-output":
				entry.Output = json.RawMessage(`"not a boolean"`)
			}
			report := &CalibrationReport{}
			calibrateRecording(wf.Inspect(d).States[0], entry, CalibrationConfig{}, report)
			advisory := mode == "legacy" || mode == "version" || mode == "incomplete"
			if advisory {
				if len(report.ContextUnknown) != 1 || len(report.Problems) != 0 {
					t.Fatalf("unknown context not advisory: %+v", report)
				}
			} else if len(report.RealProblems) == 0 || len(report.ContextUnknown) != 0 {
				t.Fatalf("observed failure hidden: %+v", report)
			}
		})
	}
}

func TestRecordedCalibrationDoesNotHideRefusalBeforeUnknownContext(t *testing.T) {
	d := wf.New("partial-recording")
	a := wf.NewAgent[corpusStart, bool](d)
	end := wf.NewTerminal[repeatedDone](d, wf.Success)
	reject := wf.Checkf("reject", func(bool, wf.Ctx) error { return fmt.Errorf("known-good refused") })
	unknown := wf.Checkf("late-context", func(_ bool, c wf.Ctx) error { c.Eval(wf.Const(1)); return nil })
	a.Spec(wf.PromptText("work"), wf.Retry[corpusStart](wf.Const(1), a.To(end, "stop")), reject, unknown)
	a.Then(a.To(end, "done"))
	entry := wf.CorpusEntry{State: "corpusStart", Output: json.RawMessage(`true`), Context: &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "reject"}, {Name: "late-context"}}}}
	report := &CalibrationReport{}
	calibrateRecording(wf.Inspect(d).States[0], entry, CalibrationConfig{}, report)
	if len(report.RealProblems) == 0 || len(report.ContextUnknown) != 0 {
		t.Fatalf("late unknown context hid earlier refusal: %+v", report)
	}
}
