package resumable_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/resumable"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := resumable.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Fetch", "Transform", "Publish", "Verify", "Done", "Failed",
		"resumable v3",
		"done when: raw.json exists",
		"done when: shaped.json exists",
		"done when: published.json digest recorded",
		"[on uncertain]",
		"shell.run key=non-idempotent",
		"shell.run key=stamp/{dataset}"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	if rk := wf.Inspect(d).RunKey; len(rk) != 1 || rk[0] != "dataset" {
		t.Errorf("run key %v", rk)
	}
}
