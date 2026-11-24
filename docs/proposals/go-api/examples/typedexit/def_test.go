package typedexit_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/typedexit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := typedexit.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Work", "Next", "Done", "LimitsStop", "FormatStop", "BlockedStop",
		"cases exit: submitted → submitted | limits_exceeded → limits exceeded | format_error_repeated → format error repeated | blocked → blocked | continue → next step",
		"max visits: 8"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	if got := wf.Inspect(d).Enums["exit"]; strings.Join(got, ",") != "submitted,limits_exceeded,format_error_repeated,blocked,continue" {
		t.Errorf("exit enum %v", got)
	}
}

// TestStepCapDispatchCount walks the declared edges, always answering
// "continue", and pins the dispatch count: the agent runs on arrivals 1..8 and
// the ninth arrival takes the visit-cap edge.
func TestStepCapDispatchCount(t *testing.T) {
	states := map[string]wf.StateView{}
	for _, s := range wf.Inspect(typedexit.Definition()).States {
		states[s.Name] = s
	}
	edge := func(state, label string) string {
		for _, e := range states[state].Edges {
			if e.Label == label {
				return e.To
			}
		}
		t.Fatalf("%s has no edge %q", state, label)
		return ""
	}
	cap, err := strconv.Atoi(states["Work"].MaxVisits)
	if err != nil {
		t.Fatalf("Work max visits %q: %v", states["Work"].MaxVisits, err)
	}
	entries, at := 0, "Work"
	for i := 0; i < 100 && at == "Work"; i++ {
		if entries == cap {
			at = edge("Work", "visit cap reached before the step")
			break
		}
		entries++
		next := edge("Work", "next step")
		at = edge(next, "back to work")
	}
	if entries != 8 || at != "LimitsStop" {
		t.Errorf("Work entered %d times then %s, want 8 then LimitsStop", entries, at)
	}
}
