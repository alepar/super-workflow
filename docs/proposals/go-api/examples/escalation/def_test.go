package escalation_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/escalation"
)

func TestLintAndRender(t *testing.T) {
	d := escalation.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Cheap",
		"Strong ",
		"[on refused]",
		"[on unavailable]",
		"inputs: out Cheap",
		"checks: tests green",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
