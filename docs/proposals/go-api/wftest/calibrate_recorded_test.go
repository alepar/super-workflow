package wftest_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestCalibrateMixedRecordingsUseTheirOwnContexts(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("mixed-recordings")
		p := d.Param("expected")
		a := wf.NewAgent[fuzzAgent, string](d)
		end := wf.NewTerminal[fuzzDone](d, wf.Success)
		a.Spec(wf.PromptText("answer"), wf.Retry[fuzzAgent](wf.Const(1), a.To(end, "stop")), wf.Checkf("matches", func(out string, c wf.Ctx) error {
			if out != c.Param(p) {
				return errors.New("different run")
			}
			return nil
		}).Accepts("live").Refuses("wrong", "different run"))
		a.Then(a.To(end, "done"))
		return wftest.New(d, wftest.WithParams(map[string]string{"expected": "live"}))
	}
	cfg := wftest.CalibrationConfig{Seeds: 1, Valid: map[string][]json.RawMessage{"fuzzAgent": {json.RawMessage(`"live"`)}}}
	for _, value := range []string{"one", "two"} {
		raw, _ := json.Marshal(value)
		cfg.Corpus = append(cfg.Corpus, wf.CorpusEntry{State: "fuzzAgent", Output: raw, Context: &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "matches", Reads: []wf.RecordedRead{{Kind: "param", Name: "expected", Value: raw}}}}}})
	}
	got := wftest.Calibrate(factory, cfg)
	if len(got.Problems) != 0 {
		t.Fatalf("mixed contexts borrowed live state: %s", got)
	}
	cfg.Corpus[0].Context = nil
	got = wftest.Calibrate(factory, cfg)
	if len(got.Problems) != 0 || len(got.ContextUnknown) != 1 || !strings.Contains(got.String(), "context unknown") {
		t.Fatalf("legacy context silently calibrated: %s", got)
	}
}
