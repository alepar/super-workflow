package wf_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	ScA   struct{}
	ScB   struct{}
	ScEnd struct{}
)

// scopedDef builds A -> B -> end with a budget on the A->B edge reading re.
// scoped selects whether re is scoped to round; incRound and incRe choose which
// Incs the edges carry (re first in declaration order when both are set).
func scopedDef(scoped, incRound, incRe bool) (*wf.Def, wf.Edge[ScA]) {
	d := wf.New("scoped").Version(1)
	round := d.Counter("round")
	var re wf.Counter
	if scoped {
		re = d.Counter("redispatch", wf.ScopedTo(round))
	} else {
		re = d.Counter("redispatch")
	}
	a := wf.NewCommand[ScA](d, "a")
	b := wf.NewCommand[ScB](d, "b")
	end := wf.NewTerminal[ScEnd](d, wf.Success)
	ab := a.To(b, "next")
	if incRe {
		ab = ab.With(re.Inc())
	}
	if incRound {
		ab = ab.With(round.Inc())
	}
	a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScA] { return ab }, ab)
	bd := b.To(end, "done")
	b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScB] { return bd }, bd)
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(re)), bd)
	return d, ab
}

func TestScopedCounterRendersNameScope(t *testing.T) {
	d, _ := scopedDef(true, true, true)
	if got := wf.Inspect(d).Counters; !reflect.DeepEqual(got, []string{"round", "redispatch/round"}) {
		t.Fatalf("Counters = %v", got)
	}
	out := d.Render()
	for _, want := range []string{"inc redispatch/round", "1 + redispatch/round"} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
}

func TestScopedCounterHashed(t *testing.T) {
	plain, _ := scopedDef(false, true, true)
	scoped, _ := scopedDef(true, true, true)
	if plain.Hash() == scoped.Hash() {
		t.Fatal("scoping a counter did not change the definition hash")
	}
}

func TestPlainCounterViewUnchanged(t *testing.T) {
	d, _ := scopedDef(false, true, true)
	if got := wf.Inspect(d).Counters; !reflect.DeepEqual(got, []string{"round", "redispatch"}) {
		t.Fatalf("Counters = %v", got)
	}
	if out := d.Render(); !strings.Contains(out, "inc redispatch") || strings.Contains(out, "redispatch/") {
		t.Errorf("plain counter render:\n%s", out)
	}
}

func TestSameEdgeEffectOrder(t *testing.T) {
	d, _ := scopedDef(true, true, true) // declared: re.Inc(), round.Inc()
	got := edgeNamed(t, stateNamed(t, d, "ScA"), "next").Effects
	if want := []string{"inc round", "inc redispatch/round"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped edge effects = %v, want %v", got, want)
	}

	d2 := wf.New("unrelated")
	x, y := d2.Counter("x"), d2.Counter("y")
	a := wf.NewCommand[ScA](d2, "a")
	end := wf.NewTerminal[ScEnd](d2, wf.Success)
	e := a.To(end, "go").With(y.Inc(), x.Inc())
	a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScA] { return e }, e)
	got = edgeNamed(t, stateNamed(t, d2, "ScA"), "go").Effects
	if want := []string{"inc y", "inc x"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unrelated edge effects = %v, want %v", got, want)
	}
}

func TestCounterScopeLint(t *testing.T) {
	for _, tc := range []struct {
		name               string
		incRound, incRe, w bool
	}{
		{"scope never incremented", false, true, true},
		{"scoped never incremented", true, false, true},
		{"both incremented", true, true, false},
	} {
		d, _ := scopedDef(true, tc.incRound, tc.incRe)
		if got := hasRule(d.Lint(), wf.RuleCounterScope); got != tc.w {
			t.Errorf("%s: counter-scope fired = %v, want %v", tc.name, got, tc.w)
		}
	}
}

