package critique_test

import (
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/critique"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"testing"
)

func TestReservedGateDelivery(t *testing.T) {
	var steps []wftest.Step
	for i := 0; i < 5; i++ {
		steps = append(steps, wftest.SubmitAt("Draft", critique.Text{Body: fmt.Sprint(i)}), wftest.SubmitAt("Critique", critique.Review{Verdict: critique.Revise, Notes: "revise"}))
	}
	steps = append(steps, wftest.SubmitAt("Draft", critique.Text{Body: "sixth"}))
	r := wftest.CheckReservedGateDelivery(func() *wftest.Sim { return wftest.New(critique.Definition()) }, "Human", "drop", steps...)
	t.Log(r)
	if r.PositiveControls != 8 || len(r.FixtureGaps) > 0 || len(r.Problems) > 0 {
		t.Fatal(r)
	}
}
