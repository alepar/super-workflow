package wf

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// HeadlessConfig is a run's configuration of its cross-harness children
// (units whose state hints a Family): journaled at the run's start, so a
// resume spawns them the same way. Every field is the user's: a definition
// names only a family, never an argv, a home or an environment.
//
// Owns: how a family resolves to a harness (Other), which login a child
// harness uses (Homes), what is scrubbed from a child's environment beyond
// the harnesses' own identities (EnvDrop), and a launch that replaces an
// adapter's default argv (Cmd).
// Does not: decide which units run where (the definition's OnFamily does) or
// confine a child (its own CLI's native protections do).
type HeadlessConfig struct {
	// Other maps a coordinator harness, or "other" for every coordinator, to
	// the harness FamilyOther means for it. The defaults are
	// claude -> codex and codex -> claude; a coordinator with neither has
	// no other family.
	Other map[string]string `json:",omitempty"`
	// Homes maps a child harness to its login home (its HeadlessHome
	// variable); unset, the child inherits the coordinator's environment.
	Homes map[string]string `json:",omitempty"`
	// EnvDrop are more environment names (a trailing * matches a prefix)
	// scrubbed from every child, for tools the engine does not know by name.
	EnvDrop []string `json:",omitempty"`
	// Cmd maps a child harness to a JSON-argv launch template that replaces
	// the adapter's default; placeholders as headlessPlaceholders lists.
	Cmd map[string][]string `json:",omitempty"`
}

// defaultOther is FamilyOther's resolution with no configuration.
var defaultOther = map[string]string{"claude": "codex", "codex": "claude"}

// headlessPlaceholders are the words a Cmd template may hold; spawn
// replaces each with its value for the unit.
var headlessPlaceholders = []string{"{workspace}", "{state}", "{run}", "{unit}", "{model}", "{effort}", "{session}", "{wf}"}

// resolveFamily is the harness f names for a coordinator of harness coord:
// "" for the coordinator's own (no hint, or a named harness equal to it),
// the named harness, or FamilyOther's mapping. ok is false when f is
// FamilyOther and coord has no other family.
func resolveFamily(f Family, coord string, cfg *HeadlessConfig) (target string, ok bool) {
	coord = normalizeHarness(coord)
	switch f {
	case "":
		return "", true
	case FamilyOther:
		other := defaultOther[coord]
		if cfg != nil {
			if o, set := cfg.Other["other"]; set {
				other = o
			}
			if o, set := cfg.Other[coord]; set {
				other = o
			}
		}
		other = normalizeHarness(other)
		if other == "" || other == coord {
			return "", false
		}
		return other, true
	}
	if t := normalizeHarness(string(f)); t != coord {
		return t, true
	}
	return "", true
}

// familyStates are the families hinted by def's agent states and the
// states of its children, each with the tiers its states use.
func familyStates(def *Def) map[Family][]Tier {
	out := map[Family]map[Tier]bool{}
	seen := map[*Def]bool{}
	var walk func(d *Def)
	walk = func(d *Def) {
		if d == nil || seen[d] {
			return
		}
		seen[d] = true
		for _, n := range d.nodes {
			k, ok := n.kind.(*agentKind)
			if !ok {
				continue
			}
			h := hintsOf(k.dispatch)
			if h.Family == "" {
				continue
			}
			if out[h.Family] == nil {
				out[h.Family] = map[Tier]bool{}
			}
			if h.Tier != "" {
				out[h.Family][h.Tier] = true
			}
		}
		for _, ch := range d.children {
			walk(ch)
		}
	}
	walk(def)
	res := map[Family][]Tier{}
	for f, ts := range out {
		res[f] = []Tier{}
		for _, t := range dispatchTiers {
			if ts[t] {
				res[f] = append(res[f], t)
			}
		}
	}
	return res
}

// childTargets are the harnesses def's family hints resolve to for a
// coordinator of harness coord, each with the tiers its states use, the
// coordinator's own harness left out.
func childTargets(def *Def, coord string, cfg *HeadlessConfig) map[string][]Tier {
	out := map[string][]Tier{}
	for f, tiers := range familyStates(def) {
		t, ok := resolveFamily(f, coord, cfg)
		if !ok || t == "" {
			continue
		}
		for _, tier := range tiers {
			if !slices.Contains(out[t], tier) {
				out[t] = append(out[t], tier)
			}
		}
		if out[t] == nil {
			out[t] = []Tier{}
		}
	}
	for t := range out {
		slices.SortFunc(out[t], func(a, b Tier) int { return slices.Index(dispatchTiers, a) - slices.Index(dispatchTiers, b) })
	}
	return out
}

