package wf

import (
	"errors"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
)

// harnessTiers maps each abstract model tier to a concrete model per harness;
// the rows are the adapters' (package harness), the evidence below theirs.
// generic has no row on purpose: there is no fallback model, so a generic run
// with tiered states must supply --model-map (the model-tiers check refuses).
//
// claude: light=haiku, standard=sonnet, strong=opus, top=fable.
// codex: light=gpt-6-luna, standard=gpt-6.2-sol, strong and top=gpt-6.2-astra.
//
// Effort is part of the tier.
// claude: the Agent tool takes a per-spawn effort parameter on non-fork
// subagents (Claude Code v2.1.292+): https://code.claude.com/docs/en/sub-agents
// ("Choose an effort level": the parameter overrides the effort field).
// Levels available depend on the model, so top stays at high.
// codex: spawn_agent takes reasoning_effort ("Reasoning effort override for the
// new agent. Omit to inherit the parent effort."):
// https://github.com/openai/codex/blob/rust-v0.167.1/codex-rs/core/src/tools/handlers/multi_agents_spec.rs
// An empty effort leaves the choice to the harness.
// agy: every tier inherit, the coordinator's model (spec §3); the values agy
// takes are its own tier names (harness.ModelChecker). The rows are listed
// here, never taken from harness.Names(): pi and hermes have none.
//
// Built on first use, never at package initialization: reading the harness
// registry closes it to Register, and a binary that links the engine
// registers its own adapters from its init functions, which run after this
// package's.
var harnessTiers = sync.OnceValue(func() map[string]map[Tier]TierModel {
	out := map[string]map[Tier]TierModel{}
	for _, h := range []string{"claude", "codex", "agy"} {
		row := map[Tier]TierModel{}
		for t, tm := range harness.For(h).Tiers() {
			row[Tier(t)] = TierModel{tm.Model, tm.Effort}
		}
		out[h] = row
	}
	return out
})

// harnessAgentType is the subagent type an agent instruction names when its
// state sets none. claude: the Agent tool's subagent_type, general-purpose
// being the type that takes a fresh, non-fork context and any tool
// (https://code.claude.com/docs/en/sub-agents). codex has no row: spawn_agent
// takes no agent type, so the instruction names none.
func harnessAgentType(h string) string { return harness.For(normalizeHarness(h)).AgentType() }

// EffortHarnessDefault is how the journal spells an empty effort: the harness
// applies its own default and the engine substitutes nothing.
const EffortHarnessDefault = "harness default"

// TierModel is one tier's resolution: the concrete model and the reasoning
// effort to spawn it at.
//
// Owns: the pairing of model and effort for a tier, and the one encoding of
// "the harness cannot set effort": an empty Effort. Hides whether a given
// harness can set effort per spawn.
// Does not: choose models (the harnessTiers table and --model-map do), apply
// precedence against a state's own wf.Effort (mapModel does), or reach the
// client's resolvedModels binding, which stays tier -> model string.
// Role: value type, immutable once resolved. New per-tier spawn settings
// belong here as fields; the resolution order stays in resolveTiers.
type TierModel struct {
	Model  string
	Effort string
}

// effortKeySuffix marks a --model-map key that overrides a tier's effort:
// "strong.effort=xhigh". The value EffortHarnessDefault clears it.
const effortKeySuffix = ".effort"

// collectTiers is the one walker for the tiers a definition uses: every agent
// state's hint in def and in the child definitions reachable from it, each
// tier once, in dispatchTiers order. The model-tiers check and run creation
// both call it and never walk the definition themselves.
func collectTiers(def *Def) []Tier {
	used := map[Tier]bool{}
	seen := map[*Def]bool{}
	var walk func(d *Def)
	walk = func(d *Def) {
		if d == nil || seen[d] {
			return
		}
		seen[d] = true
		for _, n := range d.nodes {
			if k, ok := n.kind.(*agentKind); ok {
				// A state on another family also resolves on its child
				// harness (childTables); its tier stays here for the case
				// where its family is the coordinator's own harness.
				if t := hintsOf(k.dispatch).Tier; t != "" {
					used[t] = true
				}
			}
		}
		for _, ch := range d.children {
			walk(ch)
		}
	}
	walk(def)
	var out []Tier
	for _, t := range dispatchTiers {
		if used[t] {
			out = append(out, t)
		}
	}
	return out
}

