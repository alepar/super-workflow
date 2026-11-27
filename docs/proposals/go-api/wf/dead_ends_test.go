package wf_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	deOpts    struct{ Auto, Skip bool }
	deGrade   struct{}
	dePass    struct{}
	deFail    struct{}
	dePanel   struct{}
	deReview  struct{}
	deCleanup struct{}
	deAsk     struct{}
	deSubrun  struct{}
	deDone    struct{}
	deStop    struct{}
	deStall   struct{}
)

type deVerdict string

func deFind(t *testing.T, ds []wf.DeadEnd, path, trigger string) wf.DeadEnd {
	t.Helper()
	for _, d := range ds {
		if d.Path == path && d.Trigger == trigger {
			return d
		}
	}
	t.Fatalf("no dead end %s %q in:\n%s", path, trigger, wf.FormatDeadEnds("x", ds))
	return wf.DeadEnd{}
}

// deGrader is one panel seat: its verdict fail ends the seat at Failed.
func deGrader() *wf.Def {
	d := wf.New("de-grader")
	verdicts := wf.NewEnum(d, "verdict", deVerdict("pass"), deVerdict("fail"))
	grade := wf.NewAgent[deGrade, deVerdict](d)
	pass := wf.NewTerminal[dePass](d, wf.Success)
	fail := wf.NewTerminal[deFail](d, wf.Stopped)
	d.OnFailure(wf.Blocked, fail)
	grade.Spec(wf.PromptText("grade"), wf.Retry[deGrade](wf.Const(2), grade.To(fail, "no grade")))
	grade.RouteCases(wf.Cases[deGrade, deVerdict](func(v deVerdict) deVerdict { return v }, verdicts,
		wf.On(deVerdict("pass"), grade.To(pass, "passed")),
		wf.On(deVerdict("fail"), grade.To(fail, "failed")),
	))
	wf.Yield(pass, func(wf.Ctx) bool { return true })
	wf.Yield(fail, func(wf.Ctx) bool { return false })
	return d
}

// deDef holds every shape the report names: a panel whose one failing seat
// ends the run, a review whose redirect shares its exit with stop, a route
// through a cleanup state, an option route-around, a policy-answerable gate
// and a Sub exit.
func deDef() *wf.Def {
	d := wf.New("de-root")
	opts := wf.Options[deOpts](d)
	auto := wf.Field(opts, func(o *deOpts) *bool { return &o.Auto }).Is(true)
	skip := wf.Field(opts, func(o *deOpts) *bool { return &o.Skip }).Is(true)
	verdicts := wf.NewEnum(d, "review", deVerdict("approve"), deVerdict("redirect"), deVerdict("stop"))
	answers := wf.NewEnum(d, "answer", deVerdict("go"), deVerdict("halt"))
	grader := wf.Typed[string, bool](deGrader())

	panel := wf.NewMap[dePanel, string, string, bool](d, wf.FromValues("a", "b", "c", "d", "e"), func(s string) string { return s }, grader)
	review := wf.NewAgent[deReview, deVerdict](d)
	cleanup := wf.NewCommand[deCleanup](d, "true")
	ask := wf.NewGate[deAsk, deVerdict](d, "continue?", answers, wf.PolicyAnswerable)
	sub := wf.NewSub[deSubrun, string, bool](d, grader, func(wf.Ctx) string { return "solo" })
	done := wf.NewTerminal[deDone](d, wf.Success)
	stop := wf.NewTerminal[deStop](d, wf.Stopped)
	stall := wf.NewTerminal[deStall](d, wf.Stalled)
	d.OnFailures(stop, wf.Blocked, wf.Unavailable, wf.Uncertain)

	panel.Join(wf.AllOK(), panel.To(review, "panel agrees"), panel.To(stop, "a seat failed").Ends(wf.Stopped, "the panel did not agree"))

	review.Spec(wf.PromptText("review"), wf.Retry[deReview](wf.Const(3), review.To(stall, "review exhausted")))
	halt := review.To(cleanup, "halt")
	review.RouteCases(wf.Cases[deReview, deVerdict](func(v deVerdict) deVerdict { return v }, verdicts,
		wf.On(deVerdict("approve"), review.To(ask, "approved")),
		wf.On(deVerdict("redirect"), halt),
		wf.On(deVerdict("stop"), halt),
	))
	review.To(stop, "skip review").When(skip)
	cleanup.Then(cleanup.To(stop, "cleaned up"))

	ask.Answers(
		wf.On(deVerdict("go"), ask.To(sub, "go on")),
		wf.On(deVerdict("halt"), ask.To(stop, "halted").Ends(wf.Stopped, "the person halts")),
	).Timeout(time.Hour, ask.To(stop, "nobody answered"))
	d.SetPolicy(wf.Policy(wf.PolicyAnswer(ask, deVerdict("halt"), auto)))

	sub.Exits(wf.OnExit[dePass](sub.To(done, "solo passed")), wf.OnExit[deFail](sub.To(stop, "solo failed")))
	return d
}

