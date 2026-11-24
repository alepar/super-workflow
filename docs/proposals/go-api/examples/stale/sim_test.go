package stale_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/stale"
	"github.com/alepar/super-workflow/docs/proposals/go-api/git"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func TestContinuallyMovingHeadStopsAfterThreeReviews(t *testing.T) {
	world := wftest.NewWorld()
	wftest.Stale(world, git.Head("."))
	tr := wftest.New(stale.Definition(), wftest.WithTB(t), wftest.WithWorld(world)).Run(
		wftest.SubmitAt("Review", stale.Verdict{Approve: true}),
		wftest.SubmitAt("Review", stale.Verdict{Approve: true}),
		wftest.SubmitAt("Review", stale.Verdict{Approve: true}),
	)
	if tr.Terminal() != "GaveUp" {
		t.Fatalf("terminal=%s\n%s", tr.Terminal(), tr)
	}
	wftest.AssertRoute(t, tr, "AwaitHead", "Review", "AwaitHead", "Review", "AwaitHead", "Review", "AwaitHead", "GaveUp")
}
