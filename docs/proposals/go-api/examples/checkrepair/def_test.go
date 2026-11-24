package checkrepair_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/checkrepair"
)

func TestLintAndRender(t *testing.T) {
	d := checkrepair.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"RunTests", "CaptureFailure", "Repair", "Green", "GaveUp",
		"command go test ./...",
		"cases test-exit: 0 → tests pass | 1 → tests fail: capture stderr",
		"inputs: out CaptureFailure",
		`observes: shell.run argv=["go", "vet", "./..."] dir="", fsx.schema_check`} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
}
