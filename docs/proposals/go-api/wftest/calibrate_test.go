package wftest_test

import (
	"encoding/json"
	"errors"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"strings"
	"testing"
)

type calibrationOutput struct {
	Name string
	Note string
}

func TestCalibrateReportsStaleCorpusWithoutRefusingIt(t *testing.T) {
	var cfg wftest.CalibrationConfig
	if err := json.Unmarshal([]byte(`{"Seeds":1,"Valid":{"fuzzAgent":[true]},"Corpus":[{"State":"fuzzAgent","Output":true,"PromptShapeHash":"sha256:old","Run":"source","Seq":7,"Context":{"Version":1,"Checks":[]}}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	report := wftest.Calibrate(checkFactory, cfg)
	b, _ := json.Marshal(report)
	var fields struct{ Stale []json.RawMessage }
	_ = json.Unmarshal(b, &fields)
	if len(fields.Stale) != 1 {
		t.Fatalf("changed prompt was not reported stale: %s", b)
	}
	if len(report.Problems) != 0 || report.Samples == 0 {
		t.Fatalf("stale output was not calibrated: %s", report)
	}
}

func TestCalibrateDetectsFalseRefusalAndUncheckedFields(t *testing.T) {
	for _, broken := range []bool{false, true} {
		factory := func() *wftest.Sim {
			d := wf.New("calibration")
			a := wf.NewAgent[fuzzAgent, calibrationOutput](d)
			end := wf.NewTerminal[fuzzDone](d, wf.Success)
			check := wf.Checkf("name", func(o calibrationOutput, _ wf.Ctx) error {
				if o.Name == "" || broken && o.Name == "valid" {
					return errors.New("missing name")
				}
				return nil
			}).Accepts(calibrationOutput{Name: "example"}).Refuses(calibrationOutput{}, "missing name")
			a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(2), a.To(end, "exhausted")), check)
			a.Then(a.To(end, "accepted"))
			return wftest.New(d)
		}
		got := wftest.Calibrate(factory, wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`{"Name":"valid","Note":"optional"}`)}}})
		if broken {
			if !strings.Contains(got.String(), "missing name") || len(got.Problems) == 0 {
				t.Fatalf("false refusal missed: %s", got)
			}
		} else {
			if len(got.Problems) != 0 {
				t.Fatalf("good check failed: %s", got)
			}
			if len(got.NotChecked) == 0 {
				t.Fatalf("unchecked field not reported: %s", got)
			}
		}
	}
}

func TestCalibrateRequiresBothExamples(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("missing")
		a := wf.NewAgent[fuzzAgent, bool](d)
		end := wf.NewTerminal[fuzzDone](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "exhausted")), wf.Checkf("unchecked", func(bool, wf.Ctx) error { return nil }))
		a.Then(a.To(end, "accepted"))
		return wftest.New(d)
	}
	got := wftest.Calibrate(factory, wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}})
	if !strings.Contains(got.String(), "examples") {
		t.Fatalf("missing examples not reported: %s", got)
	}
}

func TestCalibrateObservedExamplesCannotOverrideUndeclaredReads(t *testing.T) {
	for _, declared := range []bool{true, false} {
		factory := func() *wftest.Sim {
			d := wf.New("observed")
			a := wf.NewAgent[fuzzAgent, struct{}](d)
			end := wf.NewTerminal[fuzzDone](d, wf.Success)
			obs := wf.Observe[bool]("repo", "dirty", nil)
			check := wf.Checkf("clean", func(_ struct{}, c wf.Ctx) error {
				dirty, err := wf.Observed(c, obs)
				if err != nil {
					return err
				}
				if dirty {
					return errors.New("dirty checkout")
				}
				return nil
			})
			if declared {
				check = check.Observes(obs)
			}
			check = check.Accepts(struct{}{}, wf.ExampleObserved(obs, false)).Refuses(struct{}{}, "dirty checkout", wf.ExampleObserved(obs, true))
			a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "exhausted")), check)
			a.Then(a.To(end, "accepted"))
			w := wftest.NewWorld()
			wftest.Set(w, obs, false)
			return wftest.New(d, wftest.WithWorld(w))
		}
		got := wftest.Calibrate(factory, wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`{}`)}}})
		if declared && (len(got.Problems) > 0 || !strings.Contains(got.String(), "overrides")) {
			t.Fatalf("declared override: %s", got)
		}
		if !declared && !strings.Contains(got.String(), "undeclared") {
			t.Fatalf("undeclared override escaped: %s", got)
		}
	}
}

func TestCalibrateDoesNotLoseRewritePanic(t *testing.T) {
	got := wftest.Calibrate(fuzzSimpleFactory(false), wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}, Rewrites: map[string]func(json.RawMessage) []json.RawMessage{"fuzzAgent": func(json.RawMessage) []json.RawMessage { panic("broken rewrite") }}})
	if !strings.Contains(got.String(), "broken rewrite") {
		t.Fatalf("rewrite panic disappeared: %s", got)
	}
}

func TestCalibrateWritesConfiguredArtifactsBeforeChecks(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("artifact-check")
		a := wf.NewAgent[fuzzAgent, bool](d)
		artifact := wf.NewArtifact[string](a, "result.txt")
		end := wf.NewTerminal[fuzzDone](d, wf.Success)
		check := wf.Checkf("artifact", func(ok bool, c wf.Ctx) error {
			value, err := wf.Read(c, artifact)
			if err != nil {
				return err
			}
			if !ok || value != "expected" {
				return errors.New("bad artifact")
			}
			return nil
		}).Accepts(true).Refuses(false, "bad artifact")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "exhausted")), check)
		a.Then(a.To(end, "accepted"))
		return wftest.New(d)
	}
	got := wftest.Calibrate(factory, wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}, Artifacts: map[string]map[string]string{"fuzzAgent": {"result.txt": "expected"}}})
	if len(got.Problems) > 0 {
		t.Fatalf("artifact fixture absent: %s", got)
	}
}

func TestCalibrateReportsMissingSemanticRewrites(t *testing.T) {
	for _, declared := range []bool{false, true} {
		cfg := wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`true`)}}}
		if declared {
			cfg.Rewrites = map[string]func(json.RawMessage) []json.RawMessage{"fuzzAgent": func(raw json.RawMessage) []json.RawMessage { return []json.RawMessage{raw} }}
		}
		got := wftest.Calibrate(fuzzSimpleFactory(false), cfg)
		found := false
		for _, gap := range got.NotChecked {
			if strings.Contains(gap, "fuzzAgent: no rewrites declared") {
				found = true
			}
		}
		if found == declared {
			t.Fatalf("declared=%v: %s", declared, got)
		}
	}
}

func TestCalibrateTypeRejectedMutationsRemainUnchecked(t *testing.T) {
	type typedOutput struct {
		Name  string
		Count int
	}
	factory := func() *wftest.Sim {
		d := wf.New("typed-calibration")
		a := wf.NewAgent[fuzzAgent, typedOutput](d)
		end := wf.NewTerminal[fuzzDone](d, wf.Success)
		check := wf.Checkf("name", func(o typedOutput, _ wf.Ctx) error {
			if o.Name == "" {
				return errors.New("missing name")
			}
			return nil
		}).Accepts(typedOutput{Name: "valid"}).Refuses(typedOutput{}, "missing name")
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "exhausted")), check)
		a.Then(a.To(end, "accepted"))
		return wftest.New(d)
	}
	got := wftest.Calibrate(factory, wftest.CalibrationConfig{Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`{"Name":"valid","Count":3}`)}}})
	if len(got.Problems) > 0 {
		t.Fatalf("unexpected defect: %s", got)
	}
	if len(got.TypeRejected) == 0 {
		t.Fatalf("decode-only mutations hidden: %s", got)
	}
	for _, rejected := range got.TypeRejected {
		found := false
		for _, unchecked := range got.NotChecked {
			if strings.Contains(rejected, unchecked) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("type rejection counted as checked: %q; unchecked=%v", rejected, got.NotChecked)
		}
	}
	if !strings.Contains(got.String(), "type rejected:") {
		t.Fatalf("type diagnostics not printed: %s", got)
	}
}
