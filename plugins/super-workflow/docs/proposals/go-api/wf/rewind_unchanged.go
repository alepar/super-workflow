package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Rewind to the longest unchanged prefix (sw-0x90).
//
// Owns: per-state identities, recorded at run start and on every migration;
// the scan for the first journal record that runs a state whose identity
// the current definition changed (or that the operator named as changed);
// the fork cut before it, checked by a dry-run adoption onto the current
// definition, stepping back a bounded number of cuts when that adoption is
// refused.
// Does not: see inside closures (route, check and compute bodies): their
// journaled decisions are kept as recorded, which is why the operator can
// name states with changed code, and why a different binary with no
// declared change is refused rather than called unchanged. It never moves
// the source run: the cut is a new run.

// stateIDParts are the parts of a state's identity, by their one-letter key
// in the recorded string, in that string's order.
var stateIDParts = []struct{ key, name string }{
	{"s", "shape"}, {"p", "prompt"}, {"c", "checks"}, {"e", "edges"}, {"a", "other declared fields"},
}

func shortHash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("wf: hashing a state view: " + err.Error()) // views are plain data
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// stateIDs is each state's identity under the asset roots: "<definition>/<state>"
// to "s=<shape> p=<prompt> c=<checks> e=<edges> a=<all>", each a short hash.
// The shape is the state's recorded shape (defShape) and its child's seam;
// the prompt is its template and inputs with the content of each asset it
// reads; the checks its named checks and declared reads; the edges every
// edge, route, case table, retry, visit cap and join; and all is the whole
// state view with the definition-level values it reads (its case enum's
// values, its gate's policy rules, the counters and options its view
// names). Two states with equal identity are declared the same way.
func stateIDs(d *Def, roots map[string]string) map[string]string {
	v := inspectStructure(d)
	p := hashPlanOf(d, v)
	shape := defShape(d)
	out := map[string]string{}
	i := 0
	var walk func(v *DefView, prefix string)
	walk = func(v *DefView, prefix string) {
		key := prefix + v.Name
		root, ok := roots[key]
		if !ok {
			root = v.assetRoot
		}
		refs := p.refs[i]
		i++
		assets, _ := readAssetRefs(root, refs, hostAssets(root))
		for _, sv := range v.States {
			var read map[string]string
			for _, sr := range refs {
				if sr.node != sv.node {
					continue
				}
				for _, rel := range sr.rels {
					if read == nil {
						read = map[string]string{}
					}
					read[rel] = assets[rel]
				}
			}
			out[v.Name+"/"+sv.Name] = stateID(v, sv, read, shape)
		}
		for j := range v.Children {
			walk(&v.Children[j], key+"/")
		}
	}
	walk(&v, "")
	return out
}

func stateID(v *DefView, sv StateView, assets map[string]string, shape map[string]string) string {
	seam := ""
	if sv.Child != "" {
		seam = shape[sv.Child+"/:seam"]
	}
	full, _ := json.Marshal(sv)
	text := string(full)
	reads := map[string]any{}
	if sv.Cases != nil && sv.Cases.Enum != "" {
		reads["enum"] = v.Enums[sv.Cases.Enum]
	}
	var policy []PolicyView
	for _, r := range v.Policy {
		if r.Gate == sv.Name {
			policy = append(policy, r)
		}
	}
	reads["policy"] = policy
	var named []string
	for _, c := range append(slices.Clone(v.Counters), v.Options...) {
		if name, _, _ := strings.Cut(c, " "); name != "" && strings.Contains(text, name) {
			named = append(named, c)
		}
	}
	reads["named"] = named
	parts := map[string]string{
		"s": shortHash([]string{shape[v.Name+"/"+sv.Name], seam}),
		"p": shortHash(struct {
			Prompt     string
			Candidates []string
			Inputs     []string
			Assets     map[string]string
			Question   string
		}{sv.Prompt, sv.PromptCandidates, sv.Inputs, assets, sv.Question}),
		"c": shortHash(struct {
			Checks, Observes []string
			DoneWhen         string
		}{sv.Checks, sv.Observes, sv.DoneWhen}),
		"e": shortHash(struct {
			Edges     []EdgeView
			Retry     *RetryView
			Cases     *CasesView
			Route     []string
			MaxVisits string
			Measure   string
			Exits     map[string]string
			Join      string
		}{sv.Edges, sv.Retry, sv.Cases, sv.Route, sv.MaxVisits, sv.Measure, sv.Exits, sv.Join}),
		"a": shortHash(struct {
			View   json.RawMessage
			Assets map[string]string
			Reads  map[string]any
		}{full, assets, reads}),
	}
	fields := make([]string, len(stateIDParts))
	for i, p := range stateIDParts {
		fields[i] = p.key + "=" + parts[p.key]
	}
	return strings.Join(fields, " ")
}

