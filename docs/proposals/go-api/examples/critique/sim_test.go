package critique_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"testing"
)

// The tutorial's revise/approve script executes the real definition, rather
// than walking its render as if the route were the runtime.
func TestTutorialReviewLoop(t *testing.T) {
	d := critique.Definition()
	tr := wftest.New(d, wftest.WithTB(t)).Run(
		wftest.SubmitAt("Draft", critique.Text{Body: "first draft"}),
		wftest.SubmitAt("Critique", critique.Review{Verdict: critique.Revise, Notes: "add evidence"}),
		wftest.SubmitAt("Draft", critique.Text{Body: "draft with evidence"}),
		wftest.SubmitAt("Critique", critique.Review{Verdict: critique.Approve, Notes: "supported"}),
	)
	if tr.Terminal() != "Accepted" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
	wftest.AssertRoute(t, tr, "Draft", "Critique", "Draft", "Critique", "Accepted")
}

func TestTutorialLoopCapReachesHuman(t *testing.T) {
	d := critique.Definition()
	var script []wftest.Step
	for i := 0; i < 5; i++ {
		script = append(script, wftest.SubmitAt("Draft", critique.Text{Body: "a draft"}), wftest.SubmitAt("Critique", critique.Review{Verdict: critique.Revise}))
	}
	script = append(script, wftest.SubmitAt("Draft", critique.Text{Body: "last draft"}), wftest.AnswerAt("Human", "drop"))
	tr := wftest.New(d, wftest.WithTB(t)).Run(script...)
	if tr.Terminal() != "Dropped" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
}
