package wftest_test

import (
	"slices"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
)

// crash at gate_offered: the engine dies right after a person gate's offer is
// journaled. On reopen the gate is presented again with no second offer for
// the visit, the person's answer lands, and the journal is the uncrashed one.
func init() {
	pass := Draft{Verdict: Pass}
	sc := loopScenario(false, nil, func(crash bool, draft *wf.Agent[DraftS, Draft], approve *wf.Gate[ApproveS, Yes]) []wftest.Step {
		return append(after(crash, wf.JournalGateOffered, wftest.Submit(draft, pass)), wftest.Answer(approve, Yes("yes")))
	})
	point := simPoint(wf.JournalGateOffered, sc)
	crashPoints[wf.JournalGateOffered] = func(t *testing.T) {
		point(t)
		kinds := func(crash bool) []wf.JournalKind {
			d, opts, steps := sc(t, crash)
			var ks []wf.JournalKind
			for _, r := range wftest.New(d, append(opts, wftest.WithTB(t))...).Run(steps...).Journal() {
				ks = append(ks, r.Kind)
			}
			return ks
		}
		want, got := kinds(false), kinds(true)
		if n := countKind(want, wf.JournalGateOffered); n != 1 {
			t.Fatalf("the uncrashed run offered the gate %d times, want 1: %v", n, want)
		}
		if !slices.Equal(got, want) {
			t.Errorf("after a crash at gate_offered the journal is\n%v\nwant\n%v", got, want)
		}
	}
}

func countKind(ks []wf.JournalKind, k wf.JournalKind) int {
	n := 0
	for _, x := range ks {
		if x == k {
			n++
		}
	}
	return n
}
