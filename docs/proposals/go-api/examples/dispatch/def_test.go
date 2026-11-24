package dispatch_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/dispatch"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := dispatch.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Fan", "Merged", "Partial", "map over", "child dispatch-item", "Review"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	const hints = "effort=low agent-type=reviewer tier=standard"
	if got := wf.Inspect(d).Children[0].States[0].Dispatch; got != hints {
		t.Errorf("hints %q", got)
	}
	if !strings.Contains(out, "dispatch: "+hints) {
		t.Errorf("render lacks hints\n%s", out)
	}
}
