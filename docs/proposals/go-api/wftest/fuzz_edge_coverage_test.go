package wftest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type coverageSource struct{}
type coverageEnd struct{}
type coverageSub struct{}

func coverageDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewCompute[coverageSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	end := wf.NewTerminal[coverageEnd](d, wf.Success)
	first, second := a.To(end, "same"), a.To(end, "same")
	a.Route(func(bool, wf.Ctx) wf.Edge[coverageSource] { return second }, first, second)
	wf.Yield(end, func(wf.Ctx) struct{} { return struct{}{} })
	return d
}

func TestEdgeCoverageDistinguishesParallelEdges(t *testing.T) {
	factory := func() *Sim { return New(coverageDef("edges")) }
	c := NewFuzzCoverage(factory, nil)
	c.Add(Fuzz(factory, FuzzConfig{Seed: 1, MaxSteps: 10}))
	problems := c.EdgeProblems()
	if len(problems) != 1 || !strings.Contains(problems[0], "edge[0]") {
		t.Fatalf("uncovered parallel edge: %v", problems)
	}
	if !strings.Contains(problems[0], `.Unreachable("reason")`) {
		t.Fatalf("missing concrete annotation guidance: %v", problems)
	}
	if c.EdgesReached["edges/coverageSource/edge[1]"] != 1 {
		t.Fatalf("committed second edge not covered: %v", c.EdgesReached)
	}
}

func coverageRecord(kind wf.JournalKind, instance, state string, data any) wf.JournalRecord {
	raw, _ := json.Marshal(data)
	return wf.JournalRecord{Kind: kind, Instance: instance, State: state, Data: raw}
}

func TestEdgeCoverageRequiresCommittedPlan(t *testing.T) {
	factory := func() *Sim { return New(coverageDef("edges")) }
	c := NewFuzzCoverage(factory, nil)
	first := wf.EdgePlan{Steps: []wf.EdgeStep{{From: "coverageSource", Index: 0, To: "coverageEnd", Label: "same"}}}
	second := wf.EdgePlan{Steps: []wf.EdgeStep{{From: "coverageSource", Index: 1, To: "coverageEnd", Label: "same"}}}
	// A failed effect abandons the accepted route and commits its failure route.
	trace := &Trace{journal: []wf.JournalRecord{
		coverageRecord(wf.JournalAccepted, "", "coverageSource", wf.AcceptedData{Plan: first}),
		coverageRecord(wf.JournalRefused, "", "coverageSource", wf.RefusedData{Plan: &second}),
		coverageRecord(wf.JournalTransition, "", "coverageSource", wf.TransitionData{To: "coverageEnd"}),
		coverageRecord(wf.JournalAccepted, "", "coverageSource", wf.AcceptedData{Plan: first}),
	}}
	c.Add(&FuzzResult{End: "success", Trace: trace})
	if len(c.EdgeProblems()) != 1 || c.EdgesReached["edges/coverageSource/edge[0]"] != 0 || c.EdgesReached["edges/coverageSource/edge[1]"] != 1 {
		t.Fatalf("failed or uncommitted plan counted: %v; %v", c.EdgeProblems(), c.EdgesReached)
	}
}

func TestEdgeCoverageQualifiesNestedDefinitions(t *testing.T) {
	factory := func() *Sim {
		child := coverageDef("child")
		parent := wf.New("parent")
		sub := wf.NewSub[coverageSub](parent, wf.Typed[struct{}, struct{}](child), func(wf.Ctx) struct{} { return struct{}{} })
		end := wf.NewTerminal[coverageEnd](parent, wf.Success)
		sub.Exits(wf.OnExit[coverageEnd](sub.To(end, "done")))
		return New(parent)
	}
	c := NewFuzzCoverage(factory, nil)
	c.Add(Fuzz(factory, FuzzConfig{Seed: 1, MaxSteps: 10}))
	if c.EdgesReached["parent/child/coverageSource/edge[1]"] != 1 || c.EdgesReached["parent/coverageSub/edge[0]"] != 1 {
		t.Fatalf("qualified child coverage: %v", c.EdgesReached)
	}
	if len(c.EdgeProblems()) != 1 {
		t.Fatalf("want only first child edge missing: %v", c.EdgeProblems())
	}
}

