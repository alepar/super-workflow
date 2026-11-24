package unavailable_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/unavailable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := unavailable.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Triage", "TriageDone", "ToolBlocked", "NotConverged",
		"three semantic failures [retry ≤3, then recovery offer]",
		"tool unavailable: file a blocker [on unavailable] {bd.create key=bd/blocker/triage tool unavailable issue="} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	r := wf.Inspect(d).States[0].Retry
	if r.ByClass["unavailable"] == r.Exhausted {
		t.Errorf("unavailable and exhausted share a route: %+v", r)
	}
}
