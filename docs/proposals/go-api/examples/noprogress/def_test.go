package noprogress_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/noprogress"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := noprogress.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Fix", "Fixed", "GaveUp", "StalledAt",
		"five attempts used [retry ≤5, stall after 2, then recovery offer]",
		"same failures twice: stalled [stalled]"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	r := wf.Inspect(d).States[0].Retry
	if r == nil || r.Fingerprint != "failing-check-ids" || r.NoProgressAfter != 2 {
		t.Errorf("retry %+v", r)
	}
}
