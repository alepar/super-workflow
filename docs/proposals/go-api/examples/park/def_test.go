package park_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/park"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := park.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Release", "Shipped", "Held", "[policy-answerable]",
		"policy: hold when Autonomous=true (park: release held for human sign-off; ship manually when ready)"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	if p := wf.Inspect(d).Policy; len(p) != 1 || p[0].Park == "" {
		t.Errorf("policy %v", p)
	}
}