func TestEdgeCoverageExemptionRequiresReasonAndMustStayUnreached(t *testing.T) {
	for _, reason := range []string{"reserved for a future input", "   "} {
		t.Run(reason, func(t *testing.T) {
			factory := func() *Sim {
				d := wf.New("exempt")
				a := wf.NewCompute[coverageSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
				end := wf.NewTerminal[coverageEnd](d, wf.Success)
				unused := a.To(end, "unused").Unreachable(reason)
				used := a.To(end, "used")
				a.Route(func(bool, wf.Ctx) wf.Edge[coverageSource] { return used }, unused, used)
				return New(d)
			}
			c := NewFuzzCoverage(factory, nil)
			c.Add(Fuzz(factory, FuzzConfig{Seed: 1, MaxSteps: 10}))
			if got := len(c.EdgeProblems()); (got == 0) != (strings.TrimSpace(reason) != "") {
				t.Fatalf("reason %q problems %v", reason, c.EdgeProblems())
			}
			c.EdgesReached["exempt/coverageSource/edge[0]"] = 1
			if len(c.EdgeProblems()) == 0 {
				t.Fatal("reached exemption accepted")
			}
		})
	}
}

func TestEdgeCoverageCountsEveryCommittedPlanKindOncePerSeed(t *testing.T) {
	plan := wf.EdgePlan{Steps: []wf.EdgeStep{{From: "coverageSource", Index: 1, To: "coverageEnd", Label: "same"}}}
	for _, kind := range []wf.JournalKind{wf.JournalAccepted, wf.JournalCompletedByObservation, wf.JournalRefused, wf.JournalRouteOffCandidates, wf.JournalPromptOffCandidates, wf.JournalAutoAnswered, wf.JournalGateAnswered, wf.JournalRetried} {
		t.Run(kind.String(), func(t *testing.T) {
			c := NewFuzzCoverage(func() *Sim { return New(coverageDef("edges")) }, nil)
			records := []wf.JournalRecord{
				coverageRecord(kind, "", "coverageSource", struct{ Plan wf.EdgePlan }{plan}),
				coverageRecord(wf.JournalTransition, "", "coverageSource", wf.TransitionData{To: "coverageEnd"}),
			}
			c.Add(&FuzzResult{End: "success", Trace: &Trace{journal: append(records, records...)}})
			if c.EdgesReached["edges/coverageSource/edge[1]"] != 1 {
				t.Fatalf("traversal seed count: %v", c.EdgesReached)
			}
			c.Add(&FuzzResult{End: "success", Trace: &Trace{journal: records}})
			if c.EdgesReached["edges/coverageSource/edge[1]"] != 2 {
				t.Fatalf("suite seed count: %v", c.EdgesReached)
			}
		})
	}
}

func TestEdgeCoverageCreditsCommittedBudgetRedirectSteps(t *testing.T) {
	d := wf.New("budget")
	a := wf.NewCompute[coverageSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	b := wf.NewCompute[coverageSub, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	end := wf.NewTerminal[coverageEnd](d, wf.Success)
	next := a.To(b, "next")
	over := b.To(end, "over")
	a.Route(func(bool, wf.Ctx) wf.Edge[coverageSource] { return next }, next)
	b.Route(func(bool, wf.Ctx) wf.Edge[coverageSub] { return over }, over)
	c := NewFuzzCoverage(func() *Sim { return New(d) }, nil)
	plan := wf.EdgePlan{Steps: []wf.EdgeStep{{From: "coverageSource", Index: 0, To: "coverageSub", Label: "next"}, {From: "coverageSub", Index: 0, To: "coverageEnd", Label: "over"}}}
	c.Add(&FuzzResult{End: "success", Trace: &Trace{journal: []wf.JournalRecord{
		coverageRecord(wf.JournalAccepted, "", "coverageSource", wf.AcceptedData{Plan: plan}),
		coverageRecord(wf.JournalTransition, "", "coverageSource", wf.TransitionData{To: "coverageEnd"}),
	}}})
	if problems := c.EdgeProblems(); len(problems) != 0 {
		t.Fatalf("committed composite plan: %v", problems)
	}
}