// resolveTiers is the single place tier -> {model, effort} happens.
//
// Owns: the order tier -> harness table -> --model-map override, the set of
// tiers it cannot resolve, and validating "<tier>.effort" values. Hides the
// per-harness table.
// Does not: run on resume (the journaled Models and Efforts govern a resumed
// run) or at issue time from the live table. It applies --model-map exactly
// once, never chained: a tier key replaces the table model; otherwise a model
// key equal to the table model replaces it (the tier key wins when both
// match). Either way the tier keeps its row's effort; only a "<tier>.effort"
// key changes it (or clears it with "harness default"), whatever supplied the
// model. The map returned is final and post-map; callers journal it and do not
// re-apply --model-map to tiered hints. A tier with neither a table model nor
// a tier key comes back in unresolved: there is no fallback model. Effort is
// empty only when the harness has no row for the tier and no ".effort" key
// gives one. A "<tier>.effort" value outside dispatchEfforts and
// EffortHarnessDefault is an error, returned before anything is resolved.
func resolveTiers(harness string, tiers []Tier, modelMap map[string]string) (models map[string]TierModel, unresolved []Tier, err error) {
	modelMap = ownKeys(modelMap) // keys qualified with a harness are its children's (keysFor)
	if err := checkEfforts(modelMap); err != nil {
		return nil, nil, err
	}
	if err := checkModelValues(harness, modelMap); err != nil {
		return nil, nil, err
	}
	models = map[string]TierModel{}
	if _, ok := noModelHarness(harness); ok {
		// No model reaches the harness's children: every tier runs on the
		// coordinator's model, and no map value applies.
		return models, slices.Clone(tiers), nil
	}
	row := harnessTiers()[normalizeHarness(harness)]
	for _, t := range tiers {
		tm, inRow := row[t]
		if to, ok := modelMap[string(t)]; ok {
			tm.Model = to
		} else if to, ok := modelMap[allTiersKey]; ok {
			tm.Model = to
		} else if inRow {
			if to, ok := modelMap[tm.Model]; ok {
				tm.Model = to
			}
		} else {
			unresolved = append(unresolved, t)
			continue
		}
		e, ok := modelMap[string(t)+effortKeySuffix]
		if !ok {
			e, ok = modelMap[allTiersKey+effortKeySuffix]
		}
		if ok {
			tm.Effort = e
			if e == EffortHarnessDefault {
				tm.Effort = ""
			}
		}
		models[string(t)] = tm
	}
	return models, unresolved, nil
}

// allTiersKey is the --model-map key that pins every tier: "all=<model>",
// and "all.effort=<effort>" for every tier's effort. A tier's own key wins.
const allTiersKey = "all"

// errBadModelMap marks a --model-map the person must fix: the CLI reports it
// as a usage error.
var errBadModelMap = errors.New("bad --model-map")

// checkEfforts refuses an "<key>.effort" value outside dispatchEfforts and
// EffortHarnessDefault, in key order.
func checkEfforts(modelMap map[string]string) error {
	keys := make([]string, 0, len(modelMap))
	for k := range modelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if e := modelMap[k]; strings.HasSuffix(k, effortKeySuffix) && e != EffortHarnessDefault && !slices.Contains(dispatchEfforts, e) {
			return fmt.Errorf("%w: %s=%s: effort %q is not one of %s, %s", errBadModelMap,
				k, e, e, strings.Join(dispatchEfforts, ", "), EffortHarnessDefault)
		}
	}
	return nil
}

// checkModelValues refuses, in key order, a --model-map value the harness h
// does not take (harness.ModelChecker). It is the one value check and runs
// only inside resolveTiers, so a value is checked once, against the harness
// that starts or binds the segment: no verb entry guesses a harness for it.
// ".effort" values are efforts (checkEfforts). A harness without a checker,
// or none named, takes any value.
func checkModelValues(h string, modelMap map[string]string) error {
	if h == "" {
		return nil
	}
	mc, ok := harness.For(normalizeHarness(h)).(harness.ModelChecker)
	if !ok {
		return nil
	}
	keys := make([]string, 0, len(modelMap))
	for k := range modelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, effortKeySuffix) {
			continue
		}
		if err := mc.CheckModel(modelMap[k]); err != nil {
			return fmt.Errorf("%w: %s=%s: %v", errBadModelMap, k, modelMap[k], err)
		}
	}
	return nil
}

