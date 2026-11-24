package wf_test

import (
	"encoding/json"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"testing"
)

type ownedReadSeed struct{}
type ownedReadReview struct{}
type ownedReadDone struct{}

func TestEngineObservationOwnershipAndPurpose(t *testing.T) {
	d := wf.New("owned-observations")
	observation := wf.Observe[bool]("git", "head", nil)
	seed := wf.NewCompute[ownedReadSeed, bool](d, func(c wf.Ctx) (bool, error) { return wf.Observed(c, observation) }).Observes(observation)
	review := wf.NewAgent[ownedReadReview, bool](d)
	end := wf.NewTerminal[ownedReadDone](d, wf.Success)
	review.Spec(wf.PromptText("review"), wf.Retry[ownedReadReview](wf.Const(1), review.To(end, "stop")), wf.Checkf("head", func(_ bool, c wf.Ctx) error { _, err := wf.Observed(c, observation); return err }).Observes(observation))
	seed.Then(seed.To(review, "next"))
	review.Then(review.To(end, "done"))
	rt := &contextualReadRuntime{fakeRuntime: fakeRuntime{ops: []string{"git.head"}, value: json.RawMessage(`true`)}}
	h := newHarness(t, d, rt)
	run := h.run(d, nil)
	instructions := h.next(run)
	if len(instructions) != 1 {
		t.Fatalf("instructions: %+v", instructions)
	}
	if result := h.submit(run, instructions[0], `true`); result.Class != wf.Accepted {
		t.Fatalf("submit: %+v", result)
	}
	seen := map[string]bool{}
	for i, at := range rt.contexts {
		if at.Run != run || at.Instance != "" || rt.descriptors[i].Adapter != "git" || rt.descriptors[i].Op != "head" {
			t.Fatalf("wrong ownership: %+v", at)
		}
		seen[at.State+":"+at.Purpose] = true
	}
	for _, key := range []string{"ownedReadSeed:compute", "ownedReadReview:issue", "ownedReadReview:check"} {
		if !seen[key] {
			t.Fatalf("missing %s in %+v", key, rt.contexts)
		}
	}
}
