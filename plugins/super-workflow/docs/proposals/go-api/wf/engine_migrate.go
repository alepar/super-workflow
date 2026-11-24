package wf

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// migrate reconciles the run's current definition hash (the last migrated
// ToHash, else the one run_started journaled) with def. A difference is covered
// by a declared Migration with the matching FromHash and ToHash, journaled once
// as migrated under the run's lock; with none it is ErrDefChanged naming both
// hashes. The hash does not cover Migrations, so adding one never changes it.
func (c *cycle) migrate(def *Def) error {
	if c.e.opts.Adopt {
		return c.migrateAdopting(def)
	}
	have, want := c.st.hash, definitionHashAssets(def, c.selectedAssetRoots(def))
	if c.e.opts.Assets != "" && c.e.opts.Assets != c.st.started.Assets && !c.e.opts.Adopt {
		return fmt.Errorf("%w: changing the asset root requires resume --adopt --assets <root>", ErrDefChanged)
	}
	if c.e.opts.Adopt && c.e.opts.Assets == "" && c.st.started.Assets == "" {
		if old := c.st.started.Params["skillsRoot"]; old != "" && def.assets != "" {
			oldRoot, _ := filepath.Abs(old)
			newRoot, _ := filepath.Abs(def.assets)
			if oldRoot != newRoot {
				return fmt.Errorf("%w: recorded skillsRoot %s differs from binary assets %s; choose --assets <root> explicitly", ErrDefChanged, oldRoot, newRoot)
			}
		}
	}
	if have == want {
		return nil
	}
	for _, m := range def.migrations {
		if m.FromHash == have && m.ToHash == want {
			return c.appendMigration(def, MigratedData{FromHash: have, ToHash: want, States: m.States, Assets: c.e.opts.Assets, AssetRoots: c.selectedAssetRoots(def)})
		}
	}
	return fmt.Errorf("%w: run %s was started at %s, definition %s is now %s; declare a Migration from %s to %s, or resume with --adopt if its live states are unchanged", ErrDefChanged, c.run, have, def.name, want, have, want)
}

// migrateAdopting is migrate for an adoption (resume or next --adopt, and the
// adoption of a fork): the run goes onto def with def's own assets. They are
// --assets when given, else the roots def pins; a run that recorded roots
// adopted onto a definition that pins none is refused until --assets names
// them. The recorded roots are never kept under a new definition, so its
// states never run with another definition's prompts.
func (c *cycle) migrateAdopting(def *Def) error {
	if c.e.opts.Assets == "" && c.st.started.Assets == "" {
		if old := c.st.started.Params["skillsRoot"]; old != "" && def.assets != "" {
			oldRoot, _ := filepath.Abs(old)
			newRoot, _ := filepath.Abs(def.assets)
			if oldRoot != newRoot {
				return fmt.Errorf("%w: recorded skillsRoot %s differs from binary assets %s; choose --assets <root> explicitly", ErrDefChanged, oldRoot, newRoot)
			}
		}
	}
	roots := assetRoots(def, c.e.opts.Assets)
	if roots == nil && c.e.opts.Assets == "" && (len(c.st.started.AssetRoots) > 0 || c.st.started.Assets != "") {
		return fmt.Errorf("%w: definition %s pins no assets, and run %s recorded its own; adopt with --assets <root> to choose them", ErrDefChanged, def.name, c.run)
	}
	have, want := c.st.hash, definitionHashAssets(def, roots)
	if have == want && reflect.DeepEqual(roots, nilIfEmpty(c.st.started.AssetRoots)) && (c.e.opts.Assets == "" || c.e.opts.Assets == c.st.started.Assets) {
		return nil
	}
	// The selected roots must hold every asset a state reads: an adoption
	// onto a root missing one is refused before anything is journaled.
	if problems := assetProblems(def, roots); len(problems) > 0 {
		msgs := make([]string, len(problems))
		for i, p := range problems {
			msgs[i] = p.Msg
		}
		return fmt.Errorf("%w: run %s cannot adopt definition %s's assets: %s", ErrDefChanged, c.run, def.name, strings.Join(msgs, "; "))
	}
	// The top-level choice is journaled with the roots, --assets or the
	// definition's own pin, so the fold moves Assets and the legacy
	// skillsRoot binding with them.
	top := c.e.opts.Assets
	if top == "" {
		top = roots[def.name]
	}
	// A recorded top-level choice is never dropped silently: a definition
	// that pins no top-level root over a run that recorded one is refused
	// until --assets names it.
	if top == "" && (c.st.started.Assets != "" || c.st.started.AssetRoots[def.name] != "" || c.st.started.Params["skillsRoot"] != "") {
		return fmt.Errorf("%w: definition %s pins no top-level assets, and run %s recorded its own; adopt with --assets <root> to choose them", ErrDefChanged, def.name, c.run)
	}
	for _, m := range def.migrations {
		if m.FromHash == have && m.ToHash == want {
			return c.appendMigration(def, MigratedData{FromHash: have, ToHash: want, States: m.States, Assets: top, AssetRoots: roots})
		}
	}
	return c.adoptWith(def, have, want, roots, top)
}

