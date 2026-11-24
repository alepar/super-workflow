package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestTrackerRulesFollowTheDefinition(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		tracker bool
	}{{name: "plain"}, {name: "tracker", tracker: true}} {
		t.Run(scenario.name, func(t *testing.T) {
			d := wf.New(scenario.name)
			work := wf.NewAgent[eA, eOut](d)
			done, bad := twoTerminals(d)
			work.Spec(wf.PromptText("Do the task."), wf.Retry[eA](wf.Const(2), work.To(bad, "gave up")))
			next := work.To(done, "done")
			if scenario.tracker {
				next = next.With(bd.Create(wf.KeyOf("task"), bd.Issue{Title: "Follow up"}))
			}
			work.Then(next)
			assertTrackerRules(t, d, scenario.tracker)
		})
	}
}

type (
	nestedTrackerSub  struct{}
	nestedTrackerRead struct{}
)

func TestTrackerRulesIncludeNestedBeadsObservations(t *testing.T) {
	child := wf.New("read-tracker")
	work := wf.NewAgent[eA, eOut](child)
	obs := bd.Show("task")
	read := wf.NewCompute[nestedTrackerRead, bd.Issue](child, func(c wf.Ctx) (bd.Issue, error) {
		return wf.Observed(c, obs)
	})
	read.Observes(obs)
	childDone, childBad := twoTerminals(child)
	work.Spec(wf.PromptText("Do the task."), wf.Retry[eA](wf.Const(2), work.To(childBad, "gave up")))
	work.Then(work.To(read, "read the task"))
	read.Then(read.To(childDone, "task read"))
	yield := func(wf.Ctx) struct{} { return struct{}{} }
	wf.Yield(childDone, yield)
	wf.Yield(childBad, yield)

	parent := wf.New("nested-tracker")
	sub := wf.NewSub[nestedTrackerSub](parent, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
	done, bad := twoTerminals(parent)
	sub.Exits(wf.OnExit[eDone](sub.To(done, "child done")), wf.OnExit[eBad](sub.To(bad, "child stopped")))
	assertTrackerRules(t, parent, true)
}

func assertTrackerRules(t *testing.T, d *wf.Def, tracker bool) {
	t.Helper()
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	status, err := h.e.Status(run)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := h.e.(*wf.Engine).PromptFetch(run, ins.Name, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range []struct{ name, text, trackedRules string }{
		{"bootstrap", wf.Bootstrap(status, "workflow", "/state"), wf.RulesForThisRun},
		{"brief", ins.Brief, wf.RulesForSpawnBrief},
		{"prompt", fetched.Prompt, wf.RulesForAnyAgent},
	} {
		if got := strings.Contains(surface.text, "tracker items"); got != tracker {
			t.Errorf("%s tracker rule = %t, want %t", surface.name, got, tracker)
		}
		if tracker && !strings.Contains(surface.text, surface.trackedRules) {
			t.Errorf("%s changed the existing tracker rules", surface.name)
		}
	}
}
