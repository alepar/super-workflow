package orchestrator_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/orchestrator"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := orchestrator.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{
		"▶ Lead",
		"Fan ",
		"Synthesize",
		"map over output of Lead",
		"join all_ok",
		"child orchestrator-worker",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	// Requery is not part of the text render; check the structural view.
	var fan wf.StateView
	for _, s := range wf.Inspect(d).States {
		if s.Name == "Fan" {
			fan = s
		}
	}
	if !fan.Requery || fan.Parallel != 0 {
		t.Errorf("Fan requery=%v parallel=%d, want true and no Parallel limit", fan.Requery, fan.Parallel)
	}
}
