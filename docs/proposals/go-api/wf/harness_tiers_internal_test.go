package wf

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHarnessTiersRows(t *testing.T) {
	if _, ok := harnessTiers()["generic"]; ok {
		t.Fatal("generic must have no tier row")
	}
	want := map[Tier]TierModel{TierLight: {"haiku", "low"}, TierStandard: {"sonnet", "medium"}, TierStrong: {"opus", "high"}, TierTop: {"fable", "high"}}
	if !reflect.DeepEqual(harnessTiers()["claude"], want) {
		t.Fatalf("claude row = %v", harnessTiers()["claude"])
	}
	for _, h := range []string{"claude", "codex"} {
		models, un, _ := resolveTiers(h, dispatchTiers, nil)
		if len(un) != 0 || len(models) != len(dispatchTiers) {
			t.Fatalf("%s: models=%v unresolved=%v", h, models, un)
		}
	}
	for tier, m := range harnessTiers()["codex"] {
		if !strings.HasPrefix(m.Model, "gpt-") {
			t.Fatalf("codex %s = %q, want a GPT id", tier, m)
		}
	}
	wantAgy := map[Tier]TierModel{}
	for _, tier := range dispatchTiers {
		wantAgy[tier] = TierModel{Model: "inherit"}
	}
	if !reflect.DeepEqual(harnessTiers()["agy"], wantAgy) {
		t.Fatalf("agy row = %v", harnessTiers()["agy"])
	}
	for _, h := range []string{"pi", "hermes"} {
		if _, ok := harnessTiers()[h]; ok {
			t.Fatalf("%s must have no tier row", h)
		}
	}
}

func TestResolveTiersMapPrecedence(t *testing.T) {
	tiers := []Tier{TierLight, TierStandard}
	// tier key replaces the table model
	m, _, _ := resolveTiers("claude", tiers, map[string]string{"light": "x"})
	if m["light"].Model != "x" || m["standard"].Model != "sonnet" {
		t.Fatalf("tier key: %v", m)
	}
	// model key replaces the table model
	m, _, _ = resolveTiers("claude", tiers, map[string]string{"sonnet": "y"})
	if m["standard"].Model != "y" || m["light"].Model != "haiku" {
		t.Fatalf("model key: %v", m)
	}
	// both match the same tier: tier key wins
	m, _, _ = resolveTiers("claude", tiers, map[string]string{"standard": "t", "sonnet": "mdl"})
	if m["standard"].Model != "t" {
		t.Fatalf("tier key must win: %v", m)
	}
	// chained: applied once only
	m, _, _ = resolveTiers("claude", tiers, map[string]string{"sonnet": "a", "a": "b", "standard": "standard2", "standard2": "z"})
	if m["standard"].Model != "standard2" {
		t.Fatalf("tier chain: %v", m)
	}
	m, _, _ = resolveTiers("claude", tiers, map[string]string{"sonnet": "a", "a": "b"})
	if m["standard"].Model != "a" {
		t.Fatalf("model chain: %v", m)
	}
	// a tier key resolves a harness with no row
	m, un, _ := resolveTiers("generic", tiers, map[string]string{"light": "g"})
	if m["light"].Model != "g" || !reflect.DeepEqual(un, []Tier{TierStandard}) {
		t.Fatalf("generic: %v %v", m, un)
	}
}

func agentWithTier(d *Def, t Tier) {
	a := NewAgent[struct{}, struct{}](d)
	a.Dispatch(ModelTier(t))
}

func TestCollectTiersIncludesChildren(t *testing.T) {
	child := New("child")
	agentWithTier(child, TierTop)
	parent := New("parent")
	agentWithTier(parent, TierLight)
	parent.children = append(parent.children, child, child)
	got := collectTiers(parent)
	if !reflect.DeepEqual(got, []Tier{TierLight, TierTop}) {
		t.Fatalf("tiers = %v", got)
	}
}

func TestCollectTiersIncludesGrandchildren(t *testing.T) {
	grandchild := New("grandchild")
	agentWithTier(grandchild, TierStrong)
	child := New("child")
	child.children = append(child.children, grandchild)
	parent := New("parent")
	agentWithTier(parent, TierLight)
	parent.children = append(parent.children, child)
	if got := collectTiers(parent); !reflect.DeepEqual(got, []Tier{TierLight, TierStrong}) {
		t.Fatalf("tiers = %v", got)
	}
}

