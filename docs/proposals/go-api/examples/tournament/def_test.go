package tournament_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/tournament"
)

func TestLintAndRender(t *testing.T) {
	d := tournament.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Judges",
		`map over bd.list label="candidate"`,
		"join quorum(2)",
		"param seed",
		"child tournament-judge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
