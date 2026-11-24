package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type recordedPrior struct{}
type recordedNow struct{}
type recordedDone struct{}
type recordedPattern struct {
	Branch string `json:"branch" pattern:"[a-z]+"`
}

func TestCalibrationProbesMatchLivePatternGuard(t *testing.T) {
	d := wf.New("pattern-probe")
	a := wf.NewAgent[recordedNow, recordedPattern](d)
	end := wf.NewTerminal[recordedDone](d, wf.Success)
	a.Spec(wf.PromptText("branch"), wf.Retry[recordedNow](wf.Const(2), a.To(end, "stop")))
	a.Then(a.To(end, "done"))
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	view := wf.Inspect(d).States[0]
	context := &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{}}
	for _, raw := range []string{`{"branch":"--help"}`, `{"branch":42}`} {
		live, err := h.e.(*wf.Engine).CheckOutput(run, in.Name, json.RawMessage(raw), "")
		if err != nil || live["$decode"] == "" {
			t.Errorf("live probe accepted invalid output %s: %v %v", raw, live, err)
		}
		recorded, err := view.CheckRecordedOutput(json.RawMessage(raw), context)
		if err != nil || recorded["$decode"] == "" {
			t.Errorf("recorded probe accepted invalid output %s: %v %v", raw, recorded, err)
		}
	}
	if got := h.submit(run, in, `{"branch":"--help"}`); got.Class != wf.Refused {
		t.Fatalf("live submission accepted: %+v", got)
	}
	got, err := view.CheckRecordedOutput(json.RawMessage(`{"branch":"feature"}`), context)
	if err != nil || len(got) != 0 {
		t.Fatalf("valid output refused: %v %v", got, err)
	}
}

func recordedView(t *testing.T, skip bool) wf.StateView {
	t.Helper()
	d := wf.New("recorded")
	p := wf.NewAgent[recordedPrior, string](d)
	a := wf.NewAgent[recordedNow, string](d)
	end := wf.NewTerminal[recordedDone](d, wf.Success)
	p.Spec(wf.PromptText("prior"), wf.Retry[recordedPrior](wf.Const(1), p.To(end, "stop")))
	p.Then(p.To(a, "next"))
	a.Spec(wf.PromptText("current"), wf.Retry[recordedNow](wf.Const(1), a.To(end, "stop")), wf.Checkf("same", func(out string, c wf.Ctx) error {
		if !skip && out != wf.OutputOf(c, p) {
			return errors.New("different")
		}
		return nil
	}))
	a.Then(a.To(end, "done"))
	return wf.Inspect(d).States[1]
}

func TestRecordedProbeUsesOnlyItsOwnConsumedContext(t *testing.T) {
	view := recordedView(t, false)
	for _, value := range []string{"first run", "second run"} {
		raw, _ := json.Marshal(value)
		ctx := &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "same", Reads: []wf.RecordedRead{{Kind: "output", Def: "recorded", State: "recordedPrior", Value: raw}}}}}
		got, err := view.CheckRecordedOutput(raw, ctx)
		if err != nil || len(got) != 0 {
			t.Fatalf("own context refused: %v %v", got, err)
		}
		got, err = view.CheckRecordedOutput(json.RawMessage(`"wrong"`), ctx)
		if err != nil || got["same"] != "different" {
			t.Fatalf("mismatch not refused: %v %v", got, err)
		}
	}
}

func TestRecordedProbeFailsClosed(t *testing.T) {
	valid := func() *wf.CheckContexts {
		return &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "same", Reads: []wf.RecordedRead{{Kind: "output", Def: "recorded", State: "recordedPrior", Value: json.RawMessage(`"ok"`)}}}}}
	}
	for _, tc := range []struct {
		name, want string
		skip       bool
		change     func(*wf.CheckContexts) *wf.CheckContexts
	}{
		{"legacy", "context unknown", false, func(*wf.CheckContexts) *wf.CheckContexts { return nil }},
		{"version", "context unknown", false, func(c *wf.CheckContexts) *wf.CheckContexts { c.Version = 99; return c }},
		{"identity", "context unknown", false, func(c *wf.CheckContexts) *wf.CheckContexts { c.Checks[0].Name = "other"; return c }},
		{"incomplete", "context unknown", false, func(c *wf.CheckContexts) *wf.CheckContexts {
			c.Checks[0].Incomplete = []string{"artifact read"}
			return c
		}},
		{"missing", "not recorded", false, func(c *wf.CheckContexts) *wf.CheckContexts { c.Checks[0].Reads = nil; return c }},
		{"unused", "not read", true, func(c *wf.CheckContexts) *wf.CheckContexts { return c }},
		{"wrong-type", "invalid", false, func(c *wf.CheckContexts) *wf.CheckContexts {
			c.Checks[0].Reads[0].Value = json.RawMessage(`42`)
			return c
		}},
		{"foreign", "definition", false, func(c *wf.CheckContexts) *wf.CheckContexts { c.Checks[0].Reads[0].Def = "other"; return c }},
		{"duplicate", "duplicate", false, func(c *wf.CheckContexts) *wf.CheckContexts {
			c.Checks[0].Reads = append(c.Checks[0].Reads, c.Checks[0].Reads[0])
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := recordedView(t, tc.skip).CheckRecordedOutput(json.RawMessage(`"ok"`), tc.change(valid()))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRecordedProbePreservesObservationBindingAndError(t *testing.T) {
	d := wf.New("observed-recording")
	p := d.Param("ticket")
	a := wf.NewAgent[recordedNow, bool](d)
	end := wf.NewTerminal[recordedDone](d, wf.Success)
	declared := wf.Observe[bool]("service", "read", map[string]any{"ticket": "{ticket}"})
	requested := wf.Observe[bool]("service", "read", map[string]any{"ticket": "42"})
	wantError := "adapter unavailable: fixture offline"
	a.Spec(wf.PromptText("observe"), wf.Retry[recordedNow](wf.Const(1), a.To(end, "stop")), wf.Checkf("optional", func(_ bool, c wf.Ctx) error {
		_, err := wf.Observed(c, wf.Observe[bool]("service", "read", map[string]any{"ticket": c.Param(p)}))
		if !errors.Is(err, wf.ErrUnavailable) || err.Error() != wantError {
			return errors.New("error changed")
		}
		return nil
	}).Observes(declared))
	a.Then(a.To(end, "done"))
	decl, req := declared.Desc(), requested.Desc()
	context := &wf.CheckContexts{Version: 1, Checks: []wf.RecordedCheck{{Name: "optional", Reads: []wf.RecordedRead{
		{Kind: "param", Name: "ticket", Value: json.RawMessage(`"42"`)},
		{Kind: "observed", Desc: &req, Declaration: &decl, Error: wantError, ErrorKind: "unavailable"},
	}}}}
	got, err := wf.Inspect(d).States[0].CheckRecordedOutput(json.RawMessage(`true`), context)
	if err != nil || len(got) != 0 {
		t.Fatalf("recorded observation changed: %v %v", got, err)
	}
}