// adopt carries the run onto def with no declared Migration (E1): every live
// instance's current state must exist in def under the same name, with the
// same kind and result type as the run's recorded shape says it had. Then the
// adoption is journaled as migrated, with def's shape; otherwise it is
// refused, listing each state that vanished or changed. A journal from before
// shapes compares names only.
func (c *cycle) adopt(def *Def, have, want string) error {
	return c.adoptWith(def, have, want, c.selectedAssetRoots(def), c.e.opts.Assets)
}

// adoptWith is adopt journaling roots as the run's assets and top as its
// top-level choice ("" keeps the recorded one).
func (c *cycle) adoptWith(def *Def, have, want string, roots map[string]string, top string) error {
	if c.st.ended {
		return fmt.Errorf("%w: run %s has ended: an ended run is never resumed; rewind it (wf dev rewind --to died) and adopt the fork", ErrRejected, c.run)
	}
	shape := defShape(def)
	// States an engine before stable names recorded under their legacy form
	// ("<Ident>·<N>]") carry over to the one state of the same definition
	// named for that type: the renames go into the adoption's migrated
	// record; none or several is a refusal naming them.
	// Historic names are read as of now: the renames earlier adoptions and
	// migrations journaled apply, so only names still unresolved are inferred.
	recs := namesNow(c.recs, c.st)
	renames, ambiguous := c.legacyRenames(def, shape, recs)
	if len(ambiguous) > 0 {
		slices.Sort(ambiguous)
		return fmt.Errorf("%w: run %s was started at %s, definition %s is now %s, and its states do not all carry over: %s; declare a Migration from %s to %s", ErrDefChanged, c.run, have, def.name, want, strings.Join(ambiguous, ", "), have, want)
	}
	rn := func(in *instance, name string) (string, bool) {
		if to, ok := renames[strings.Join(append(c.st.defPath(in), name), "/")]; ok {
			return to[strings.LastIndex(to, "/")+1:], true
		}
		return name, false
	}
	// Where each instance is: its current state once it moved (a transition
	// names the state it left, so a record's State is not where it is);
	// before that, the state its records name, else the first state of its
	// definition as the run's shape recorded it. The fold places an instance
	// that never moved at the new definition's first state.
	moved := map[string]bool{}
	at := map[string]string{}
	visited := map[string]map[string]bool{} // instance to the states it recorded work at
	for _, r := range recs {
		if r.Kind == JournalTransition {
			moved[r.Instance] = true
		}
		if r.State != "" {
			at[r.Instance] = r.State
			if visited[r.Instance] == nil {
				visited[r.Instance] = map[string]bool{}
			}
			visited[r.Instance][r.State] = true
		}
	}
	var bad []string
	// Every child definition the run spawned keeps its input and result
	// types: a parent reads a settled child's result across that seam.
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in == nil || in.ID == "" || c.st.shape == nil {
			continue
		}
		key := c.st.ownDef(in) + "/:seam"
		if was, now := c.st.shape[key], shape[key]; was != "" && was != now {
			bad = append(bad, fmt.Sprintf("%s (child %s: %s, now %s)", c.st.qualified(c.st.insts[in.Parent]), c.st.ownDef(in), was, now))
		}
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in == nil {
			continue
		}
		own := c.st.ownDef(in)
		// Every state the instance recorded work at keeps its result type:
		// a later state may read its output (E1, not only the live state).
		for name := range visited[id] {
			if _, renamed := rn(in, name); renamed {
				continue // a legacy name's recorded type is legacy-named too
			}
			key := own + "/" + name
			if now, ok := shape[key]; ok && c.st.shape != nil && c.st.shape[key] != "" && c.st.shape[key] != now {
				bad = append(bad, fmt.Sprintf("%s (%s, now %s)", strings.Join(append(c.st.defPath(in), name), "/"), c.st.shape[key], now))
			}
		}
		if in.Phase == phaseDone {
			continue
		}
		name := in.Cur
		if !moved[id] {
			if n := at[id]; n != "" {
				name = n
			} else if first := c.st.shape[own+"/"]; first != "" {
				name = first
			}
		}
		if name == "" {
			continue
		}
		recorded := name
		name, renamed := rn(in, name)
		cur, _ := rn(in, in.Cur)
		where := strings.Join(append(c.st.defPath(in), name), "/")
		key := own + "/" + name
		now, ok := shape[key]
		switch {
		case !ok:
			bad = append(bad, where+" (no such state)")
		case !renamed && !visited[id][recorded] && c.st.shape != nil && c.st.shape[key] != "" && c.st.shape[key] != now:
			bad = append(bad, fmt.Sprintf("%s (%s, now %s)", where, c.st.shape[key], now))
		case name != cur:
			bad = append(bad, where+" (the definition's first state is now "+cur+")")
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		bad = slices.Compact(bad)
		return fmt.Errorf("%w: run %s was started at %s, definition %s is now %s, and its states do not all carry over: %s; declare a Migration from %s to %s", ErrDefChanged, c.run, have, def.name, want, strings.Join(bad, ", "), have, want)
	}
	// A changed definition must be able to bind what the run's live
	// instances can still reach: refused here, before anything is
	// journaled, never as a halt mid-run.
	if have != want {
		stateName := func(in *instance, name string) string { n, _ := rn(in, name); return n }
		if problems := c.adoptBindingProblems(def, stateName, recs); len(problems) > 0 {
			return fmt.Errorf("%w: run %s cannot adopt definition %s (%s): %s", ErrDefChanged, c.run, def.name, want, strings.Join(problems, "; "))
		}
	}
	carried := map[string]string{}
	for _, id := range c.st.order {
		if in := c.st.insts[id]; in != nil {
			carried[id] = c.st.qualified(in)
		}
	}
	data := MigratedData{FromHash: have, ToHash: want, Adopted: true, Shape: shape, Carried: carried, Assets: top, AssetRoots: roots, States: renames}
	if err := c.appendMigration(def, data); err != nil {
		return err
	}
	if !c.dry && c.e.opts.OnAdopt != nil {
		c.e.opts.OnAdopt(c.run, data)
	}
	return nil
}