func TestDeadEndsListsEveryRouteIntoAStopWithItsTrigger(t *testing.T) {
	d := deDef()
	if ps := d.Lint(); len(ps) != 0 {
		t.Fatalf("lint: %v", ps)
	}
	ds, err := d.DeadEnds(nil)
	if err != nil {
		t.Fatal(err)
	}

	// One grader of five ends the run.
	seat := deFind(t, ds, "de-root/dePanel", "join partial (all_ok)")
	if seat.Single != "yes" || seat.To != "deStop" || seat.Ends != "the panel did not agree" || len(seat.Causes) == 0 {
		t.Errorf("panel partial = %+v", seat)
	}
	failed := deFind(t, seat.Causes, "de-root/de-grader/deGrade", "case verdict=fail")
	if failed.Single != "yes" || failed.To != "deFail" {
		t.Errorf("seat fail cause = %+v", failed)
	}
	if !strings.Contains(seat.Why, `de-root/de-grader/deGrade "failed"`) && !strings.Contains(seat.Why, "de-grader/deGrade") {
		t.Errorf("panel why does not name the cause: %q", seat.Why)
	}

	// The approved-looking redirect shares its exit with stop, through cleanup.
	redirect := deFind(t, ds, "de-root/deReview", "case review=redirect")
	stopCase := deFind(t, ds, "de-root/deReview", "case review=stop")
	if redirect.Edge != "halt" || stopCase.Edge != "halt" || strings.Join(redirect.Via, ",") != "deCleanup" || redirect.To != "deStop" {
		t.Errorf("redirect = %+v, stop = %+v", redirect, stopCase)
	}
	// The forwarding state's own Then is not a route of its own.
	for _, x := range ds {
		if x.Path == "de-root/deCleanup" && x.Trigger == "every result" {
			t.Errorf("forwarding state listed as a deciding state: %+v", x)
		}
	}
	// Failures, budgets and timeouts are listed with their reachability.
	if ex := deFind(t, ds, "de-root/deReview", "attempts exhausted (≤3)"); ex.Single != "no" || ex.Outcome != wf.Stalled {
		t.Errorf("exhausted = %+v", ex)
	}
	if bl := deFind(t, ds, "de-root/deReview", "class blocked (definition default)"); bl.Single != "yes" {
		t.Errorf("blocked = %+v", bl)
	}
	if to := deFind(t, ds, "de-root/deAsk", "no answer within 1h0m0s"); to.Single != "no" {
		t.Errorf("timeout = %+v", to)
	}
	if h := deFind(t, ds, "de-root/deAsk", "answer=halt"); h.Single != "no" || h.Policy != nil || h.Open != nil {
		t.Errorf("halt without variants = %+v", h)
	}
	deFind(t, ds, "de-root/deReview", "route-around when Skip=true")

	// A Sub exit carries the child's routes into the mapped terminal.
	solo := deFind(t, ds, "de-root/deSubrun", "child de-grader ends at deFail")
	if solo.Single != "yes" || len(solo.Causes) != 3 {
		t.Errorf("sub exit = %+v", solo)
	}
	// No route into a success terminal is listed.
	for _, x := range ds {
		if x.To == "deDone" {
			t.Errorf("success route listed: %+v", x)
		}
	}
}

func TestDeadEndsWithVariants(t *testing.T) {
	ds, err := deDef().DeadEnds(map[string]any{"manual": deOpts{}, "auto": deOpts{Auto: true}, "fast": deOpts{Skip: true}})
	if err != nil {
		t.Fatal(err)
	}
	h := deFind(t, ds, "de-root/deAsk", "answer=halt")
	if strings.Join(h.Policy, ",") != "auto" || strings.Join(h.Open, ",") != "auto,fast,manual" {
		t.Errorf("halt = %+v", h)
	}
	if g := deFind(t, ds, "de-root/deReview", "route-around when Skip=true"); strings.Join(g.Open, ",") != "fast" {
		t.Errorf("skip open under %v, want fast", g.Open)
	}
	out := wf.FormatDeadEnds("de-root", ds)
	for _, want := range []string{"answered by policy under: auto", "open under: fast", "caused by:", "de-root/deReview -\"halt\"-> deCleanup -> deStop (stopped)"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if _, err := deDef().DeadEnds(map[string]any{"bad": "not options"}); err == nil {
		t.Error("a variant of the wrong type decoded")
	}
}

func TestDeadEndsCommand(t *testing.T) {
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: deDef()}}}
	var out, errOut bytes.Buffer
	if code := wf.CLI(cfg, []string{"dead-ends", "de-root", "--variant", `auto={"Auto":true}`}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "answered by policy under: auto") {
		t.Errorf("text report:\n%s", out.String())
	}
	out.Reset()
	if code := wf.CLI(cfg, []string{"dead-ends", "de-root", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var ds []wf.DeadEnd
	if err := json.Unmarshal(out.Bytes(), &ds); err != nil || len(ds) == 0 {
		t.Fatalf("json report %q: %v", out.String(), err)
	}
	for _, args := range [][]string{{"dead-ends"}, {"dead-ends", "unknown"}, {"dead-ends", "de-root", "--variant", "noequals"}, {"dead-ends", "de-root", "extra"}} {
		if code := wf.CLI(cfg, args, &out, &errOut); code == 0 {
			t.Errorf("%v exited 0", args)
		}
	}
}
