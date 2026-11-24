package wftest_test

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

type stopCauseOrigin struct{}
type stopCauseReview struct{}
type stopCauseSave struct{}
type stopCauseFlush struct{}
type stopCauseEnd struct{}

// Persistence commands must preserve a declared upstream stop cause. A later
// accepted agent decision starts a new causal chain and cannot borrow that cause.
func TestFuzzStopCauseAcrossPersistenceCommands(t *testing.T) {
	for _, tc := range []struct {
		name        string
		named       bool
		review      bool
		unusedCause bool
		wantFinding bool
	}{
		{"named cause", true, false, false, false},
		{"silent stop", false, false, false, true},
		{"later agent supersedes cause", true, true, false, true},
		{"unselected sibling cause", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := func() *wftest.Sim {
				d := wf.New("persisted-stop")
				origin := wf.NewCommand[stopCauseOrigin](d, "classify")
				save := wf.NewCommand[stopCauseSave](d, "save")
				flush := wf.NewCommand[stopCauseFlush](d, "flush")
				stop := wf.NewTerminal[stopCauseEnd](d, wf.Stopped)
				edge := origin.To(save, "blocked")
				if tc.review {
					review := wf.NewAgent[stopCauseReview, bool](d)
					review.Spec(wf.PromptText("decide"), wf.Retry[stopCauseReview](wf.Const(1), review.To(stop, "review failed")))
					review.Then(review.To(save, "accepted"))
					edge = origin.To(review, "blocked")
				}
				if tc.named {
					edge = edge.Ends("open work is blocked")
				}
				if tc.unusedCause {
					unused := origin.To(save, "blocked").Ends("unused reason")
					origin.Route(func(wf.RunResult, wf.Ctx) wf.Edge[stopCauseOrigin] { return edge }, edge, unused)
				} else {
					origin.Then(edge)
				}
				save.Then(save.To(flush, "snapshot written"))
				flush.Then(flush.To(stop, "snapshot persisted"))
				return wftest.New(d)
			}
			got := wftest.Fuzz(factory, wftest.FuzzConfig{
				MaxSteps: 16, FailPercent: -1, SchemaPercent: -1,
				Valid: map[string][]json.RawMessage{"stopCauseReview": {json.RawMessage(`true`)}},
			})
			if tc.wantFinding {
				if got.Finding == nil || got.Finding.Rule != "ends-silent" {
					t.Fatalf("unexplained stop escaped the checker: %s", got)
				}
			} else if got.Finding != nil || got.Status.Terminal != "stopCauseEnd" {
				t.Fatalf("persistence lost the named stop cause: %s", got)
			}
		})
	}
}

func TestFuzzDirectStopCannotBorrowSiblingCause(t *testing.T) {
	factory := func() *wftest.Sim {
		d := wf.New("direct-stop")
		origin := wf.NewCommand[stopCauseOrigin](d, "classify")
		stop := wf.NewTerminal[stopCauseEnd](d, wf.Stopped)
		selected := origin.To(stop, "stopped")
		unused := origin.To(stop, "stopped").Ends("unused reason")
		origin.Route(func(wf.RunResult, wf.Ctx) wf.Edge[stopCauseOrigin] { return selected }, selected, unused)
		return wftest.New(d)
	}
	got := wftest.Fuzz(factory, wftest.FuzzConfig{MaxSteps: 8, FailPercent: -1})
	if got.Finding == nil || got.Finding.Rule != "ends-silent" {
		t.Fatalf("unselected end reason excused direct stop: %s", got)
	}
}