// defShape is d's shape, for d and every child definition:
// "<definition>/<state>" to the state's kind, its output type, a terminal's
// yielded result type with its fields, and a Sub or Map's child definition;
// "<definition>/" to its first state; and "<definition>/:seam" to a child
// definition's input and result types with their fields, the seam a parent
// reads a settled child's result across.
func defShape(d *Def) map[string]string {
	out := map[string]string{}
	seen := map[*Def]bool{}
	var walk func(d *Def)
	walk = func(d *Def) {
		if seen[d] {
			return
		}
		seen[d] = true
		views := inspectStates(d, d.name)
		if len(views) > 0 {
			out[d.name+"/"] = views[0].Name
		}
		for _, v := range views {
			parts := []string{v.Kind}
			if v.Output != "" {
				parts = append(parts, v.Output)
			}
			if t, ok := v.node.kind.(*terminalKind); ok && t.result != nil {
				parts = append(parts, "yields "+typeShape(t.result))
			}
			if v.Child != "" {
				parts = append(parts, "child "+v.Child)
			}
			out[d.name+"/"+v.Name] = strings.Join(parts, " ")
		}
		if d.input != nil || d.result != nil {
			out[d.name+"/:seam"] = typeShape(d.input) + " -> " + typeShape(d.result)
		}
		for _, ch := range d.children {
			walk(ch)
		}
	}
	walk(d)
	return out
}

