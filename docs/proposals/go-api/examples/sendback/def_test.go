package sendback_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/sendback"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestLintAndRender(t *testing.T) {
	d := sendback.Definition()
	if ps := append(d.Lint(), d.LintSound()...); len(ps) != 0 {
		t.Fatalf("lint problems: %v", ps)
	}
	if out := d.Render(); !strings.Contains(out, "[send back ≤2, then findings remain after two returns]") {
		t.Errorf("render lacks the send-back:\n%s", out)
	}
}

func sub(path string, out any) wftest.Step { return wftest.SubmitAt(path, out) }

func draft(body string) wftest.Step {
	return sub("Draft", sendback.Summary{Summary: body}).WithArtifacts(map[string]string{"draft.md": body})
}

// The reviewer returns the draft once; the drafter repairs it and the
// reviewer approves.
func TestReturnThenApprove(t *testing.T) {
	tr := wftest.New(sendback.Definition(), wftest.WithTB(t)).Run(
		draft("first"),
		sub("Review", sendback.Verdict{Findings: []string{"cite the source"}}),
		draft("second, with the source"),
		sub("Review", sendback.Verdict{OK: true}),
	)
	if tr.Terminal() != "Accepted" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
	wftest.AssertRoute(t, tr, "Draft", "Review", "Draft", "Review", "Accepted")
	rs := tr.Returns()
	if len(rs) != 1 || rs[0].To != "Draft" || rs[0].Round != 1 || rs[0].Of != 2 || rs[0].Reasons[0] != "cite the source" {
		t.Fatalf("returns = %+v", rs)
	}
}

// Two returns spend the budget; the third rejection asks the person.
func TestSpentReturnsAskThePerson(t *testing.T) {
	no := sendback.Verdict{Findings: []string{"still wrong"}}
	tr := wftest.New(sendback.Definition(), wftest.WithTB(t)).Run(
		draft("one"), sub("Review", no),
		draft("two"), sub("Review", no),
		draft("three"), sub("Review", no),
		wftest.AnswerAt("Human", "drop"),
	)
	if tr.Terminal() != "Dropped" || len(tr.Returns()) != 2 {
		t.Fatalf("terminal=%s returns=%+v\n%s", tr.Terminal(), tr.Returns(), tr)
	}
}
