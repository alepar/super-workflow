package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	CanonOpts  struct{ A, B, C bool }
	CanonStart struct{}
	CanonStop  struct{}
	CanonEnd   struct{}
)

// canonDef builds a small definition whose pieces vary: the edge guard, the
// args of an external effect on a fallback edge, and the args of a DoneWhen
// observation.
func canonDef(pred func(a, b wf.Pred) wf.Pred, effectArgs, obsArgs map[string]any) *wf.Def {
	d := wf.New("canon")
	opts := wf.Options[CanonOpts](d)
	a, b := wf.Field(opts, func(o *CanonOpts) *bool { return &o.A }).Is(true), wf.Field(opts, func(o *CanonOpts) *bool { return &o.B }).Is(true)
	start := wf.NewAgent[CanonStart, string](d)
	end := wf.NewTerminal[CanonEnd](d, wf.Success)
	stop := wf.NewTerminal[CanonStop](d, wf.Stopped)
	ok := start.To(end, "ok").When(pred(a, b))
	bad := start.To(stop, "bad").With(wf.ExternalEffect{Adapter: "bd", Op: "create", Args: effectArgs, Key: wf.KeyOf("k")})
	start.Spec(wf.PromptText("go"), wf.Retry(wf.Const(1), bad))
	start.Route(func(string, wf.Ctx) wf.Edge[CanonStart] { return ok }, ok)
	wf.DoneWhen(start, wf.Observe[bool]("fs", "exists", obsArgs), "exists")
	return d
}

func andPred(a, b wf.Pred) wf.Pred { return a.And(b) }

func TestPredGrouping(t *testing.T) {
	d := wf.New("p")
	opts := wf.Options[CanonOpts](d)
	a, b, c := wf.Field(opts, func(o *CanonOpts) *bool { return &o.A }).Is(true), wf.Field(opts, func(o *CanonOpts) *bool { return &o.B }).Is(true), wf.Field(opts, func(o *CanonOpts) *bool { return &o.C }).Is(true)
	grouped, split := wf.Not(a.And(b)).String(), wf.Not(a).And(b).String()
	if grouped == split {
		t.Fatalf("Not(A and B) and (Not A) and B both render %q", grouped)
	}
	if grouped != "not (A=true and B=true)" {
		t.Errorf("Not(A.And(B)) = %q", grouped)
	}
	if split != "not A=true and B=true" {
		t.Errorf("Not(A).And(B) = %q", split)
	}
	if l, r := a.And(b).And(c).String(), a.And(b.And(c)).String(); l != r || l != "A=true and B=true and C=true" {
		t.Errorf("And not canonical: %q vs %q", l, r)
	}
	if got := wf.Not(wf.Not(a)).String(); got != "not not A=true" {
		t.Errorf("double negation = %q", got)
	}
}

func TestPredGroupingHash(t *testing.T) {
	args := map[string]any{"title": "t"}
	d1 := canonDef(func(a, b wf.Pred) wf.Pred { return wf.Not(a.And(b)) }, args, nil)
	d2 := canonDef(func(a, b wf.Pred) wf.Pred { return wf.Not(a).And(b) }, args, nil)
	if d1.Hash() == d2.Hash() {
		t.Error("Not(A and B) and (Not A) and B hash the same")
	}
	if d1.Render() == d2.Render() {
		t.Error("Not(A and B) and (Not A) and B render the same")
	}
	if !strings.Contains(d1.Render(), "not (A=true and B=true)") {
		t.Errorf("render lacks grouped predicate:\n%s", d1.Render())
	}
}

func TestEffectArgsInRenderAndHash(t *testing.T) {
	d1 := canonDef(andPred, map[string]any{"title": "one", "severity": 2}, nil)
	d2 := canonDef(andPred, map[string]any{"title": "two", "severity": 2}, nil)
	if d1.Hash() == d2.Hash() {
		t.Error("effect args do not change the hash")
	}
	// Args render sorted by key, after the key.
	if want := `bd.create key=k severity=2 title="one"`; !strings.Contains(d1.Render(), want) {
		t.Errorf("render lacks %q:\n%s", want, d1.Render())
	}
}

func TestSemanticallyDifferentDefsHashDifferently(t *testing.T) {
	notAnd := func(a, b wf.Pred) wf.Pred { return wf.Not(a.And(b)) }
	andNot := func(a, b wf.Pred) wf.Pred { return wf.Not(a).And(b) }
	e1, e2 := map[string]any{"x": 1}, map[string]any{"x": 2}
	o1, o2 := map[string]any{"path": "a"}, map[string]any{"path": "b"}
	cases := []struct {
		name string
		a, b *wf.Def
	}{
		{"predicate grouping", canonDef(notAnd, e1, o1), canonDef(andNot, e1, o1)},
		{"effect args", canonDef(andPred, e1, o1), canonDef(andPred, e2, o1)},
		{"observation args", canonDef(andPred, e1, o1), canonDef(andPred, e1, o2)},
		{"[]string args", canonDef(andPred, map[string]any{"argv": []string{"a b"}}, o1), canonDef(andPred, map[string]any{"argv": []string{"a", "b"}}, o1)},
		{"struct args", canonDef(andPred, map[string]any{"s": canonArg{A: "x y"}}, o1), canonDef(andPred, map[string]any{"s": canonArg{A: "x", B: "y"}}, o1)},
	}
	for _, c := range cases {
		if c.a.Hash() == c.b.Hash() {
			t.Errorf("%s: different definitions hash the same (%s)", c.name, c.a.Hash())
		}
	}
	if canonDef(andPred, e1, o1).Hash() != canonDef(andPred, e1, o1).Hash() {
		t.Error("identical definitions hash differently")
	}
}

type canonArg struct{ A, B string }

// TestArgsEncodingInjective: pairs that a bare %v conflates must differ in
// Render and Hash, both as effect args and as observation args.
func TestArgsEncodingInjective(t *testing.T) {
	pairs := []struct {
		name string
		a, b any
	}{
		{"slice elements", []string{"a b"}, []string{"a", "b"}},
		{"string vs int", "1", 1},
		{"nil vs empty slice", []string(nil), []string{}},
		{"struct fields", canonArg{A: "x y"}, canonArg{A: "x", B: "y"}},
	}
	for _, p := range pairs {
		ea, eb := map[string]any{"v": p.a}, map[string]any{"v": p.b}
		for _, site := range []string{"effect", "observation"} {
			var da, db *wf.Def
			if site == "effect" {
				da, db = canonDef(andPred, ea, nil), canonDef(andPred, eb, nil)
			} else {
				da, db = canonDef(andPred, nil, ea), canonDef(andPred, nil, eb)
			}
			if da.Render() == db.Render() {
				t.Errorf("%s as %s args: same Render:\n%s", p.name, site, da.Render())
			}
			if da.Hash() == db.Hash() {
				t.Errorf("%s as %s args: same Hash %s", p.name, site, da.Hash())
			}
		}
	}
}

func TestStructArgsRender(t *testing.T) {
	d := canonDef(andPred, map[string]any{"s": canonArg{A: "a", B: "b"}}, nil)
	if want := `canonArg{A: "a", B: "b"}`; !strings.Contains(d.Render(), want) {
		t.Errorf("render lacks %q:\n%s", want, d.Render())
	}
}