// typeShape is t with its fields: a type whose name stayed while its fields
// changed is not the same result.
func typeShape(t reflect.Type) string {
	if t == reflect.TypeFor[RunPath]() {
		return "wf.RunPath{\"$wf.runPath\":relative-string}"
	}
	if t == nil {
		return "none"
	}
	return t.String() + "{" + strings.Join(optionFields(t), "; ") + "}"
}

// legacyRenames maps each state name the run recorded or is at that this
// definition does not have to the one state whose name an engine before
// stable names gave it (node.legacy, matched with the compiler's ordinals
// normalized: exact, nesting kept): qualified old name to qualified new, as a
// Migration's States. problems lists a legacy-looking name with no such state
// or several, two old states that would both become one, and a rename whose
// recorded shape (kind and output type) is not the new state's.
func (c *cycle) legacyRenames(def *Def, shape map[string]string, recs []JournalRecord) (renames map[string]string, problems []string) {
	cands := map[string][]string{}
	walked := map[*Def]bool{}
	var walk func(d *Def)
	walk = func(d *Def) {
		if walked[d] {
			return
		}
		walked[d] = true
		for _, n := range d.nodes {
			if n.legacy != "" && n.legacy != n.name {
				k := d.name + "/" + normalizeLegacy(n.legacy)
				cands[k] = append(cands[k], n.name)
			}
		}
		for _, ch := range d.children {
			walk(ch)
		}
	}
	walk(def)
	type at struct {
		in   *instance
		name string
	}
	var names []at
	for _, r := range recs {
		if in := c.st.insts[r.Instance]; in != nil && r.State != "" {
			names = append(names, at{in, r.State})
		}
	}
	// A run that never moved names its first state only in its recorded
	// shape; a crash right after a transition leaves its target only in the
	// transition's data, where the fold put the instance.
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in == nil {
			continue
		}
		names = append(names, at{in, in.Cur})
		if c.st.shape != nil {
			names = append(names, at{in, c.st.shape[c.st.ownDef(in)+"/"]})
		}
	}
	seen := map[string]bool{}
	into := map[string]string{}
	for _, a := range names {
		own := c.st.ownDef(a.in)
		if a.name == "" {
			continue
		}
		if _, ok := shape[own+"/"+a.name]; ok {
			continue
		}
		path := c.st.defPath(a.in)
		old := strings.Join(append(path, a.name), "/")
		if seen[old] {
			continue
		}
		seen[old] = true
		cs := slices.Clone(cands[own+"/"+normalizeLegacy(a.name)])
		slices.Sort(cs)
		switch {
		case len(cs) == 0:
			if _, legacy := legacyStateName(a.name); legacy {
				problems = append(problems, old+" (recorded with a legacy state name: no state of this definition had that name)")
			}
			continue
		case len(cs) > 1:
			problems = append(problems, old+" (recorded with a legacy state name: several states had that name: "+strings.Join(cs, ", ")+")")
			continue
		}
		now := strings.Join(append(path, cs[0]), "/")
		if other, dup := into[now]; dup {
			problems = append(problems, fmt.Sprintf("%s and %s (recorded with legacy state names: both would be %s)", other, old, cs[0]))
			continue
		}
		into[now] = old
		if c.st.shape != nil {
			if was, is := c.st.shape[own+"/"+a.name], shape[own+"/"+cs[0]]; was != "" && was != is {
				problems = append(problems, fmt.Sprintf("%s (recorded with a legacy state name as %s, now %s is %s)", old, was, cs[0], is))
				continue
			}
		}
		if renames == nil {
			renames = map[string]string{}
		}
		renames[old] = now
	}
	return renames, problems
}

