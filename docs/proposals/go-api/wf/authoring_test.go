package wf_test

import (
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	authorPlan    struct{}
	authorPrepare struct{}
	authorRun     struct{}
	authorVerdict struct{}
	authorDone    struct{}
	authorStopped struct{}
)

type authorResult string

const (
	authorOK     authorResult = "ok"
	authorRevise authorResult = "revise"
)

func authorDefinition(short, complete bool) *wf.Def {
	d := wf.New("authoring")
	plan := wf.NewAgent[authorPlan, struct{}](d)
	prepare := wf.NewCompute[authorPrepare](d, func(wf.Ctx) (struct{}, error) { return struct{}{}, nil })
	run := wf.NewCommand[authorRun](d, "true")
	verdict := wf.NewCompute[authorVerdict](d, func(wf.Ctx) (authorResult, error) { return authorOK, nil })
	done := wf.NewTerminal[authorDone](d, wf.Success)
	stopped := wf.NewTerminal[authorStopped](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stopped)
	values := wf.NewEnum(d, "result", authorOK, authorRevise)
	exhausted := plan.To(stopped, "plan failed")
	failure := plan.To(stopped, "input unavailable")
	if short {
		plan.Spec(wf.PromptText("plan"), wf.Attempts(2, exhausted).OnClass(wf.Unavailable, failure))
		plan.ThenTo(prepare, "planned")
		prepare.ThenTo(run, "prepared")
		run.ThenTo(verdict, "command ran")
	} else {
		plan.Spec(wf.PromptText("plan"), wf.Retry(wf.Const(2), exhausted).OnClass(wf.Unavailable, failure))
		plan.Then(plan.To(prepare, "planned"))
		prepare.Then(prepare.To(run, "prepared"))
		run.Then(run.To(verdict, "command ran"))
	}
	cases := []wf.Case[authorVerdict, authorResult]{wf.On(authorOK, verdict.To(done, "accepted"))}
	if complete {
		cases = append(cases, wf.On(authorRevise, verdict.To(stopped, "needs revision")))
	}
	if short {
		wf.RouteValue(verdict, values, cases...)
	} else {
		verdict.RouteCases(wf.Cases(func(value authorResult) authorResult { return value }, values, cases...))
	}
	return d
}

func TestAuthoringHelpersPreserveDefinition(t *testing.T) {
	original, rewritten := authorDefinition(false, true), authorDefinition(true, true)
	if got, want := rewritten.Render(), original.Render(); got != want {
		t.Fatalf("helpers changed the render:\n%s", got)
	}
	if got, want := rewritten.Hash(), original.Hash(); got != want {
		t.Fatalf("helpers changed definition hash from %s to %s", want, got)
	}
}

func TestRouteValueRequiresEveryEnumValue(t *testing.T) {
	for _, problem := range authorDefinition(true, false).Lint() {
		if problem.Rule == wf.RuleCaseUncovered {
			return
		}
	}
	t.Fatal("it should be a lint error to omit an enum case")
}