// changedParts names the parts in which two identities differ.
func changedParts(was, now string) []string {
	split := func(s string) map[string]string {
		m := map[string]string{}
		for _, f := range strings.Fields(s) {
			if k, v, ok := strings.Cut(f, "="); ok {
				m[k] = v
			}
		}
		return m
	}
	a, b := split(was), split(now)
	var out []string
	for _, p := range stateIDParts {
		if a[p.key] != b[p.key] && (p.key != "a" || len(out) == 0) {
			out = append(out, p.name)
		}
	}
	return out
}

// ClosureCaveat is what a rewind to the unchanged prefix cannot see.
const ClosureCaveat = "closure bodies (routes, checks, computes, reasons) are not part of a state's identity: their recorded decisions are kept as they were; name the states whose code changed with --changed"

// Divergence is the first journal record of a run that the current
// definition would not have produced the same way.
type Divergence struct {
	Seq      int64
	Kind     JournalKind
	Instance string `json:",omitempty"`
	State    string // "<definition>/<state>"
	// Why is "named by --changed", "no such state", or the identity parts
	// that changed ("prompt", "edges", ...).
	Why []string
}

// UnchangedReport is what RewindUnchanged made.
type UnchangedReport struct {
	Run       RunID         // the fork
	Snapshot  *SnapshotFork `json:",omitempty"` // a snapshot run's restored fork
	Diverged  Divergence
	Cut       int64    // the last source record the fork keeps
	Kept      int      // records kept
	Applied   []string `json:",omitempty"` // effects applied after the cut, not undone
	SteppedBy []string `json:",omitempty"` // earlier cuts whose adoption was refused, and why
	Caveat    string
}

// unchangedStepBacks bounds how many cuts RewindUnchanged tries.
const unchangedStepBacks = 3