// legacyRefusal refuses verb on a journal that still carries state names in
// the legacy form no adoption renamed (legacyStateName): a replay, a rewind
// or a fanout reads the journal by state name, and those names exist in no
// definition built since. The run is adopted first (next --adopt), which
// journals the renames.
func legacyRefusal(verb string, run RunID, recs []JournalRecord) error {
	renamed := map[string]bool{}
	for _, r := range recs {
		var d MigratedData
		if r.Kind == JournalMigrated && json.Unmarshal(r.Data, &d) == nil {
			for old := range d.States {
				renamed[old[strings.LastIndex(old, "/")+1:]] = true
			}
		}
	}
	var names []string
	for _, r := range recs {
		if _, ok := legacyStateName(r.State); ok && !renamed[r.State] && !slices.Contains(names, r.State) {
			names = append(names, r.State)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: run %s was recorded with legacy state names (%s); adopt it first: next --adopt carries it onto this binary's names", ErrRejected, verb, run, strings.Join(names, ", "))
}

// adoptedNames is the journal with each record under its state name as of
// now (namesNow): a replay matches recorded answers and routes against the
// definition by state name.
func adoptedNames(recs []JournalRecord, def *Def) ([]JournalRecord, error) {
	if !slices.ContainsFunc(recs, renaming) {
		return recs, nil
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return nil, err
	}
	return namesNow(recs, st), nil
}

// renaming reports whether r is a migrated record that renamed states.
func renaming(r JournalRecord) bool {
	var d MigratedData
	return r.Kind == JournalMigrated && json.Unmarshal(r.Data, &d) == nil && len(d.States) > 0
}

// namesNow is recs with every record's state name as of now: each migrated
// record that renamed states (an adoption's legacy renames, a Migration's
// States), in journal order, renames the records before it, their envelope
// State and a transition's target, so a chain of renames composes. Whatever
// reads a historic state name against today's definition (a replay's index,
// an adoption's checks) reads it through this. st is the run's fold, for
// each instance's definition path.
func namesNow(recs []JournalRecord, st *runState) []JournalRecord {
	out := slices.Clone(recs)
	for m := range out {
		var d MigratedData
		if out[m].Kind != JournalMigrated || json.Unmarshal(out[m].Data, &d) != nil || len(d.States) == 0 {
			continue
		}
		for i := range out[:m] {
			r := &out[i]
			in := st.insts[r.Instance]
			if in == nil {
				continue
			}
			ren := st.renamesFor(in, d.States)
			if to, ok := ren[r.State]; ok {
				r.State = to
			}
			switch r.Kind {
			case JournalRunStarted:
				var started RunStartedData
				if json.Unmarshal(r.Data, &started) == nil && len(started.HaltBefore) > 0 {
					started.renameBreakpoints(d.States)
					if raw, err := json.Marshal(started); err == nil {
						r.Data = raw
					}
				}
			case JournalBreakpointHalted:
				var halt BreakpointHaltedData
				if json.Unmarshal(r.Data, &halt) == nil {
					halt.State = renameBreakpoint(halt.State, d.States)
					if raw, err := json.Marshal(halt); err == nil {
						r.Data = raw
					}
				}
			case JournalTransition:
				var t TransitionData
				if json.Unmarshal(r.Data, &t) == nil {
					if to, ok := ren[t.To]; ok {
						t.To = to
						if raw, err := json.Marshal(t); err == nil {
							r.Data = raw
						}
					}
				}
			}
		}
	}
	return out
}