func TestModelTiersCheck(t *testing.T) {
	d := New("d")
	agentWithTier(d, TierStandard)
	agentWithTier(d, TierTop)
	check := modelTiersCheck(d)
	if check.Name != "model-tiers" {
		t.Fatal(check.Name)
	}
	for _, h := range []string{"claude", "codex"} {
		if r := check.Run(PreflightContext{Harness: h}); r != nil {
			t.Fatalf("%s refused: %+v", h, r)
		}
	}
	r0 := check.Run(PreflightContext{Harness: "agy", ModelMap: map[string]string{"light": "gemini-3.8-flash-high"}})
	if r0 == nil || !strings.Contains(r0.Cause, "light=gemini-3.8-flash-high") || !strings.Contains(r0.Cause, "inherit, flash_lite, flash, pro") {
		t.Fatalf("agy model id refusal = %+v", r0)
	}
	if r := check.Run(PreflightContext{Harness: "agy", ModelMap: map[string]string{"light": "flash"}}); r != nil {
		t.Fatalf("agy flash refused: %+v", r)
	}
	// A3c: tiers are hints; a harness with no map is never refused.
	if r := check.Run(PreflightContext{Harness: "generic"}); r != nil {
		t.Fatalf("generic refused: %+v", r)
	}
	if r := check.Run(PreflightContext{Harness: "generic", ModelMap: map[string]string{"standard": "a", "top": "b"}}); r != nil {
		t.Fatalf("mapped generic refused: %+v", r)
	}
	r := check.Run(PreflightContext{Harness: "codex", ModelMap: map[string]string{"top.effort": "huge"}})
	if r == nil || !strings.Contains(r.Cause, "top.effort") {
		t.Fatalf("invalid effort refusal = %+v", r)
	}
	if r := check.Run(PreflightContext{Harness: "codex", ModelMap: map[string]string{"top.effort": EffortHarnessDefault}}); r != nil {
		t.Fatalf("harness default effort refused: %+v", r)
	}
	if r := modelTiersCheck(New("none")).Run(PreflightContext{Harness: "generic"}); r != nil {
		t.Fatalf("untiered refused: %+v", r)
	}
}

// an earlier revision: all=<model> pins every tier, on a harness with a table or without;
// a tier key still wins over it, and a model key no longer reaches a tier.
// all.effort sets every tier's effort; a <tier>.effort key wins over it.
func TestResolveTiersAll(t *testing.T) {
	for _, h := range []string{"claude", "codex", "generic", "hermes"} {
		m, un, err := resolveTiers(h, dispatchTiers, map[string]string{"all": "gpt-6.1-terra"})
		if err != nil || len(un) != 0 {
			t.Fatalf("%s: unresolved=%v err=%v", h, un, err)
		}
		for _, tier := range dispatchTiers {
			if m[string(tier)].Model != "gpt-6.1-terra" {
				t.Fatalf("%s: %s = %v, want gpt-6.1-terra", h, tier, m[string(tier)])
			}
		}
	}
	m, _, _ := resolveTiers("codex", dispatchTiers, map[string]string{"all": "terra", "top": "astra", "gpt-6-luna": "luna2"})
	if m["top"].Model != "astra" || m["light"].Model != "terra" || m["strong"].Effort != "high" {
		t.Fatalf("all with a tier key and a model key: %v", m)
	}
	m, _, _ = resolveTiers("codex", dispatchTiers, map[string]string{"all.effort": "low", "top.effort": "xhigh"})
	if m["light"].Effort != "low" || m["strong"].Effort != "low" || m["top"].Effort != "xhigh" || m["strong"].Model != "gpt-6.2-astra" {
		t.Fatalf("all.effort: %v", m)
	}
}

// an earlier revision: a --model-map key is a tier, all, their .effort forms, or a model
// some harness table or the definition's own untiered hints name; anything
// else is refused with the valid keys listed.
func TestCheckModelMapKeys(t *testing.T) {
	d := New("keys")
	a := NewAgent[struct{}, struct{}](d)
	a.Dispatch(Model("my-own-model"))
	for _, ok := range []map[string]string{
		nil,
		{"light": "x", "standard": "x", "strong": "x", "top": "x"},
		{"all": "gpt-6.1-terra", "all.effort": "low", "top.effort": EffortHarnessDefault},
		{"gpt-6.2-astra": "gpt-6.1-terra", "opus": "sonnet", "my-own-model": "y"},
	} {
		if err := checkModelMap(d, ok); err != nil {
			t.Fatalf("%v refused: %v", ok, err)
		}
	}
	for key, m := range map[string]map[string]string{
		"lite":        {"lite": "x"},
		"gpt-7":       {"gpt-7": "x"},
		"lite.effort": {"lite.effort": "low"},
		"all.effort":  {"all.effort": "huge"},
	} {
		err := checkModelMap(d, m)
		if err == nil || !errors.Is(err, errBadModelMap) || !strings.Contains(err.Error(), key) {
			t.Fatalf("%v = %v, want refused naming %q", m, err, key)
		}
		if key != "all.effort" && !strings.Contains(err.Error(), "light, standard, strong, top, all") || key != "all.effort" && !strings.Contains(err.Error(), "gpt-6.2-astra") || key != "all.effort" && !strings.Contains(err.Error(), "my-own-model") {
			t.Fatalf("%v: the refusal does not list the valid keys: %v", m, err)
		}
	}
}

