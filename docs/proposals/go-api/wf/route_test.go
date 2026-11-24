package wf_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	RcA   struct{}
	RcB   struct{}
	RcC   struct{}
	RcEnd struct{}
)

// routeDef is an agent RcA whose free-form Route declares candidates b and c;
// c optionally carries an increment of counter n, which a MaxVisits budget on
// the b edge reads.
func routeDef(incOnC bool, cands int) *wf.Def {
	d := wf.New("routed").Version(1)
	n := d.Counter("n")
	a := wf.NewAgent[RcA, string](d)
	b := wf.NewCommand[RcB](d, "b")
	c := wf.NewCommand[RcC](d, "c")
	end := wf.NewTerminal[RcEnd](d, wf.Success)
	toB := a.To(b, "b")
	toC := a.To(c, "c")
	if incOnC {
		toC = toC.With(n.Inc())
	}
	a.Spec(wf.PromptText("go"), wf.Retry(wf.Const(1), toB))
	all := []wf.Edge[RcA]{toB, toC}
	a.Route(func(string, wf.Ctx) wf.Edge[RcA] { return toB }, all[:cands]...)
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(n)), toB)
	b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[RcB] { return b.To(end, "done") })
	c.Route(func(wf.RunResult, wf.Ctx) wf.Edge[RcC] { return c.To(end, "done") })
	return d
}

func stateNamed(t *testing.T, d *wf.Def, name string) wf.StateView {
	t.Helper()
	for _, s := range wf.Inspect(d).States {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("state %s not found", name)
	return wf.StateView{}
}

func edgeNamed(t *testing.T, s wf.StateView, label string) wf.EdgeView {
	t.Helper()
	for _, e := range s.Edges {
		if e.Label == label {
			return e
		}
	}
	t.Fatalf("edge %s not found", label)
	return wf.EdgeView{}
}

func TestRouteCandidatesInView(t *testing.T) {
	s := stateNamed(t, routeDef(true, 2), "RcA")
	if !reflect.DeepEqual(s.Route, []string{"b", "c"}) {
		t.Fatalf("Route = %v", s.Route)
	}
	if got := edgeNamed(t, s, "c").Effects; !reflect.DeepEqual(got, []string{"inc n"}) {
		t.Fatalf("c effects = %v", got)
	}
}

func TestOnRouteCandidatesInView(t *testing.T) {
	s := stateNamed(t, onRouteDef(true), "OrA")
	if s.Cases == nil || !reflect.DeepEqual(s.Cases.Routes["x"], []string{"e1", "e2"}) {
		t.Fatalf("Cases = %+v", s.Cases)
	}
	if s.Cases.Cases[0] != [2]string{"x", "(route)"} {
		t.Fatalf("row = %v", s.Cases.Cases[0])
	}
	if got := edgeNamed(t, s, "e2").Effects; !reflect.DeepEqual(got, []string{"inc n"}) {
		t.Fatalf("e2 effects = %v", got)
	}
}

type (
	OrA   struct{}
	OrE1  struct{}
	OrE2  struct{}
	OrEnd struct{}
)

func onRouteDef(incOnE2 bool) *wf.Def {
	d := wf.New("onroute").Version(1)
	n := d.Counter("n")
	vals := wf.NewEnum(d, "v", "x", "y")
	a := wf.NewAgent[OrA, string](d)
	s1 := wf.NewTerminal[OrE1](d, wf.Success)
	s2 := wf.NewTerminal[OrE2](d, wf.Stopped)
	end := wf.NewTerminal[OrEnd](d, wf.Stopped)
	e1 := a.To(s1, "e1")
	e2 := a.To(s2, "e2")
	if incOnE2 {
		e2 = e2.With(n.Inc())
	}
	out := a.To(end, "y")
	a.Spec(wf.PromptText("go"), wf.Retry(wf.Const(1), e1))
	a.RouteCases(wf.Cases(strField, vals,
		wf.OnRoute("x", func(wf.Ctx) wf.Edge[OrA] { return e1 }, e1, e2),
		wf.On("y", out),
	))
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(n)), out)
	return d
}

func TestRouteCandidatesRendered(t *testing.T) {
	r := routeDef(true, 2).Render()
	if !strings.Contains(r, "route: b | c") {
		t.Fatalf("render lacks state route line:\n%s", r)
	}
	if !strings.Contains(r, "{inc n}") {
		t.Fatalf("render lacks candidate effect:\n%s", r)
	}
	if r := onRouteDef(true).Render(); !strings.Contains(r, "x → route(e1 | e2)") {
		t.Fatalf("render lacks route case row:\n%s", r)
	}
}

func TestRouteCandidatesHashed(t *testing.T) {
	if routeDef(true, 2).Hash() == routeDef(false, 2).Hash() {
		t.Fatal("hash ignores a candidate's effect")
	}
	if routeDef(true, 2).Hash() == routeDef(true, 1).Hash() {
		t.Fatal("hash ignores the candidate list")
	}
	if onRouteDef(true).Hash() == onRouteDef(false).Hash() {
		t.Fatal("hash ignores an OnRoute candidate's effect")
	}
}

func hasRule(ps []wf.Problem, r wf.RuleID) bool {
	for _, p := range ps {
		if p.Rule == r {
			return true
		}
	}
	return false
}

func TestBudgetUnwiredSeesRouteCandidates(t *testing.T) {
	if ps := routeDef(true, 2).Lint(); hasRule(ps, wf.RuleBudgetUnwired) {
		t.Fatalf("counter incremented on a Route candidate reported unwired: %v", ps)
	}
	if ps := routeDef(false, 2).Lint(); !hasRule(ps, wf.RuleBudgetUnwired) {
		t.Fatalf("budget unwired not reported although no candidate increments: %v", ps)
	}
}

func TestZeroCandidateIsMisuse(t *testing.T) {
	d, a, _, _ := okDef("zerocand")
	a.Route(func(string, wf.Ctx) wf.Edge[rA] { return wf.Edge[rA]{} }, wf.Edge[rA]{})
	if !hasRule(d.Lint(), wf.RuleBuild) {
		t.Fatal("zero candidate edge not reported as RuleBuild")
	}
}

func TestRouteCaseNoCandidatesFires(t *testing.T) {
	d, a, end, done := okDef("rcnc")
	_ = end
	a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "ok"),
		wf.OnRoute("ok", func(wf.Ctx) wf.Edge[rA] { return done })))
	if !hasRule(d.Lint(), wf.RuleRouteNoCandidates) {
		t.Fatal("OnRoute without candidates not reported")
	}
}

func TestNilCandidateInOnRouteIsBuildProblem(t *testing.T) {
	d, a, _, done := okDef("rcnil")
	a.RouteCases(wf.Cases(strField, wf.NewEnum(d, "v", "ok"),
		wf.OnRoute("ok", func(wf.Ctx) wf.Edge[rA] { return done }, wf.Edge[rA]{})))
	if !hasRule(d.Lint(), wf.RuleBuild) {
		t.Fatal("zero OnRoute candidate not reported as RuleBuild")
	}
}
