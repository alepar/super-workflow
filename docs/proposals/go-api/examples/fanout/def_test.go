package fanout_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/fanout"
)

func TestLintAndRender(t *testing.T) {
	d := fanout.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Fan",
		"map over bd.ready",
		"join all_settled",
		"some items failed",
		"Merged",
		"Partial",
		"child fanout-item",
		"checks: tests were run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
