package wf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type tierA struct{}

// tierDef is one agent hinted with hints; its prompt names the run bindings.
func tierDef(prompt string, hints ...wf.DispatchOption) *wf.Def {
	d := wf.New("tiers")
	a := wf.NewAgent[tierA, string](d)
	end := wf.NewTerminal[dpEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	done := a.To(end, "done")
	a.Spec(wf.PromptText(prompt), wf.Retry(wf.Const(1), done))
	a.Route(func(string, wf.Ctx) wf.Edge[tierA] { return done }, done)
	a.Dispatch(hints...)
	return d
}

func runStartedOf(t *testing.T, h *harness, run wf.RunID) wf.RunStartedData {
	t.Helper()
	var s wf.RunStartedData
	if err := json.Unmarshal(h.kind(run, wf.JournalRunStarted)[0].Data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTieredRunJournalsModelsAndIssuesConcreteModel(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	if m := runStartedOf(t, h, run).Models; m["strong"] != "gpt-6.2-astra" {
		t.Fatalf("Models = %v, want strong=gpt-6.2-astra", m)
	}
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Dispatch.Model != "gpt-6.2-astra" || ins[0].Dispatch.Tier != wf.TierStrong {
		t.Fatalf("instruction = %+v, want model gpt-6.2-astra tier strong", ins)
	}
}

func TestResumeUsesJournaledModels(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	defer wf.SetHarnessTierModel("codex", wf.TierStrong, wf.TierModel{Model: "gpt-changed", Effort: "low"})()
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	ins, err := fresh.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Dispatch.Model != "gpt-6.2-astra" || ins[0].Dispatch.Effort != "high" {
		t.Fatalf("resumed instruction = %+v, %v, want the journaled gpt-6.2-astra at high", ins, err)
	}
}

func TestPreTierJournalResolvesAsBefore(t *testing.T) {
	d := tierDef("p", wf.Model("opus"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{ModelMap: map[string]string{"opus": "sonnet"}})
	run := h.run(d, nil)
	if m := runStartedOf(t, h, run).Models; m != nil {
		t.Fatalf("untiered run journaled Models = %v", m)
	}
	if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" || ins[0].Dispatch.Tier != "" {
		t.Fatalf("instruction = %+v", ins)
	}
}

func TestResolvedModelsAndHarnessBind(t *testing.T) {
	for harness, want := range map[string]string{"claude": "opus", "codex": "gpt-6.2-astra"} {
		d := tierDef("h={{harness}} m={{resolvedModels}}", wf.ModelTier(wf.TierStrong))
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: harness})
		run := h.run(d, nil)
		ins := h.next(run)
		if len(ins) != 1 {
			t.Fatalf("%s: instructions = %+v", harness, ins)
		}
		wantText := "h=" + harness + ` m={"strong":"` + want + `"}`
		if !strings.Contains(ins[0].Prompt, wantText) {
			t.Errorf("%s: prompt = %q, want it to contain %q", harness, ins[0].Prompt, wantText)
		}
	}
}

// A3c: a tier the run's journaled models lack is a hint: the instruction is
// issued with no model, and the child runs on the session's model.
func TestTierMissingFromJournaledModelsIssuesWithNoModel(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	// A hand-written run_started whose Models lack the state's tier.
	unlock, err := h.j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(wf.RunStartedData{Def: "tiers", Harness: "codex", Models: map[string]string{"light": "x"}})
	err = h.j.Append(wf.JournalRecord{Kind: wf.JournalRunStarted, Run: "old", DefHash: d.Hash(), At: h.now, Data: data})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	ins, err := h.e.Next("old", "")
	if err != nil {
		t.Fatal(err)
	}
	if ins = withoutEnded(ins); len(ins) != 1 || ins[0].Dispatch.Model != "" || ins[0].Dispatch.Tier != wf.TierStrong {
		t.Fatalf("instructions = %+v, want one with the tier and no model", ins)
	}
}

func TestChainedModelMapAppliedOnceToTiers(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "claude", ModelMap: map[string]string{"opus": "sonnet", "sonnet": "haiku"}})
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" || runStartedOf(t, h, run).Models["strong"] != "sonnet" {
		t.Fatalf("instruction = %+v, want strong -> opus -> sonnet, once", ins)
	}
	u := tierDef("p", wf.Model("opus"))
	h = newHarnessOpts(t, u, nil, wf.EngineOptions{Harness: "claude", ModelMap: map[string]string{"opus": "sonnet", "sonnet": "haiku"}})
	if ins := h.next(h.run(u, nil)); len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" {
		t.Fatalf("untiered instruction = %+v, want the single-pass rewrite", ins)
	}
}

func TestChildOfTieredParentUsesParentModels(t *testing.T) {
	d := subDef("parent", chChild(wf.ModelTier(wf.TierStrong)))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	run := h.run(d, nil)
	if runStartedOf(t, h, run).Models["strong"] != "gpt-6.2-astra" {
		t.Fatal("parent run_started lacks the child's tier")
	}
	defer wf.SetHarnessTierModel("codex", wf.TierStrong, wf.TierModel{Model: "gpt-changed", Effort: "high"})()
	ins := h.next(run)
	if len(ins) != 1 || ins[0].Dispatch.Model != "gpt-6.2-astra" || ins[0].Dispatch.Tier != wf.TierStrong {
		t.Fatalf("child instruction = %+v, want the parent's journaled gpt-6.2-astra", ins)
	}
}