// qualifiedHarness is the harness a --model-map key is qualified with
// ("codex.strong" -> codex, "strong"), or "" for an unqualified key. Only a
// registered harness name qualifies: a model with a dot stays a model.
func qualifiedHarness(key string) (h, rest string) {
	pre, rest, ok := strings.Cut(key, ".")
	if !ok || !harness.Known(pre) || harness.Canonical(pre) != pre {
		return "", key
	}
	return pre, rest
}

// ownKeys is modelMap without its harness-qualified keys: the map the
// coordinator's own harness resolves with.
func ownKeys(modelMap map[string]string) map[string]string {
	if modelMap == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range modelMap {
		if h, _ := qualifiedHarness(k); h == "" {
			out[k] = v
		}
	}
	return out
}

// keysFor is modelMap's keys qualified with harness h, unqualified: the map
// a child harness resolves with ("codex.strong=x" is "strong=x" for codex).
func keysFor(modelMap map[string]string, h string) map[string]string {
	out := map[string]string{}
	for k, v := range modelMap {
		if q, rest := qualifiedHarness(k); q == h {
			out[rest] = v
		}
	}
	return out
}

// childTables resolves, for a coordinator of harness coord, every child
// harness def's family hints reach: its tier table under the map's keys
// qualified with it. A value the child harness refuses is errBadModelMap.
func childTables(def *Def, coord string, modelMap map[string]string, cfg *HeadlessConfig) (map[string]SegmentModels, error) {
	targets := childTargets(def, coord, cfg)
	if len(targets) == 0 {
		return nil, nil
	}
	out := map[string]SegmentModels{}
	for t, tiers := range targets {
		own := keysFor(modelMap, t)
		resolved, _, err := resolveTiers(t, tiers, own)
		if err != nil {
			return nil, fmt.Errorf("%s children: %w", t, err)
		}
		sm := SegmentModels{Harness: t, From: ModelsFromDefaults}
		if len(own) > 0 {
			sm.ModelMap, sm.From = maps.Clone(own), ModelsFromMap
		}
		for tier, tm := range resolved {
			if sm.Models == nil {
				sm.Models, sm.Efforts = map[string]string{}, map[string]string{}
			}
			sm.Models[tier], sm.Efforts[tier] = tm.Model, tm.Effort
			if tm.Effort == "" {
				sm.Efforts[tier] = EffortHarnessDefault
			}
		}
		out[t] = sm
	}
	return out, nil
}

// childModels is the table a unit for child harness target spawns with in
// a segment whose models are sm: the journaled one, else (a segment bound
// before the run had child tables) a fresh resolution under sm's map.
func (c *cycle) childModels(sm SegmentModels, coord, target string) SegmentModels {
	if cm, ok := sm.Children[target]; ok {
		return cm
	}
	tables, err := childTables(c.st.def, coord, sm.ModelMap, c.headless())
	if err == nil {
		if cm, ok := tables[target]; ok {
			return cm
		}
	}
	return SegmentModels{Harness: target}
}

// headless is the run's journaled HeadlessConfig; nil when it set none.
func (c *cycle) headless() *HeadlessConfig {
	if c.st.started == nil {
		return nil
	}
	return c.st.started.Headless
}

// errNoFamily is a FamilyOther unit whose coordinator has no other family.
func errNoFamily(coord string) error {
	return fmt.Errorf("%w: this state runs on the other family, and coordinator harness %s has none (set one with run --family %s=<harness>)", ErrUnavailable, coord, coord)
}

// familyUnavailable is why child harness target cannot run units now, from
// the current segment's preflight (harness_checked); "" when it passed or
// was not checked (a library engine without EngineOptions.HeadlessCheck).
func (c *cycle) familyUnavailable(target string) string {
	chk, ok := c.st.harnessChecks[target]
	if !ok || chk.Available {
		return ""
	}
	why := fmt.Sprintf("harness %s is unavailable for %s children: %s", target, chk.Coordinator, chk.Why)
	if chk.Fix != "" {
		why += "; fix: " + chk.Fix
	}
	return why
}

// checkChildHarnesses journals the preflight of every child harness def's
// family hints reach from coordinator harness coord: once per segment, at
// its binding. Without EngineOptions.HeadlessCheck nothing is checked.
func (c *cycle) checkChildHarnesses(coord string) error {
	check := c.e.opts.HeadlessCheck
	if check == nil {
		return nil
	}
	targets := childTargets(c.st.def, coord, c.headless())
	names := slices.Sorted(maps.Keys(targets))
	for _, t := range names {
		// A literal stand-in claim, as the hand-out sizes its offers: the
		// check reads the line's words, and a Codex rule matches its prefix.
		d := check(t, coord, c.headless(), c.spawnLine(strings.Repeat("0", 16)))
		d.Harness, d.Coordinator = t, coord
		if err := c.emitAs("", "", 0, JournalHarnessChecked, 0, d); err != nil {
			return err
		}
	}
	return nil
}