// checkModelMap refuses a --model-map key that would match nothing: a key is
// a tier, all, either with ".effort", or a model some harness's tier table
// lists or one of def's untiered hints names. Values are free: a
// mapped-to model need not be in any table. Effort values are checked too.
// It checks keys and efforts, never a value: a value is checked once, against
// the harness that binds the segment, in resolveTiers (checkModelValues), so
// no verb entry has to guess which harness that will be.
// It does not resolve anything; resolveTiers does, for one harness.
func checkModelMap(def *Def, modelMap map[string]string) error {
	if err := checkEfforts(modelMap); err != nil {
		return err
	}
	keys := make([]string, 0, len(modelMap))
	for k := range modelMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	known := knownModels(def)
	for _, k := range keys {
		if h, rest := qualifiedHarness(k); h != "" {
			// A child harness's key: a tier or all, either with .effort.
			base, _ := strings.CutSuffix(rest, effortKeySuffix)
			if base == allTiersKey || slices.Contains(dispatchTiers, Tier(base)) {
				continue
			}
			return fmt.Errorf("%w: key %q: a key qualified with harness %s names a tier or all (each also as <key>%s)", errBadModelMap, k, h, effortKeySuffix)
		}
		base, isEffort := strings.CutSuffix(k, effortKeySuffix)
		if base == allTiersKey || slices.Contains(dispatchTiers, Tier(base)) || (!isEffort && slices.Contains(known, k)) {
			continue
		}
		tiers := make([]string, len(dispatchTiers))
		for i, t := range dispatchTiers {
			tiers[i] = string(t)
		}
		return fmt.Errorf("%w: key %q is not a tier or a known model; valid keys: %s, %s (each also as <key>%s), or a model: %s",
			errBadModelMap, k, strings.Join(tiers, ", "), allTiersKey, effortKeySuffix, strings.Join(known, ", "))
	}
	return nil
}

// knownModels are the models a --model-map model key may name, sorted: every
// harness table's, and every untiered hint's in def and its children.
func knownModels(def *Def) []string {
	set := map[string]bool{}
	for _, row := range harnessTiers() {
		for _, tm := range row {
			set[tm.Model] = true
		}
	}
	seen := map[*Def]bool{}
	var walk func(d *Def)
	walk = func(d *Def) {
		if d == nil || seen[d] {
			return
		}
		seen[d] = true
		for _, n := range d.nodes {
			if k, ok := n.kind.(*agentKind); ok {
				if h := hintsOf(k.dispatch); h.Tier == "" && h.Model != "" {
					set[h.Model] = true
				}
			}
		}
		for _, ch := range d.children {
			walk(ch)
		}
	}
	walk(def)
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// normalizeHarness is a harness label's canonical name, the one adapters
// register under (harness.Canonical).
func normalizeHarness(h string) string { return harness.Canonical(h) }

// resolveHarness is called only for creation, after looking for a saved run.
func resolveHarness(h string) string {
	h = normalizeHarness(h)
	if h != "" {
		return h
	}
	return envHarness(os.Getenv)
}

// recordedHarness supplies an honest fallback for journals that recorded no
// harness. It does not mutate or rewrite historical run_started records. A
// journal that also recorded a slot count (removed) loads; the field is ignored.
func recordedHarness(d RunStartedData) (h string, legacy bool) {
	if d.Harness == "" {
		return "generic", true
	}
	return d.Harness, false
}

func harnessLine(rs RunStatus) string {
	h, legacy := rs.Harness, rs.LegacyHarness
	if h == "" {
		h, legacy = "generic", true
	}
	line := "harness: " + h
	if legacy {
		line += " (legacy generic fallback)"
	}
	return line
}

func resumeHarnessMatches(id RunID, d RunStartedData, opts EngineOptions) error {
	h, _ := recordedHarness(d)
	if opts.Harness != "" && normalizeHarness(opts.Harness) != h {
		return fmt.Errorf("run %s was started with different harness %q; resume without --harness to keep it", id, h)
	}
	return nil
}

// noModelHarness is harness h's sentence on where its children's model comes
// from, when its spawn tool takes no model (harness.NoModelSpawn).
func noModelHarness(h string) (string, bool) {
	if h == "" {
		return "", false
	}
	nm, ok := harness.For(normalizeHarness(h)).(harness.NoModelSpawn)
	if !ok {
		return "", false
	}
	return nm.NoModel(), true
}

// unresolvedTiersNote is the degraded tiers record for a segment of harness h
// whose tiers have no model, or, for a harness whose spawn takes no model, a
// --model-map that is not applied; nil when there is nothing to say.
func unresolvedTiersNote(h string, tiers []Tier, mapGiven bool) *DegradedData {
	names := make([]string, len(tiers))
	for i, t := range tiers {
		names[i] = string(t)
	}
	if why, ok := noModelHarness(h); ok {
		if len(tiers) == 0 && !mapGiven {
			return nil
		}
		action := "not applied"
		if len(tiers) > 0 {
			action += ": tier(s) " + strings.Join(names, ", ") + " run on the coordinator's model"
		}
		if mapGiven {
			action += "; the --model-map is not applied"
		}
		return &DegradedData{Kind: DegradedTiers, Cause: why, Action: action}
	}
	if len(tiers) == 0 {
		return nil
	}
	return &DegradedData{Kind: DegradedTiers,
		Cause:  fmt.Sprintf("harness %q has no model for tier(s) %s", h, strings.Join(names, ", ")),
		Action: "not applied: those children run on the session's model; --model-map <tier>=<model> names one"}
}