// RewindUnchanged forks run before the first record that runs a state the
// current definition changed, or one of changed ("<definition>/<state>"),
// and checks that the fork adopts onto the current definition. A snapshot
// run is restored into out from the latest checkpoint before that record;
// any other run is forked by its journal as dev rewind --to died forks it.
// With nothing changed and nothing named it refuses: as "nothing to rewind"
// when the binary is the run's own, or, when the binary's source differs,
// because the change is in code this check cannot see.
func (e *Engine) RewindUnchanged(run RunID, changed []string, out string) (UnchangedReport, error) {
	if err := e.CheckBoundaryModelMap(run); err != nil {
		return UnchangedReport{}, err
	}
	var recs []JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return UnchangedReport{}, err
	}
	var started RunStartedData
	if err := json.Unmarshal(recs[0].Data, &started); err != nil {
		return UnchangedReport{}, err
	}
	if started.StateIDs == nil {
		return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: run %s was recorded before per-state identities; pick a checkpoint with --to <seq> and adopt the fork", ErrRejected, run)
	}
	if started.Snapshots && out == "" {
		return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: snapshot runs need --out <new directory>", ErrRejected)
	}
	if !slices.ContainsFunc(recs, func(r JournalRecord) bool { return r.Kind == JournalRunEnded }) {
		if err := legacyRefusal("rewind", run, recs); err != nil {
			return UnchangedReport{}, err
		}
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return UnchangedReport{}, err
	}
	roots := assetRoots(def, e.opts.Assets)
	if roots == nil {
		roots = started.AssetRoots
	}
	now := stateIDs(def, roots)
	named := map[string]bool{}
	for _, s := range changed {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if _, ok := now[s]; !ok {
			if _, was := started.StateIDs[s]; !was {
				return UnchangedReport{}, fmt.Errorf("%w: --changed %s: no state of that name (use <definition>/<state>)", ErrRejected, s)
			}
		}
		named[s] = true
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return UnchangedReport{}, err
	}
	div, found, err := firstDivergence(recs, started.StateIDs, now, named, st)
	if err != nil {
		return UnchangedReport{}, err
	}
	if !found {
		switch {
		case len(named) > 0:
			return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: no state named by --changed ran in run %s, and no other state changed: nothing to rewind", ErrRejected, run)
		case started.Build == nil:
			return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: every state of run %s is declared as it was, but the run records no build to compare: code may have changed in ways this check cannot see (%s); name the states whose code changed with --changed <definition>/<state>[,...], or pick a checkpoint with --to <seq>", ErrRejected, run, ClosureCaveat)
		}
		if why := started.Build.differs(currentBuild()); why != "" {
			return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: every state of run %s is declared as it was, but this binary differs (%s): code changed in ways this check cannot see; name the states whose code changed with --changed <definition>/<state>[,...], or pick a checkpoint with --to <seq>", ErrRejected, run, why)
		}
		return UnchangedReport{}, fmt.Errorf("%w: rewind --to unchanged: run %s is unchanged: the definition and the binary are the ones it was recorded with; nothing to rewind", ErrRejected, run)
	}
	rep := UnchangedReport{Diverged: div, Caveat: ClosureCaveat}
	for _, cut := range cutCandidates(recs, div.Seq, started.Snapshots) {
		kept := recs[:cut]
		if err := e.dryAdopt(run, kept, def); err != nil {
			rep.SteppedBy = append(rep.SteppedBy, fmt.Sprintf("cut at %d: %v", kept[len(kept)-1].Seq, err))
			if len(rep.SteppedBy) >= unchangedStepBacks {
				break
			}
			continue
		}
		rep.Cut, rep.Kept, rep.Applied = kept[len(kept)-1].Seq, len(kept), appliedDropped(recs, kept)
		mark := func(d *RewoundData) { d.To, d.OperatorChosen = "unchanged", true }
		if started.Snapshots {
			fork, err := e.forkSnapshot(run, rep.Cut, out, mark)
			if err != nil {
				return rep, err
			}
			rep.Run, rep.Snapshot = fork.Run, &fork
			return rep, nil
		}
		d := RewoundData{From: run, Seq: rep.Cut, State: div.State, Applied: rep.Applied}
		mark(&d)
		fork, err := e.writeRewind(run, kept, d)
		rep.Run = fork.Run
		return rep, err
	}
	if len(rep.SteppedBy) == 0 {
		return rep, fmt.Errorf("%w: rewind --to unchanged: run %s has no checkpoint before %s (seq %d)", ErrRejected, run, div.State, div.Seq)
	}
	return rep, fmt.Errorf("%w: rewind --to unchanged: the current definition adopts no cut before %s (seq %d): %s", ErrRejected, div.State, div.Seq, strings.Join(rep.SteppedBy, "; "))
}

// firstDivergence is the first record that runs a state (any record naming
// a state but a transition, which the unchanged source decided, a world
// checkpoint or a scope reset) whose identity in the segment it was
// recorded in differs from now's, that now lacks, or that named lists.
// A migration recorded without identities cannot be judged past: the
// migration itself is the divergence.
func firstDivergence(recs []JournalRecord, ids, now map[string]string, named map[string]bool, st *runState) (Divergence, bool, error) {
	defOf := func(instance string) string {
		if in := st.insts[instance]; in != nil && in.DefName != "" {
			return in.DefName
		}
		return st.started.Def
	}
	for _, r := range recs[1:] {
		switch r.Kind {
		case JournalMigrated:
			var m MigratedData
			if err := json.Unmarshal(r.Data, &m); err != nil {
				return Divergence{}, false, err
			}
			if m.StateIDs == nil {
				return Divergence{Seq: r.Seq, Kind: r.Kind, Why: []string{"migrated by an engine that recorded no per-state identities"}}, true, nil
			}
			ids = m.StateIDs
			continue
		case JournalTransition, JournalWorldSnapshot, JournalScopeReset:
			continue
		}
		if r.State == "" {
			continue
		}
		key := defOf(r.Instance) + "/" + r.State
		var why []string
		switch was, is := ids[key], now[key]; {
		case named[key]:
			why = []string{"named by --changed"}
		case is == "":
			why = []string{"no such state"}
		case was != is:
			why = changedParts(was, is)
		default:
			continue
		}
		return Divergence{Seq: r.Seq, Kind: r.Kind, Instance: r.Instance, State: key, Why: why}, true, nil
	}
	return Divergence{}, false, nil
}

