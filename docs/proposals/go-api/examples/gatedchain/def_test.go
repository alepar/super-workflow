package gatedchain_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
)

func TestLintAndRender(t *testing.T) {
	d := gatedchain.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Draft",
		"Vet ",
		"Expand ",
		"SchemaRejected",
		"VetRejected",
		"Done",
		"fsx.schema_check",
		"checks: outline matches schema",
		"terminal ■ stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}