// The scope Inc sits on a route candidate edge, not on the edge carrying the
// scoped Inc: the candidate walk makes it visible to lint.
func TestCounterScopeLintSeesRouteCandidates(t *testing.T) {
	d := wf.New("cand").Version(1)
	round := d.Counter("round")
	re := d.Counter("redispatch", wf.ScopedTo(round))
	a := wf.NewCommand[ScA](d, "a")
	b := wf.NewCommand[ScB](d, "b")
	end := wf.NewTerminal[ScEnd](d, wf.Success)
	loop := a.To(a, "again").With(round.Inc())
	next := a.To(b, "next").With(re.Inc())
	a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScA] { return next }, loop, next)
	bd := b.To(end, "done")
	b.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScB] { return bd }, bd)
	wf.MaxVisits(wf.Const(1).Plus(wf.Count(re)), bd)
	if hasRule(d.Lint(), wf.RuleCounterScope) {
		t.Fatalf("counter-scope fired although both Incs are on candidate edges: %v", d.Lint())
	}
}

func TestScopedToMisuse(t *testing.T) {
	d := wf.New("misuse")
	d.Counter("x", wf.ScopedTo(wf.Counter{}))
	if !hasRule(d.Lint(), wf.RuleBuild) {
		t.Error("ScopedTo(zero Counter) did not record a build problem")
	}
	d2 := wf.New("misuse2")
	round := d2.Counter("round")
	mid := d2.Counter("mid", wf.ScopedTo(round))
	d2.Counter("leaf", wf.ScopedTo(mid))
	if !hasRule(d2.Lint(), wf.RuleBuild) {
		t.Error("ScopedTo(scoped Counter) did not record a build problem")
	}
}

// A scope declared on another Def is invisible to this Def's executor
// instance: the counter would never be zeroed.
func TestCounterScopeDefLint(t *testing.T) {
	build := func(sameDef bool) *wf.Def {
		p := wf.New("p")
		round := p.Counter("round")
		c := wf.New("c")
		scope := round
		if sameDef {
			scope = c.Counter("round")
		}
		x := c.Counter("x", wf.ScopedTo(scope))
		a := wf.NewCommand[ScA](c, "a")
		end := wf.NewTerminal[ScEnd](c, wf.Success)
		e := a.To(end, "go").With(x.Inc(), scope.Inc())
		a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScA] { return e }, e)
		return c
	}
	if !hasRule(build(false).Lint(), wf.RuleCounterScopeDef) {
		t.Error("counter-scope-def did not fire for a scope declared on another Def")
	}
	if hasRule(build(true).Lint(), wf.RuleCounterScopeDef) {
		t.Errorf("counter-scope-def fired for a same-Def scope: %v", build(true).Lint())
	}
}

// An edge that carries the same Inc twice shows it twice; effects repeated
// only because several uses share the edge are listed once.
func TestRepeatedIncOnOneEdge(t *testing.T) {
	d := wf.New("repeat")
	n := d.Counter("n")
	a := wf.NewCommand[ScA](d, "a")
	end := wf.NewTerminal[ScEnd](d, wf.Success)
	twice := a.To(end, "twice").With(n.Inc(), n.Inc())
	a.Route(func(wf.RunResult, wf.Ctx) wf.Edge[ScA] { return twice }, twice)
	got := edgeNamed(t, stateNamed(t, d, "ScA"), "twice").Effects
	if want := []string{"inc n", "inc n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated Inc effects = %v, want %v", got, want)
	}

	d2 := wf.New("shared")
	m := d2.Counter("m")
	a2 := wf.NewCommand[ScA](d2, "a")
	end2 := wf.NewTerminal[ScEnd](d2, wf.Success)
	once := a2.To(end2, "once").With(m.Inc())
	a2.RouteCases(wf.CasesOpen[ScA](func(r wf.RunResult) bool { return r.Exit == 0 }, wf.On(true, once)).Otherwise(once))
	got = edgeNamed(t, stateNamed(t, d2, "ScA"), "once").Effects
	if want := []string{"inc m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shared-edge effects = %v, want %v", got, want)
	}
}
