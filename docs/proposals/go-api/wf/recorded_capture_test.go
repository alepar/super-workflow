package wf_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"strings"
	"testing"
)

type captureSeed struct{}
type captureReview struct{}
type captureDone struct{}

func TestEngineCapturesActualCheckReads(t *testing.T) {
	d := wf.New("capture")
	p := d.Param("ticket")
	count := d.Counter("reviews")
	obs := wf.Observe[bool]("x", "y", map[string]any{"ticket": "{ticket}"})
	rt := newFakeRuntime()
	rt.set("x", "y", map[string]any{"ticket": "T-42"}, true)
	seed := wf.NewCompute[captureSeed, string](d, func(wf.Ctx) (string, error) { return strings.Repeat("payload", 1000), nil })
	a := wf.NewAgent[captureReview, bool](d)
	terminal := wf.NewTerminal[captureDone](d, wf.Success)
	calls := 0
	a.Spec(wf.PromptText("review"), wf.Retry[captureReview](wf.Const(1), a.To(terminal, "stop")), wf.Checkf("context", func(_ bool, c wf.Ctx) error {
		calls++
		if len(wf.ComputedOf(c, seed)) != 7000 || c.Param(p) != "T-42" || c.Attempt() != 1 || c.Count(count) != 1 || wf.InputOf[string](c) != "" {
			t.Fatal("live context changed")
		}
		_ = wf.ComputedOf(c, seed)
		value, err := wf.Observed(c, obs)
		if err != nil || !value {
			t.Fatalf("observation changed: %v %v", value, err)
		}
		return nil
	}).Observes(obs))
	first := seed.To(a, "review").With(count.Inc())
	seed.Route(func(string, wf.Ctx) wf.Edge[captureSeed] { return first }, first)
	last := a.To(terminal, "done")
	a.Route(func(bool, wf.Ctx) wf.Edge[captureReview] { return last }, last)
	h := newHarness(t, d, rt)
	run := h.run(d, map[string]string{"ticket": "T-42"})
	ins := h.next(run)
	if len(ins) != 1 {
		t.Fatalf("instructions: %+v", ins)
	}
	if result := h.submit(run, ins[0], `true`); result.Class != wf.Accepted {
		t.Fatalf("submit: %+v", result)
	}
	if calls != 1 {
		t.Fatalf("checks called %d times", calls)
	}
	accepted := h.kind(run, wf.JournalAccepted)
	var data wf.AcceptedData
	_ = json.Unmarshal(accepted[len(accepted)-1].Data, &data)
	if data.Context == nil || len(data.Context.Checks) != 1 || len(data.Context.Checks[0].Reads) != 6 {
		t.Fatalf("context: %+v", data.Context)
	}
	read := data.Context.Checks[0].Reads[0]
	if read.Seq == 0 || len(read.Value) != 0 {
		t.Fatalf("copied producer value: %+v", read)
	}
	entries, err := wf.Harvest(h.records(run), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Context.Checks[0].Reads[0].Value) != 7002 {
		t.Fatalf("harvest: %+v", entries)
	}
	replayCapturedEntry(t, d, entries[0])
}

func replayCapturedEntry(t *testing.T, d *wf.Def, e wf.CorpusEntry) {
	t.Helper()
	for _, state := range wf.Inspect(d).States {
		if state.Name == e.State {
			failures, err := state.CheckRecordedOutput(e.Output, e.Context)
			if err != nil || len(failures) != 0 {
				t.Fatalf("captured replay: %v %v", failures, err)
			}
			return
		}
	}
	t.Fatalf("missing state %s", e.State)
}
