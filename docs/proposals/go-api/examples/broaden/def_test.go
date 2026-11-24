package broaden_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/broaden"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestLintAndRender(t *testing.T) {
	d := broaden.Definition()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	out := d.Render()
	for _, want := range []string{"Answer", "AnswerWider", "Accepted", "Escalated",
		"cases confidence: confident → confident: accept | unsure → unsure: re-run with broadened context",
		"inputs: out Answer, param wider_context"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q\n%s", want, out)
		}
	}
	var names []string
	for _, s := range wf.Inspect(d).States {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "Answer,AnswerWider,Accepted,Escalated" {
		t.Errorf("states %s", got)
	}
}
