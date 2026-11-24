package wf_test

import (
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

func mapClassDef(reason string, class wf.OutcomeClass, src wf.Source[chItem]) *wf.Def {
	d := wf.New("map-class")
	m := wf.NewMap[chFan, chItem, string, chRes](d, src, func(chItem) string { return "item" }, chChild())
	ok := wf.NewTerminal[chMerged](d, wf.Success)
	stop := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Join(wf.AllOK(), m.To(ok, "met"), m.To(stop, "partial"))
	d.OnFailure(wf.Blocked, stop)
	d.OnFailure(wf.Unavailable, stop)
	m.OnClass(class, m.To(ok, "own exemption").Unreachable(reason))
	return d
}

func TestMapOnClassOwnExemptionReplacesOnlyMatchingDefault(t *testing.T) {
	d := mapClassDef("source always available", wf.Blocked, chItems())
	s := wf.Inspect(d).States[0]
	if s.Retry != nil {
		t.Fatal("Map exemption invented a retry policy")
	}
	var own, blocked, unavailable int
	for _, e := range s.Edges {
		switch e.Label {
		case "own exemption":
			own++
			if !e.Unreachable || e.UnreachableReason != "source always available" || e.Class != "blocked" {
				t.Fatalf("exemption view: %+v", e)
			}
		case "default blocked route":
			blocked++
		case "default unavailable route":
			unavailable++
		}
	}
	if own != 1 || blocked != 0 || unavailable != 1 {
		t.Fatalf("edges: %+v", s.Edges)
	}
	if d.Hash() == mapClassDef("different reason", wf.Blocked, chItems()).Hash() {
		t.Fatal("reason missing from hash")
	}
	if d.Hash() == mapClassDef("source always available", wf.Unavailable, chItems()).Hash() {
		t.Fatal("class missing from hash")
	}
	if got := mapClassDef("  ", wf.Blocked, chItems()).LintReachability(); len(got) != 1 || got[0].Rule != wf.RuleUnreachableReasonEmpty {
		t.Fatalf("blank reason: %v", got)
	}
}

func TestMapOnClassRejectsMisuse(t *testing.T) {
	for _, misuse := range []string{"ordinary edge", "accepted", "unknown class", "zero edge", "foreign source", "foreign target", "after sealing"} {
		t.Run(misuse, func(t *testing.T) {
			d := wf.New("misuse")
			m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(), func(chItem) string { return "item" }, chChild())
			end := wf.NewTerminal[chMerged](d, wf.Success)
			e := m.To(end, "exempt").Unreachable("reason")
			class := wf.Blocked
			switch misuse {
			case "ordinary edge":
				e = m.To(end, "live")
			case "accepted":
				class = wf.Accepted
			case "unknown class":
				class = wf.OutcomeClass(100)
			case "zero edge":
				e = wf.Edge[chFan]{}
			case "foreign source":
				other := wf.NewMap[chFan, chItem, string, chRes](wf.New("other"), chItems(), func(chItem) string { return "item" }, chChild())
				e = other.To(end, "foreign").Unreachable("reason")
			case "foreign target":
				e = m.To(wf.NewTerminal[chMerged](wf.New("other"), wf.Success), "foreign").Unreachable("reason")
			case "after sealing":
				wf.Inspect(d)
			}
			m.OnClass(class, e)
			for _, p := range d.Lint() {
				if p.Rule == wf.RuleBuild {
					return
				}
			}
			t.Fatalf("missing build diagnostic for %s", misuse)
		})
	}
}

func TestMapOnClassForcedTraversalInvalidatesExemption(t *testing.T) {
	// An absent source forces Blocked before children spawn. The own route must
	// still execute: Unreachable is a claim checked by coverage, not a guard.
	factory := func() *wftest.Sim {
		return wftest.New(mapClassDef("claimed impossible", wf.Blocked, wf.Source[chItem]{}))
	}
	coverage := wftest.NewFuzzCoverage(factory, nil)
	coverage.Add(wftest.Fuzz(factory, wftest.FuzzConfig{Seed: 1, MaxSteps: 10}))
	for _, problem := range coverage.EdgeProblems() {
		if strings.Contains(problem, "own exemption") && strings.Contains(problem, "but 1 seeds traversed it") {
			return
		}
	}
	t.Fatalf("traversed exemption was not rejected: %v; reached=%v", coverage.EdgeProblems(), coverage.EdgesReached)
}

func TestMapOnClassWalkIncludesTraversalEffects(t *testing.T) {
	build := func(withEffect bool) *wf.Def {
		d := wf.New("map-effects")
		count := d.Counter("count")
		m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(), func(chItem) string { return "item" }, chChild())
		end := wf.NewTerminal[chMerged](d, wf.Success)
		edge := m.To(end, "exempt").Unreachable("reason")
		if withEffect {
			edge = edge.With(count.Inc())
		}
		m.OnClass(wf.Blocked, edge)
		return d
	}
	d := build(true)
	effects := wf.Inspect(d).States[0].Edges[0].Effects
	if len(effects) != 1 || effects[0] != "inc count" {
		t.Fatalf("class route traversal effects missing: %v", effects)
	}
	if d.Hash() == build(false).Hash() {
		t.Fatal("class route traversal effects missing from hash")
	}
}
