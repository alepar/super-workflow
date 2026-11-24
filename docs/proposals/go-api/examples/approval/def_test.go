package approval_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/approval"
)

func TestLintAndRender(t *testing.T) {
	d := approval.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Draft",
		"Approve ",
		"Execute ",
		"[reserved]",
		"presents: plan",
		"produces: plan",
		"consumes: plan",
		"after 48h0m0s",
		"git.commit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