// an earlier revision: the --model-map check is split. checkModelMap, at every verb
// entry, checks keys and efforts only; a value is checked once, by
// checkModelValues inside resolveTiers, against the harness that binds.
func TestModelMapChecksSplit(t *testing.T) {
	d := New("agy-values")
	agentWithTier(d, TierLight)
	bad := map[string]string{"light": "gemini-3.8-flash-high"}
	if err := checkModelMap(d, bad); err != nil {
		t.Fatalf("a value is not checked at the verb: %v", err)
	}
	if err := checkModelMap(d, map[string]string{"lite": "x"}); !errors.Is(err, errBadModelMap) {
		t.Fatalf("an unknown key = %v, want errBadModelMap", err)
	}
	if err := checkModelMap(d, map[string]string{"all.effort": "huge"}); !errors.Is(err, errBadModelMap) {
		t.Fatalf("a bad effort = %v, want errBadModelMap", err)
	}
	err := checkModelValues("agy", bad)
	if !errors.Is(err, errBadModelMap) {
		t.Fatalf("err = %v, want errBadModelMap", err)
	}
	for _, want := range []string{"light", "gemini-3.8-flash-high", "inherit, flash_lite, flash, pro"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%v does not name %q", err, want)
		}
	}
	for _, ok := range []map[string]string{
		{"light": "flash"},
		{"all": "pro"},
		{"all": "inherit", "top.effort": EffortHarnessDefault},
		{"inherit": "flash"},
	} {
		if err := checkModelValues("agy", ok); err != nil {
			t.Fatalf("%v refused: %v", ok, err)
		}
	}
	if err := checkModelValues("agy", map[string]string{"inherit": "opus"}); err == nil || !strings.Contains(err.Error(), "inherit=opus") {
		t.Fatalf("inherit=opus = %v", err)
	}
	for _, h := range []string{"claude", "codex", "generic", ""} {
		if err := checkModelValues(h, bad); err != nil {
			t.Fatalf("%q refused: %v", h, err)
		}
	}
	// resolveSegment returns the refusal, never a models-less value a binding
	// would journal.
	c := &cycle{st: &runState{def: d}}
	sm, err := c.resolveSegment("agy", bad, ModelsFromMap)
	if !errors.Is(err, errBadModelMap) || sm.ModelMap != nil || sm.Models != nil || sm.From != "" {
		t.Fatalf("resolveSegment(agy, bad) = %+v, %v; want zero and errBadModelMap", sm, err)
	}
	if sm, err := c.resolveSegment("codex", bad, ModelsFromMap); err != nil || sm.Models["light"] != "gemini-3.8-flash-high" {
		t.Fatalf("resolveSegment(codex, bad) = %+v, %v", sm, err)
	}
	tiers := []Tier{"light", "standard", "strong", "top"}
	m, un, err := resolveTiers("agy", tiers, nil)
	if err != nil || len(un) != 0 {
		t.Fatalf("resolve: un=%v err=%v", un, err)
	}
	for _, tier := range tiers {
		if m[string(tier)] != (TierModel{Model: "inherit"}) {
			t.Fatalf("%s = %v", tier, m[string(tier)])
		}
	}
	if _, _, err := resolveTiers("agy", tiers, map[string]string{"light": "gemini-x"}); !errors.Is(err, errBadModelMap) {
		t.Fatalf("resolve gemini-x = %v", err)
	}
	m, _, err = resolveTiers("agy", tiers, map[string]string{"light": "flash"})
	if err != nil || m["light"].Model != "flash" || m["top"].Model != "inherit" {
		t.Fatalf("resolve flash: %v %v", m, err)
	}
}
