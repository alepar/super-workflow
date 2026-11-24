package stale_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
)

func TestLintAndRender(t *testing.T) {
	d := stale.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"AwaitHead", "Review", "Reviewed", "TimedOut", "GaveUp",
		"stale v2",
		"wait head moved past the reviewed commit (poll 30s)",
		`observes: git.head worktree="."`,
		"[on stale]",
		"no new commit in a day after 24h0m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
}
