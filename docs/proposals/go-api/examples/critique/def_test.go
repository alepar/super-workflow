package critique_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := critique.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Draft",
		"Critique ",
		"Human ",
		"retry ≤3 + rounds, stall after 2",
		"max visits: 5",
		"revise {inc rounds}",
		"[reserved]",
		"after 24h0m0s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

// TestCriticRoundsDispatchCount walks the declared edges with a critic that
// always asks for a revision and pins the dispatch count: the critic runs on
// arrivals 1..5; the sixth draft goes to the human gate unreviewed.
func TestCriticRoundsDispatchCount(t *testing.T) {
	states := map[string]wf.StateView{}
	for _, s := range wf.Inspect(critique.Definition()).States {
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
	cap, err := strconv.Atoi(states["Critique"].MaxVisits)
	if err != nil {
		t.Fatalf("Critique max visits %q: %v", states["Critique"].MaxVisits, err)
	}
	entries, drafts, at := 0, 0, "Draft"
	for i := 0; i < 100 && at != "Human"; i++ {
		switch at {
		case "Draft":
			drafts++
			at = edge("Draft", "drafted")
		case "Critique":
			if entries == cap {
				at = edge("Critique", "round cap reached")
				continue
			}
			entries++
			at = edge("Critique", "revise")
		}
	}
	if entries != 5 || drafts != 6 || at != "Human" {
		t.Errorf("Critique entered %d times over %d drafts, then %s; want 5 over 6 then Human", entries, drafts, at)
	}
}