// A3c: run creation never refuses for a missing tier map; the run journals
// a degraded record saying the tiers were not applied.
func TestRunWithUnresolvedTiersSaysTheyWereNotApplied(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong), wf.Model("opus"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "generic"})
	run, err := h.e.Run(d, nil, nil)
	if err != nil {
		t.Fatalf("Run = %v, want a run", err)
	}
	ds := h.kind(run, wf.JournalDegraded)
	if len(ds) != 1 || !strings.Contains(string(ds[0].Data), "strong") || !strings.Contains(string(ds[0].Data), "not applied") {
		t.Fatalf("degraded = %+v, want one naming tier strong as not applied", ds)
	}
}

func TestScriptedRunsCarryTierEffortPerHarness(t *testing.T) {
	for _, c := range []struct{ harness, model, effort string }{
		{"codex", "gpt-6.2-astra", "high"},
		{"claude", "opus", "high"},
	} {
		d := tierDef("p", wf.ModelTier(wf.TierStrong))
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: c.harness})
		run := h.run(d, nil)
		if e := runStartedOf(t, h, run).Efforts["strong"]; e != c.effort {
			t.Errorf("%s: journaled effort = %q, want %s", c.harness, e, c.effort)
		}
		ins := h.next(run)
		if len(ins) != 1 || ins[0].Dispatch.Model != c.model || ins[0].Dispatch.Effort != c.effort {
			t.Errorf("%s: instruction = %+v, want model %s effort %s", c.harness, ins, c.model, c.effort)
		}
	}
}

func TestHarnessWithoutEffortOmitsItAndJournalsHarnessDefault(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "generic", ModelMap: map[string]string{"strong": "m1"}})
	run := h.run(d, nil)
	if e := runStartedOf(t, h, run).Efforts["strong"]; e != "harness default" {
		t.Fatalf("journaled effort = %q, want harness default", e)
	}
	if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Model != "m1" || ins[0].Dispatch.Effort != "" {
		t.Fatalf("instruction = %+v, want m1 with no effort", ins)
	}
}

func TestModelMapOverridesTierEffort(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"strong.effort": "xhigh"}})
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Model != "gpt-6.2-astra" || ins[0].Dispatch.Effort != "xhigh" {
		t.Fatalf("instruction = %+v, want gpt-6.2-astra at xhigh", ins)
	}
	h = newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"strong.effort": "harness default"}})
	run = h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Effort != "" {
		t.Fatalf("instruction = %+v, want effort cleared", ins)
	}
}

func TestModelMapKeepsRowEffortForTierAndModelKeys(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	for _, mm := range []map[string]string{{"strong": "sonnet"}, {"opus": "sonnet"}} {
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "claude", ModelMap: mm})
		run := h.run(d, nil)
		if e := runStartedOf(t, h, run).Efforts["strong"]; e != "high" {
			t.Errorf("%v: journaled effort = %q, want the row's high", mm, e)
		}
		if ins := h.next(run); len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" || ins[0].Dispatch.Effort != "high" {
			t.Errorf("%v: instruction = %+v, want sonnet at the row's high", mm, ins)
		}
	}
}

func TestRunRefusesInvalidTierEffortBeforeJournaling(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"strong.effort": "huge"}})
	_, err := h.e.Run(d, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "strong.effort") || !strings.Contains(err.Error(), "huge") {
		t.Fatalf("Run = %v, want an error naming strong.effort=huge", err)
	}
	if runs, _ := h.j.Runs(); len(runs) != 0 {
		t.Fatalf("runs journaled = %v, want none", runs)
	}
}

func TestStateEffortWinsOverTierEffort(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong), wf.Effort("low"))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	if ins := h.next(h.run(d, nil)); len(ins) != 1 || ins[0].Dispatch.Effort != "low" {
		t.Fatalf("instruction = %+v, want the state's low over the tier's high", ins)
	}
	u := tierDef("p", wf.ModelTier(wf.TierStrong))
	h = newHarnessOpts(t, u, nil, wf.EngineOptions{Harness: "codex"})
	if ins := h.next(h.run(u, nil)); len(ins) != 1 || ins[0].Dispatch.Effort != "high" {
		t.Fatalf("instruction = %+v, want the tier's high", ins)
	}
}

func TestPlainStringModelsJournalLoadsAtHarnessDefaultEffort(t *testing.T) {
	d := tierDef("p", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{})
	unlock, err := h.j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(wf.RunStartedData{Def: "tiers", Harness: "codex", Models: map[string]string{"strong": "plain-model"}})
	err = h.j.Append(wf.JournalRecord{Kind: wf.JournalRunStarted, Run: "old", DefHash: d.Hash(), At: h.now, Data: data})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	ins, err := h.e.Next("old", "")
	if err != nil || len(ins) != 1 || ins[0].Dispatch.Model != "plain-model" || ins[0].Dispatch.Effort != "" {
		t.Fatalf("instruction = %+v, %v, want plain-model with no effort", ins, err)
	}
}

func TestResolvedModelsKeepsFlatStringShape(t *testing.T) {
	d := tierDef("m={{resolvedModels}}", wf.ModelTier(wf.TierStrong))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "codex"})
	ins := h.next(h.run(d, nil))
	if len(ins) != 1 || !strings.Contains(ins[0].Prompt, `m={"strong":"gpt-6.2-astra"}`) {
		t.Fatalf("prompt = %+v, want the flat tier -> model string map, no effort", ins)
	}
}
