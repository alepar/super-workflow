package wf_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type (
	dpA   struct{}
	dpEnd struct{}
)

func dispatchDef(opts ...wf.DispatchOption) *wf.Def {
	d := wf.New("dp")
	a := wf.NewAgent[dpA, string](d)
	end := wf.NewTerminal[dpEnd](d, wf.Success)
	d.OnFailure(wf.Blocked, end)
	done := a.To(end, "done")
	a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done))
	a.Route(func(string, wf.Ctx) wf.Edge[dpA] { return done }, done)
	a.Dispatch(opts...)
	return d
}

func TestDispatchHintsShowEverywhere(t *testing.T) {
	d := dispatchDef(wf.Model("opus"), wf.Effort("high"), wf.AgentType("implementer"))
	const want = "model=opus effort=high agent-type=implementer"
	if got := wf.Inspect(d).States[0].Dispatch; got != want {
		t.Errorf("Inspect Dispatch = %q, want %q", got, want)
	}
	if !strings.Contains(d.Render(), "dispatch: "+want+"\n") {
		t.Errorf("render lacks the dispatch line:\n%s", d.Render())
	}
	if d.Hash() == dispatchDef().Hash() {
		t.Error("hints do not change the hash")
	}
	if ps := d.Lint(); len(ps) != 0 {
		t.Errorf("valid hints lint: %v", ps)
	}
}

func TestDispatchHintOrderIsCanonical(t *testing.T) {
	a := dispatchDef(wf.Model("opus"), wf.Effort("high"))
	b := dispatchDef(wf.Effort("high"), wf.Model("opus"))
	if a.Hash() != b.Hash() {
		t.Error("option order changes the hash")
	}
}

func TestNoHintsAddNothing(t *testing.T) {
	d := dispatchDef()
	if got := wf.Inspect(d).States[0].Dispatch; got != "" {
		t.Errorf("Inspect Dispatch = %q, want empty", got)
	}
	if strings.Contains(d.Render(), "dispatch:") {
		t.Errorf("render shows a dispatch line:\n%s", d.Render())
	}
}

func TestRuleDispatchHint(t *testing.T) {
	for name, opts := range map[string][]wf.DispatchOption{
		"empty model":      {wf.Model("")},
		"empty agent type": {wf.AgentType("")},
		"bad effort":       {wf.Effort("huge")},
		"repeated option":  {wf.Model("opus"), wf.Model("sonnet")},
		"zero stall-after": {wf.StallAfter(0)},
		"negative stall":   {wf.StallAfter(-time.Second)},
		"repeated stall":   {wf.StallAfter(time.Minute), wf.StallAfter(2 * time.Minute)},
	} {
		t.Run(name, func(t *testing.T) {
			for _, p := range dispatchDef(opts...).Lint() {
				if p.Rule == wf.RuleDispatchHint {
					return
				}
			}
			t.Fatal("RuleDispatchHint did not fire")
		})
	}
}

func TestEffortLevelsAccepted(t *testing.T) {
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		if ps := dispatchDef(wf.Effort(e)).Lint(); len(ps) != 0 {
			t.Errorf("effort %s: %v", e, ps)
		}
	}
}

func TestInstructionCarriesHintsAndWorkspace(t *testing.T) {
	i := wf.Instruction{Dispatch: wf.DispatchHints{Model: "m", Effort: "low", AgentType: "t"}, Workspace: "/w"}
	if i.Dispatch.Model != "m" || i.Workspace != "/w" {
		t.Fatalf("%+v", i)
	}
}

const goldenDispatchHash = "sha256:e4a2517f84592d5f"

func TestStallAfterRendersOnDispatchLine(t *testing.T) {
	d := dispatchDef(wf.Model("sonnet"), wf.StallAfter(5*time.Minute))
	const want = "model=sonnet stall-after=5m0s"
	if got := wf.Inspect(d).States[0].Dispatch; got != want {
		t.Errorf("Inspect Dispatch = %q, want %q", got, want)
	}
	if !strings.Contains(d.Render(), "dispatch: "+want+"\n") {
		t.Errorf("render lacks the dispatch line:\n%s", d.Render())
	}
	if d.Hash() == dispatchDef(wf.Model("sonnet")).Hash() {
		t.Error("StallAfter does not change the hash")
	}
}

// TestNoStallAfterUnchanged pins the dispatch line and hash of a definition
// that does not use StallAfter to literals captured before the hint existed.
func TestNoStallAfterUnchanged(t *testing.T) {
	d := dispatchDef(wf.Model("opus"), wf.Effort("high"), wf.AgentType("implementer"))
	if got := wf.Inspect(d).States[0].Dispatch; got != "model=opus effort=high agent-type=implementer" {
		t.Errorf("dispatch line = %q", got)
	}
	if got := d.Hash(); got != goldenDispatchHash {
		t.Errorf("Hash = %s, want %s", got, goldenDispatchHash)
	}
}

func TestTierHintShowsEverywhere(t *testing.T) {
	d := dispatchDef(wf.Model("opus"), wf.ModelTier(wf.TierStrong), wf.StallAfter(time.Minute))
	const want = "model=opus tier=strong stall-after=1m0s"
	if got := wf.Inspect(d).States[0].Dispatch; got != want {
		t.Errorf("Inspect Dispatch = %q, want %q", got, want)
	}
	if d.Hash() == dispatchDef(wf.Model("opus"), wf.StallAfter(time.Minute)).Hash() {
		t.Error("tier does not change the hash")
	}
	for _, tier := range []wf.Tier{wf.TierLight, wf.TierStandard, wf.TierStrong, wf.TierTop} {
		if ps := dispatchDef(wf.ModelTier(tier)).Lint(); len(ps) != 0 {
			t.Errorf("tier %q lints: %v", tier, ps)
		}
	}
	if ps := dispatchDef(wf.ModelTier("huge")).Lint(); len(ps) == 0 {
		t.Error("unknown tier does not lint")
	}
}

func TestGitAuthorityCarriedOnlyWhenDeclared(t *testing.T) {
	g := wf.GitAuthority{Refs: []string{"task-branch"}, Allowed: []string{"commit"}, Forbidden: []string{"push"}, Section: "git-authority"}
	for _, tc := range []struct {
		name string
		set  bool
	}{{"set", true}, {"unset", false}} {
		d := wf.New("ga")
		a := wf.NewAgent[dpA, string](d)
		end := wf.NewTerminal[dpEnd](d, wf.Success)
		done := a.To(end, "done")
		a.Spec(wf.PromptText("p"), wf.Retry(wf.Const(1), done))
		a.Route(func(string, wf.Ctx) wf.Edge[dpA] { return done }, done)
		if tc.set {
			a.GitAuthority(g)
		}
		h := newHarness(t, d, nil)
		ins := h.next(h.run(d, nil))
		if len(ins) != 1 {
			t.Fatalf("%s: instructions = %+v", tc.name, ins)
		}
		raw, err := json.Marshal(ins[0])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		got, has := m["gitAuthority"]
		if !tc.set {
			if has || ins[0].GitAuthority != nil {
				t.Errorf("unset: gitAuthority present: %s", raw)
			}
			continue
		}
		const want = `{"refs":["task-branch"],"allowed":["commit"],"forbidden":["push"],"section":"git-authority"}`
		if string(got) != want {
			t.Errorf("set: gitAuthority = %s, want %s", got, want)
		}
	}
}
