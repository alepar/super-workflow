package router_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/router"
)

func TestLintAndRender(t *testing.T) {
	d := router.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Classify",
		"cases kind: bug → bug | feature → feature | question → question | otherwise",
		"Bug ",
		"Feature ",
		"Question ",
		"Unclear ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