// cutCandidates are the prefix lengths to try, latest first: a snapshot
// run's available checkpoints before seq; any other run's record just
// before seq, then each earlier arrival (a transition record, and the
// start state's arrival before the run's first state record).
func cutCandidates(recs []JournalRecord, seq int64, snapshots bool) []int {
	var out []int
	if !snapshots && seq > 1 {
		out = append(out, int(seq-1))
	}
	for i := int(seq) - 2; i >= 0; i-- {
		r := recs[i]
		switch {
		case snapshots && r.Kind == JournalWorldSnapshot:
			var w WorldSnapshotData
			if json.Unmarshal(r.Data, &w) == nil && w.Unavailable == "" {
				out = append(out, i+1)
			}
		case !snapshots && r.Kind == JournalTransition && i+1 != int(seq-1):
			out = append(out, i+1)
		}
	}
	// The run's arrival at its start state: before the first record that
	// runs a state.
	if !snapshots {
		for i, r := range recs[1:] {
			if r.State == "" || r.Kind == JournalTransition || r.Kind == JournalWorldSnapshot || r.Kind == JournalScopeReset {
				continue
			}
			if n := i + 1; n < int(seq) && (len(out) == 0 || n < out[len(out)-1]) {
				out = append(out, n)
			}
			break
		}
	}
	return out
}

// dryAdopt adopts the prefix kept onto def in memory: nothing is journaled.
func (e *Engine) dryAdopt(run RunID, kept []JournalRecord, def *Def) error {
	st, err := foldRun(kept, def)
	if err != nil {
		return err
	}
	if st.ended {
		return errors.New("the prefix ends the run")
	}
	c := &cycle{e: e, run: run, recs: kept, st: st, dry: true}
	return c.migrateAdopting(def)
}

// rewindUnchanged is dev rewind --to unchanged: the fork on stdout, the
// report on stderr.
func (c *cli) rewindUnchanged(e *Engine, stateDir string, run RunID, f *flags) int {
	var changed []string
	if f.changed != "" {
		changed = strings.Split(f.changed, ",")
	}
	rep, err := e.RewindUnchanged(run, changed, f.forkOut)
	for _, s := range rep.SteppedBy {
		fmt.Fprintf(c.stderr, "  stepped back: %s\n", s)
	}
	if err != nil {
		return c.fromError("dev rewind", err)
	}
	fmt.Fprintln(c.stdout, rep.Run)
	d := rep.Diverged
	at := d.State
	if d.Instance != "" {
		at += " (" + d.Instance + ")"
	}
	fmt.Fprintf(c.stderr, "first change: %s at seq %d (%s): %s\n", at, d.Seq, d.Kind, strings.Join(d.Why, ", "))
	fmt.Fprintf(c.stderr, "kept %d records of %s up to seq %d in %s\n", rep.Kept, run, rep.Cut, rep.Run)
	for _, a := range rep.Applied {
		fmt.Fprintf(c.stderr, "  applied after that point, not undone: %s\n", a)
	}
	fmt.Fprintf(c.stderr, "note: %s\n", rep.Caveat)
	if rep.Snapshot != nil {
		fmt.Fprintf(c.stderr, "restored checkpoint %d into %s; state: %s\n", rep.Snapshot.Seq, rep.Snapshot.Workspace, rep.Snapshot.StateDir)
		c.reportInstalled(*rep.Snapshot)
		return exitOK
	}
	if _, err := os.Stat(filepath.Join(stateDir, runIDFile)); err == nil {
		if err := os.WriteFile(filepath.Join(stateDir, runIDFile), []byte(string(rep.Run)+"\n"), 0o644); err != nil {
			return c.failf(exitInfra, "dev rewind: %v", err)
		}
	}
	lines, err := e.commandLines(rep.Run)
	if err != nil {
		return c.fromError("dev rewind", err)
	}
	fmt.Fprintf(c.stderr, "carry on: adopt the fork with:\n  %s\nthen the coordinator drives it with:\n  %s\n", lines("next", "--adopt"), lines("next"))
	return exitOK
}
