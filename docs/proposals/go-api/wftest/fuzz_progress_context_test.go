package wftest_test

import (
	"encoding/json"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wftest"
	"strings"
	"testing"
)

type contextPlan struct{}
type contextReview struct{}
type contextTask struct {
	ID string `json:"id"`
}

func progressContextFactory() *wftest.Sim {
	d := wf.New("progress-context")
	plan := wf.NewAgent[contextPlan, contextTask](d)
	review := wf.NewAgent[contextReview, contextTask](d)
	done := wf.NewTerminal[fuzzDone](d, wf.Success)
	stop := wf.NewTerminal[fuzzStop](d, wf.Stopped)
	plan.Spec(wf.PromptText("plan"), wf.Retry[contextPlan](wf.Const(1), plan.To(stop, "no plan")))
	plan.Then(plan.To(review, "review"))
	review.Spec(wf.PromptText("review"), wf.Retry[contextReview](wf.Const(1), review.To(stop, "no review")), wf.Checkf("same task", func(v contextTask, c wf.Ctx) error {
		if v.ID != wf.OutputOf(c, plan).ID {
			return fmt.Errorf("review id %q does not name plan task %q", v.ID, wf.OutputOf(c, plan).ID)
		}
		return nil
	}))
	review.Then(review.To(done, "done"))
	return wftest.New(d)
}
func progressContextConfig() wftest.FuzzConfig {
	corpus := map[string][]json.RawMessage{"contextPlan": {json.RawMessage(`{"id":"add"}`)}, "contextReview": {json.RawMessage(`{"id":"add"}`)}}
	return wftest.FuzzConfig{MaxSteps: 12, Liveness: true, LivenessAfter: 1, FailPercent: -1, SchemaPercent: 100, Valid: corpus, Progress: corpus}
}

func TestLivenessPrefixPreservesCorpusContext(t *testing.T) {
	for seed := int64(0); seed < 8; seed++ {
		cfg := progressContextConfig()
		cfg.Seed = seed
		got := wftest.Fuzz(progressContextFactory, cfg)
		if got.Finding != nil || got.Status.Outcome != wf.Success {
			t.Fatalf("seed%d generated incompatible prefix: %s", seed, got)
		}
		if got.Mix["schema"] != 0 {
			t.Fatalf("liveness prefix drew schema: %s", got)
		}
		if again := got.Replay(progressContextFactory); again.Finding != nil || again.Status.Outcome != wf.Success {
			t.Fatalf("replay: %s", again)
		}
	}
}
func TestMixedSimulationStillDrawsSchema(t *testing.T) {
	cfg := progressContextConfig()
	cfg.Liveness = false
	got := wftest.Fuzz(progressContextFactory, cfg)
	if got.Mix["schema"] == 0 {
		t.Fatalf("ordinary mixed exploration constrained: %s", got)
	}
}
func TestExplicitIncompatibleProgressIsNotLiveness(t *testing.T) {
	cfg := progressContextConfig()
	cfg.Prefix = []wftest.FuzzAction{{State: "contextPlan", Kind: wftest.FuzzSubmit, Output: json.RawMessage(`{"id":"a"}`)}}
	got := wftest.Fuzz(progressContextFactory, cfg)
	if got.Finding == nil || got.Finding.Rule != "progress-context" || !strings.Contains(got.Finding.Why, "does not name plan task") {
		t.Fatalf("invalid all-good premise called liveness: %s", got)
	}
	if again := got.Replay(progressContextFactory); again.Finding == nil || again.Finding.Rule != "progress-context" {
		t.Fatalf("replay lost context failure: %s", again)
	}
}
func TestLivenessPrefixDoesNotFallBackToSchema(t *testing.T) {
	cfg := progressContextConfig()
	cfg.Valid = nil
	cfg.Progress = nil
	got := wftest.Fuzz(progressContextFactory, cfg)
	if got.Finding == nil || got.Finding.Rule != "fixture-gap" || got.Mix["schema"] != 0 {
		t.Fatalf("missing corpus generated prefix: %s", got)
	}
}

// A bounded adverse prelude is not an all-good phase. The checker separately
// fails the target coverage requirement if the selected fault is never reached.
func TestUnreachedFaultPreludeIsNotLivenessFailure(t *testing.T) {
	got := wftest.Fuzz(fuzzSimpleFactory(false), wftest.FuzzConfig{MaxSteps: 2, OutcomeSwitches: true, Fault: &wftest.FuzzFault{State: "Never", Class: "effect-adverse", Adapter: "test", Op: "write"}, Prefix: []wftest.FuzzAction{{Kind: wftest.FuzzWait}, {Kind: wftest.FuzzWait}}})
	if got.Finding != nil || !got.PrefixOutcome || got.FaultInjected {
		t.Fatalf("unreached fault claimed liveness: %s", got)
	}
}
