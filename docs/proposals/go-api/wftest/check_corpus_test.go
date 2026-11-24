package wftest_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"testing"
)

type corpusChoose struct{}
type corpusReached struct{}
type corpusEnd struct{}
type corpusStop struct{}

func TestCheckUsesRecordedCorpusForSimulation(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("recorded-path")
		choose := wf.NewAgent[corpusChoose, string](d)
		reached := wf.NewAgent[corpusReached, bool](d)
		end := wf.NewTerminal[corpusEnd](d, wf.Success)
		stop := wf.NewTerminal[corpusStop](d, wf.Stopped)
		d.OnFailure(wf.Blocked, stop)
		choose.Spec(wf.PromptText("choose"), wf.Retry[corpusChoose](wf.Const(1), choose.To(stop, "exhausted")))
		yes, no := choose.To(reached, "recorded path"), choose.To(stop, "other path").Ends("not requested")
		choose.Route(func(value string, _ wf.Ctx) wf.Edge[corpusChoose] {
			if value == "only-recorded-path-7ea" {
				return yes
			}
			return no
		}, yes, no)
		reached.Spec(wf.PromptText("finish"), wf.Retry[corpusReached](wf.Const(1), reached.To(stop, "exhausted")))
		reached.Then(reached.To(end, "finished"))
		return wftest.New(d)
	}
	cfg := wftest.CheckConfig{CalibrationConfig: wftest.CalibrationConfig{Seeds: 64, MaxSteps: 12, Corpus: []wf.CorpusEntry{{State: "corpusChoose", Output: json.RawMessage(`"only-recorded-path-7ea"`)}, {State: "corpusReached", Output: json.RawMessage(`true`)}}}}
	got := wftest.Check(factory, cfg)
	if got.Coverage == nil || got.Coverage.Reached["corpusReached"] == 0 {
		t.Fatalf("recording never drove simulation: %s", got)
	}
	if cfg.Valid != nil {
		t.Fatal("Check mutated caller config")
	}
}
