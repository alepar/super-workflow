package agentless_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/agentless"
)

func TestLintAndRender(t *testing.T) {
	d := agentless.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Localize",
		"Repair ",
		"Validate ",
		"Rank ",
		"Select ",
		"map over output of Localize",
		"compute → Choice",
		"command go test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
