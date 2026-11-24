package wf_test

import (
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"testing"
)

type reachSource struct{}
type reachEnd struct{}

func TestLintReachabilityCountsExemptionsAndRejectsBlankReasons(t *testing.T) {
	d := wf.New("reach")
	a := wf.NewCompute[reachSource, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	end := wf.NewTerminal[reachEnd](d, wf.Success)
	good := a.To(end, "never").Unreachable("not in this deployment")
	bad := a.To(end, "blank").Unreachable(" \t")
	a.Route(func(bool, wf.Ctx) wf.Edge[reachSource] { return good }, good, bad)
	if d.UnreachableEdges() != 2 {
		t.Fatalf("exemption count %d", d.UnreachableEdges())
	}
	ps := d.LintReachability()
	if len(ps) != 1 || ps[0].Rule != wf.RuleUnreachableReasonEmpty {
		t.Fatalf("blank exemption reason: %v", ps)
	}
}
