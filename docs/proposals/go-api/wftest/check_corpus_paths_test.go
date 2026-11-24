package wftest

import (
	"encoding/json"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type corpusStart struct{}
type corpusReview struct{}
type corpusEnd struct{}
type corpusCap struct{}
type corpusChild struct{}

func TestCorpusPathsPreservePrefixAndRepeatNegativeResult(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("corpus-path")
		start := wf.NewAgent[corpusStart, bool](d)
		review := wf.NewAgent[corpusReview, bool](d)
		end := wf.NewTerminal[corpusEnd](d, wf.Success)
		cap := wf.NewTerminal[corpusCap](d, wf.Stopped)
		d.OnFailure(wf.Blocked, cap)
		start.Spec(wf.PromptText("start"), wf.Retry[corpusStart](wf.Const(1), start.To(cap, "invalid prefix")))
		start.Then(start.To(review, "ready"))
		review.Spec(wf.PromptText("review"), wf.Retry[corpusReview](wf.Const(1), review.To(cap, "invalid review")))
		good, bad := review.To(end, "approved"), review.To(start, "repair")
		review.Route(func(v bool, _ wf.Ctx) wf.Edge[corpusReview] {
			if v {
				return good
			}
			return bad
		}, good, bad)
		wf.MaxVisits(wf.Const(2), review.To(cap, "review cap"))
		return New(d)
	}
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 20,
		Valid:    map[string][]json.RawMessage{"corpusReview": {json.RawMessage(`false`)}},
		Progress: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}, "corpusReview": {json.RawMessage(`true`)}},
	}}
	coverage := NewFuzzCoverage(factory, nil)
	count := checkCorpusPaths(factory, cfg, func(r *FuzzResult) {
		if r.Finding != nil {
			t.Fatal(r.Finding)
		}
		coverage.Add(r)
	})
	if count != 1 || coverage.EdgesReached["corpus-path/corpusReview/edge[2]"] == 0 || coverage.EdgesReached["corpus-path/corpusReview/edge[3]"] == 0 {
		t.Fatalf("negative route and visit cap not covered: runs=%d edges=%v", count, coverage.EdgesReached)
	}
}

func TestAgentPathsExhaustRefusalsAtTargetState(t *testing.T) {
	factory := func() *Sim {
		d := wf.New("agent-path")
		start := wf.NewAgent[corpusStart, bool](d)
		review := wf.NewAgent[corpusReview, bool](d)
		end := wf.NewTerminal[corpusEnd](d, wf.Success)
		cap := wf.NewTerminal[corpusCap](d, wf.Stopped)
		d.OnFailure(wf.Blocked, cap)
		start.Spec(wf.PromptText("start"), wf.Retry[corpusStart](wf.Const(2), start.To(cap, "start exhausted")))
		start.Then(start.To(review, "ready"))
		review.Spec(wf.PromptText("review"), wf.Retry[corpusReview](wf.Const(2), review.To(cap, "review exhausted")))
		review.Then(review.To(end, "approved"))
		return New(d)
	}
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 20, Progress: map[string][]json.RawMessage{"corpusStart": {json.RawMessage(`true`)}, "corpusReview": {json.RawMessage(`true`)}}}}
	coverage := NewFuzzCoverage(factory, nil)
	count := checkAgentPaths(factory, cfg, func(r *FuzzResult) {
		if r.Finding != nil {
			t.Fatal(r.Finding)
		}
		coverage.Add(r)
	})
	if count != 8 || coverage.EdgesReached["agent-path/corpusReview/edge[0]"] == 0 {
		t.Fatalf("targeted retry exhaustion missing: runs=%d edges=%v", count, coverage.EdgesReached)
	}
}

func TestCorpusPathsLeafSampleOverridesQualifiedProgress(t *testing.T) {
	factory := func() *Sim {
		child := wf.New("child")
		review := wf.NewAgent[corpusReview, bool](child)
		yes := wf.NewTerminal[corpusEnd](child, wf.Success)
		no := wf.NewTerminal[corpusCap](child, wf.Stopped)
		child.OnFailure(wf.Blocked, no)
		review.Spec(wf.PromptText("review"), wf.Retry[corpusReview](wf.Const(1), review.To(no, "exhausted")))
		approved, declined := review.To(yes, "approved"), review.To(no, "declined").Ends("negative review")
		review.Route(func(value bool, _ wf.Ctx) wf.Edge[corpusReview] {
			if value {
				return approved
			}
			return declined
		}, approved, declined)
		wf.Yield(yes, func(wf.Ctx) bool { return true })
		wf.Yield(no, func(wf.Ctx) bool { return false })
		d := wf.New("parent")
		sub := wf.NewSub[corpusChild](d, wf.Typed[struct{}, bool](child), func(wf.Ctx) struct{} { return struct{}{} })
		end := wf.NewTerminal[corpusEnd](d, wf.Success)
		sub.Exits(wf.OnExit[corpusEnd](sub.To(end, "approved")), wf.OnExit[corpusCap](sub.To(end, "declined")))
		return New(d)
	}
	cfg := CheckConfig{CalibrationConfig: CalibrationConfig{MaxSteps: 10,
		Valid:    map[string][]json.RawMessage{"corpusReview": {json.RawMessage(`false`)}},
		Progress: map[string][]json.RawMessage{"child/corpusReview": {json.RawMessage(`true`)}},
	}}
	coverage := NewFuzzCoverage(factory, nil)
	checkCorpusPaths(factory, cfg, func(r *FuzzResult) {
		if r.Finding != nil {
			t.Fatal(r.Finding)
		}
		coverage.Add(r)
	})
	if coverage.EdgesReached["parent/child/corpusReview/edge[2]"] == 0 {
		t.Fatalf("leaf-key sample shadowed by qualified progress: %v", coverage.EdgesReached)
	}
}
