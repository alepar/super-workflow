package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	CapA   struct{}
	CapEnd struct{}
	CapOut struct{}
)

func cappedDef(withEffects bool, key wf.Key) *wf.Def {
	d := wf.New("capped").Version(1)
	n := d.Counter("n")
	a := wf.NewAgent[CapA, string](d)
	end := wf.NewTerminal[CapEnd](d, wf.Success)
	out := wf.NewTerminal[CapOut](d, wf.Stopped)
	ok := a.To(end, "ok")
	over := a.To(out, "cap")
	if withEffects {
		over = over.With(wf.ExternalEffect{Adapter: "x", Op: "write", Key: key}, n.Inc())
	}
	a.Spec(wf.PromptText("go"), wf.Retry(wf.Const(1).Plus(wf.Count(n)), ok))
	a.Route(func(string, wf.Ctx) wf.Edge[CapA] { return ok }, ok)
	wf.MaxVisits(wf.Const(2), over)
	return d
}

func capEdge(t *testing.T, d *wf.Def) wf.EdgeView {
	t.Helper()
	for _, s := range wf.Inspect(d).States {
		if s.Name != "CapA" {
			continue
		}
		for _, e := range s.Edges {
			if e.Label == "cap" {
				return e
			}
		}
	}
	t.Fatal("cap edge not found")
	return wf.EdgeView{}
}

func TestMaxVisitsOverEdgeEffectsInView(t *testing.T) {
	e := capEdge(t, cappedDef(true, wf.KeyOf("k")))
	got := strings.Join(e.Effects, "|")
	if !strings.Contains(got, "x.write key=k") || !strings.Contains(got, "inc n") {
		t.Fatalf("over edge effects = %q", got)
	}
}

func TestMaxVisitsOverEdgeEffectsRendered(t *testing.T) {
	if r := cappedDef(true, wf.KeyOf("k")).Render(); !strings.Contains(r, "{x.write key=k") {
		t.Fatalf("render lacks over-edge effect:\n%s", r)
	}
}

func TestMaxVisitsOverEdgeEffectsHashed(t *testing.T) {
	if cappedDef(true, wf.KeyOf("k")).Hash() == cappedDef(false, nil).Hash() {
		t.Fatal("hash ignores over-edge effects")
	}
}

func TestMaxVisitsOverEdgeNilKeyFailsLint(t *testing.T) {
	has := func(d *wf.Def) bool {
		for _, p := range d.Lint() {
			if p.Rule == wf.RuleEffectKey {
				return true
			}
		}
		return false
	}
	for _, p := range cappedDef(true, wf.KeyOf("k")).Lint() {
		if p.Rule == wf.RuleBudgetUnwired {
			t.Fatalf("counter incremented on the over edge reported unwired: %s", p.Msg)
		}
	}
	if !has(cappedDef(true, nil)) {
		t.Fatal("nil Key on over edge not reported")
	}
	if has(cappedDef(true, wf.KeyOf("k"))) {
		t.Fatal("keyed over edge reported")
	}
}
