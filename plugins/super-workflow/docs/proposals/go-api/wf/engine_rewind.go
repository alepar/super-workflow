package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Rewind is a fork, never a truncation (converge E2). Rewind(run, "died")
// starts a new run whose journal is run's up to the point it died, plus a
// rewound record naming run and that point; run itself is left as it was.
// The fork resumes like a crashed run on whatever binary drives it next (with
// --adopt for a changed definition): the state that died is entered again,
// its decision and everything after it not done yet. Effects that applied
// after the point are named in the rewound record, not undone.
//
// The point "died" is, for an ended run, the state whose decision ended it:
// from the root's transition into its terminal, down through each Sub or Map
// child whose settling decided its parent, to the first state that decided on
// its own. For a run
// that has not ended, it is a halted instance's state; a run that neither
// ended nor halted has not died, and resume carries it on.

// RewoundData is the payload of rewound: the run this one was forked from,
// the last seq the fork kept, the state that died, and each effect that had
// applied after that point (it is not undone).
type RewoundData struct {
	From    RunID
	Seq     int64
	State   string
	Applied []string `json:",omitempty"`
	// ModelMap is the --model-map given for the fork, as on
	// ResumedData: nil carries the source's map on.
	ModelMap map[string]string `json:",omitempty"`
	// Source is a snapshot fork's source roots: a value from history bound
	// into an operational field is rebased from them (fork_rebase.go).
	Source *ForkSource `json:",omitempty"`
	// Omitted is what a snapshot restore left out of the fork on purpose,
	// each named by its adapter ("bd: dolt remote origin
	// (https://...)"): the fork diverges from its source there.
	Omitted []string `json:",omitempty"`
	// To is the rewind target the operator chose (dev rewind --to SEQ),
	// with OperatorChosen: the fork was cut at that checkpoint by hand, not
	// at the state that died.
	To             string `json:",omitempty"`
	OperatorChosen bool   `json:",omitempty"`
	// Relocated names each untyped run_started value the fork rewrote from
	// the source roots to its own ("Params.handoff"), an earlier revision.
	Relocated []string `json:",omitempty"`
}

// RewindReport is what Rewind made.
type RewindReport struct {
	Run  RunID // the fork
	Data RewoundData
}

// rewindLimit is how many forks of one run Rewind tries ids for.
const rewindLimit = 100

// Rewind forks run at point to; only "died" is supported.
func (e *Engine) Rewind(run RunID, to string) (RewindReport, error) {
	if to != "died" {
		return RewindReport{}, fmt.Errorf("%w: rewind: --to %q: only --to died is supported yet", ErrRejected, to)
	}
	if err := e.CheckBoundaryModelMap(run); err != nil {
		return RewindReport{}, err
	}
	var recs []JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return RewindReport{}, err
	}
	// A rewind reads the journal only, so an ended legacy run (which an
	// adoption refuses) is forked as recorded and its fork adopted; a run
	// that has not ended is adopted itself first.
	if !slices.ContainsFunc(recs, func(r JournalRecord) bool { return r.Kind == JournalRunEnded }) {
		if err := legacyRefusal("rewind", run, recs); err != nil {
			return RewindReport{}, err
		}
	}
	var started RunStartedData
	if err := json.Unmarshal(recs[0].Data, &started); err != nil {
		return RewindReport{}, err
	}
	if started.Snapshots {
		return RewindReport{}, fmt.Errorf("%w: snapshot runs require dev rewind --out <new directory>", ErrRejected)
	}
	chain, state, err := diedAt(recs)
	if err != nil {
		return RewindReport{}, fmt.Errorf("%w: rewind: run %s: %v", ErrRejected, run, err)
	}
	kept := forkRecords(recs, chain)
	d := RewoundData{From: run, Seq: kept[len(kept)-1].Seq, State: state, Applied: appliedDropped(recs, kept)}
	return e.writeRewind(run, kept, d)
}

// writeRewind writes kept and d as the run's next free fork id
// (<run>.rw<k>) and carries the source's run dir to it.
func (e *Engine) writeRewind(run RunID, kept []JournalRecord, d RewoundData) (RewindReport, error) {
	for k := 1; k <= rewindLimit; k++ {
		fork := RunID(fmt.Sprintf("%s.rw%d", run, k))
		done, err := e.writeFork(fork, kept, d)
		if err != nil {
			return RewindReport{}, err
		}
		if done {
			if e.opts.RunDir != nil {
				// The fork's run dir starts as the source's: its records,
				// detached effects' results, outputs and prompt bodies.
				for _, sub := range []string{"records", "effects", "out", "prompt"} {
					if err := copyTree(filepath.Join(e.opts.RunDir(run), sub), filepath.Join(e.opts.RunDir(fork), sub)); err != nil {
						// The fork is committed: the report names it, so
						// what follows the failure is the fork's.
						return RewindReport{Run: fork, Data: d}, fmt.Errorf("carrying run %s's %s to its fork %s: %w", run, sub, fork, err)
					}
				}
			}
			return RewindReport{Run: fork, Data: d}, nil
		}
	}
	return RewindReport{}, fmt.Errorf("%w: rewind: run %s already has %d forks", ErrRejected, run, rewindLimit)
}

// withRecords reads run's journal under its lock.
func (e *Engine) withRecords(run RunID, f func([]JournalRecord)) error {
	unlock, err := e.j.Lock(run)
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := e.j.Records(run)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("%w: %s", ErrUnknownRun, run)
	}
	f(recs)
	return nil
}

// writeFork writes prefix and the rewound record d as run fork. false when
// fork is taken by another run; a fork a crash left part-written is
// finished, and one already whole for the same point is the fork, so a
// rewind run again after a crash makes no second fork. A part-written fork
// is recognised only by its fork.pending marker (forkMarker), written
// before its first record and removed after its rewound record: an
// independent run whose records merely match the prefix (an explicit run id
// of the .rwN form) is another run's, never extended.
func (e *Engine) writeFork(fork RunID, prefix []JournalRecord, d RewoundData) (bool, error) {
	// The map was checked before any side effect (checkBoundaryModelMap).
	d.ModelMap = e.opts.BoundaryModelMap
	unlock, err := e.j.Lock(fork)
	if err != nil {
		return false, err
	}
	defer unlock()
	have, err := e.j.Records(fork)
	switch {
	case err == nil && len(have) == 0:
	case err == nil:
		if len(have) == len(prefix)+1 && have[len(have)-1].Kind == JournalRewound {
			// This rewind already wrote it whole (a crash after its last
			// record): it is the fork, when it forked the same point.
			var got RewoundData
			if json.Unmarshal(have[len(have)-1].Data, &got) == nil && forkIdentity(got) == forkIdentity(d) {
				return true, nil
			}
			return false, nil
		}
		if !e.forkPending(fork, d) || len(have) > len(prefix) || have[0].Kind != JournalRunStarted || string(have[0].Data) != string(prefix[0].Data) {
			return false, nil
		}
		for i := range have {
			if have[i].Kind != prefix[i].Kind || string(have[i].Data) != string(prefix[i].Data) {
				return false, nil
			}
		}
		prefix = prefix[len(have):]
	case errors.Is(err, ErrUnknownRun):
	default:
		return false, err
	}
	if len(have) == 0 {
		if err := e.markFork(fork, d); err != nil {
			return false, err
		}
	}
	for _, r := range prefix {
		r.Run = fork
		if err := e.j.Append(r); err != nil {
			return false, err
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	last := prefix
	if len(last) == 0 {
		last = have
	}
	at := last[len(last)-1]
	if err := e.j.Append(JournalRecord{Run: fork, Kind: JournalRewound, DefHash: at.DefHash, At: at.At, Data: raw}); err != nil {
		return false, err
	}
	return true, e.unmarkFork(fork)
}

// forkMarker is the file in a fork's run dir that says a fork of a source run
// at a seq is being written: its content is "<source> <seq>".
const forkMarker = "fork.pending"

// markFork durably records, before a fork's first record, that fork is
// being written from d's source and seq. An engine with no run dirs (an
// in-memory journal) records nothing and never finishes a partial fork.
func (e *Engine) markFork(fork RunID, d RewoundData) error {
	if e.opts.RunDir == nil {
		return nil
	}
	dir := e.opts.RunDir(fork)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, forkMarker), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprint(f, forkIdentity(d)); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// forkPending reports whether fork's marker names d's source and seq.
func (e *Engine) forkPending(fork RunID, d RewoundData) bool {
	if e.opts.RunDir == nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(e.opts.RunDir(fork), forkMarker))
	return err == nil && string(b) == forkIdentity(d)
}

// forkIdentity is what makes two forks the same fork: the source, the seq
// and the --model-map given, so a retry of one rewind is idempotent
// and the same point forked with another map is another fork. A fork with no
// map keeps the identity it had before maps.
func forkIdentity(d RewoundData) string {
	id := fmt.Sprintf("%s %d", d.From, d.Seq)
	if len(d.ModelMap) > 0 {
		id += " model-map " + mapText(d.ModelMap)
	}
	return id
}

// CheckBoundaryModelMap refuses the engine's BoundaryModelMap against run's
// definition (checkModelMap: keys and efforts only), before a rewind, fork or
// fanout does anything. A value is not checked here: the harness that
// will bind the new segment is not known yet, so the bind refuses it
// (bindSegment), and a corrupt run_started is the bind's to find too. No map
// is never refused.
func (e *Engine) CheckBoundaryModelMap(run RunID) error {
	if len(e.opts.BoundaryModelMap) == 0 {
		return nil
	}
	var first JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { first = rs[0] }); err != nil {
		return err
	}
	def, err := e.defFor(first)
	if err != nil {
		return err
	}
	return checkModelMap(def, e.opts.BoundaryModelMap)
}

// unmarkFork removes fork's marker once its rewound record is written.
func (e *Engine) unmarkFork(fork RunID) error {
	if e.opts.RunDir == nil {
		return nil
	}
	if err := os.Remove(filepath.Join(e.opts.RunDir(fork), forkMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cutAt is one instance of the chain that died and the index of its first
// record the fork drops: the dying instance's arrival at the state that died,
// and each ancestor's settling of the chain child that decided it.
type cutAt struct {
	inst string
	from int
	// decided is where the ancestor decided (its first record after its last
	// child settled): what other instances journaled from there on, the
	// decision caused. The dying instance's is its from.
	decided int
}

// diedAt is the chain of instances whose decisions ended the run, each with
// where its dropped records start, and the qualified state that died.
func diedAt(recs []JournalRecord) ([]cutAt, string, error) {
	terminal := map[string]string{} // instance to the terminal it ended at
	failed := map[string]bool{}     // instance that settled other than in success
	ended, cause := false, ""
	for _, r := range recs {
		switch r.Kind {
		case JournalRunEnded:
			var d RunEndedData
			if json.Unmarshal(r.Data, &d) == nil && r.Instance == "" {
				terminal[""], ended, cause = d.Terminal, true, d.Cause
			}
		case JournalChildSettled:
			var d ChildSettledData
			if json.Unmarshal(r.Data, &d) == nil {
				terminal[d.ID] = d.Terminal
				failed[d.ID] = !d.Outcome.Delivered()
			}
		}
	}
	if !ended {
		return haltedAt(recs)
	}
	if terminal[""] == "" {
		return nil, "", fmt.Errorf("it ended with no terminal (%s): nothing decided its end, so there is no state to run again", cause)
	}
	inst, path := "", []string{}
	var chain []cutAt
	for depth := 0; depth < 64; depth++ {
		term := terminal[inst]
		into := -1 // the instance's transition into its terminal
		for i := len(recs) - 1; i >= 0; i-- {
			if r := recs[i]; r.Kind == JournalTransition && r.Instance == inst && transitionTo(r) == term && term != "" {
				into = i
				break
			}
		}
		if into < 0 {
			return nil, "", fmt.Errorf("found no transition of %s into its terminal %q", instName(inst), term)
		}
		state := recs[into].State
		arrive := arrival(recs, inst, state, into)
		// A Sub or Map state decided by its children's settling: descend into
		// the child that failed (the last to), else the last to settle.
		child, settledAt, lastSettle := "", -1, -1
		for i := arrive; i < into; i++ {
			if r := recs[i]; r.Kind == JournalChildSettled && r.Instance == inst {
				lastSettle = i
				var d ChildSettledData
				if json.Unmarshal(r.Data, &d) != nil || terminal[d.ID] == "" {
					continue
				}
				if child == "" || failed[d.ID] || !failed[child] {
					child, settledAt = d.ID, i
				}
			}
		}
		path = append(path, state)
		if child == "" {
			return append(chain, cutAt{inst, arrive, arrive}), strings.Join(path, " / "), nil
		}
		decided := into
		for i := lastSettle + 1; i <= into; i++ {
			if recs[i].Instance == inst {
				decided = i
				break
			}
		}
		chain = append(chain, cutAt{inst, settledAt, decided})
		inst = child
	}
	return nil, "", fmt.Errorf("the chain of children that ended it is too deep")
}

// haltedAt is the chain of a run that has not ended: its last instance still
// halted (no retried and no move after the halt).
func haltedAt(recs []JournalRecord) ([]cutAt, string, error) {
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if r.Kind != JournalRefused {
			continue
		}
		var d RefusedData
		if json.Unmarshal(r.Data, &d) != nil || !d.Halt || d.Recovery {
			continue // a recovery offer is the coordinator's to answer: resume carries it on
		}
		cleared := false
		for _, later := range recs[i+1:] {
			if later.Instance == r.Instance && (later.Kind == JournalRetried || later.Kind == JournalTransition) {
				cleared = true
				break
			}
		}
		if cleared {
			continue
		}
		where := r.State
		if r.Instance != "" {
			where = r.Instance + " / " + r.State
		}
		at := arrival(recs, r.Instance, r.State, i)
		return []cutAt{{r.Instance, at, at}}, where, nil
	}
	return nil, "", fmt.Errorf("it has not died (no terminal and no halt left): resume it, with --adopt on a changed binary")
}

// forkRecords is the fork's journal before its rewound record: every record
// of the source, except each chain instance's from its cut on and every
// other instance's after the first ancestor's decision (what the decisions
// caused: withdrawals, late votes), each replaced by a note so the kept records keep
// their seqs (an effect result names its intent by seq). Nothing after the
// last kept record is written.
func forkRecords(recs []JournalRecord, chain []cutAt) []JournalRecord {
	from := map[string]int{}
	first := len(recs) // the first ancestor's decision; the run's own when it died alone
	for i, c := range chain {
		from[c.inst] = c.from
		if i < len(chain)-1 || len(chain) == 1 {
			first = min(first, c.decided)
		}
	}
	drop := make([]bool, len(recs))
	for i, r := range recs {
		f, inChain := from[r.Instance]
		switch {
		case r.Kind == JournalRunEnded && r.Instance == "":
			drop[i] = true
		case inChain && i >= f:
			drop[i] = true
		case !inChain && i > first && r.Instance != "":
			drop[i] = true
		}
	}
	// A child whose spawn is dropped is not in the fork, nor is any of its
	// descendants: drop their every record, so the fork never names an
	// instance it does not spawn. Its state spawns them again.
	gone := map[string]bool{}
	for i, r := range recs {
		if r.Kind != JournalChildSpawned {
			continue
		}
		var d ChildSpawnedData
		if json.Unmarshal(r.Data, &d) != nil {
			continue
		}
		if drop[i] || gone[r.Instance] {
			gone[d.ID] = true
		}
	}
	// A cleanup confirmation cannot survive its deferred record, even when
	// a child-only rewind otherwise keeps the root's records.
	deferredDropped := map[int64]bool{}
	for i, r := range recs {
		if r.Kind == JournalCleanupDeferred {
			deferredDropped[r.Seq] = drop[i] || gone[r.Instance]
		}
		if r.Kind == JournalCleanupApplied {
			var d CleanupAppliedData
			if json.Unmarshal(r.Data, &d) == nil && deferredDropped[d.Deferred] {
				drop[i] = true
			}
		}
	}
	last := -1
	for i, r := range recs {
		if gone[r.Instance] {
			drop[i] = true
		}
		if !drop[i] {
			last = i
		}
	}
	var out []JournalRecord
	for i, r := range recs[:last+1] {
		if drop[i] {
			raw, _ := json.Marshal(NoteData{Text: fmt.Sprintf("rewound: seq %d (%s of %s) not carried into this fork", r.Seq, r.Kind, instName(r.Instance))})
			r = JournalRecord{Seq: r.Seq, Kind: JournalNote, DefHash: r.DefHash, At: r.At, Data: raw}
		}
		out = append(out, r)
	}
	return out
}

// arrival is the index of the first record of instance inst at state after
// its last arrival there before index before: the state's work starts there.
func arrival(recs []JournalRecord, inst, state string, before int) int {
	from := 0
	for i := before - 1; i >= 0; i-- {
		if r := recs[i]; r.Instance == inst && r.Kind == JournalTransition && transitionTo(r) == state {
			from = i + 1
			break
		}
	}
	for i := from; i < before; i++ {
		if recs[i].Instance == inst && recs[i].State == state {
			return i
		}
	}
	return before
}

func transitionTo(r JournalRecord) string {
	var d TransitionData
	_ = json.Unmarshal(r.Data, &d)
	return d.To
}

func instName(inst string) string {
	if inst == "" {
		return "the run"
	}
	return inst
}

// appliedDropped names each effect whose applied result the fork dropped
// (it is not undone).
func appliedDropped(recs, kept []JournalRecord) []string {
	keptSeq := map[int64]bool{}
	for _, r := range kept {
		if r.Kind != JournalNote {
			keptSeq[r.Seq] = true
		}
	}
	intents := map[int64]JournalRecord{}
	for _, r := range recs {
		if r.Kind == JournalEffectIntent || r.Kind == JournalCleanupDeferred {
			intents[r.Seq] = r
		}
	}
	var out []string
	for _, r := range recs {
		if (r.Kind != JournalEffectResult && r.Kind != JournalCleanupApplied) || keptSeq[r.Seq] {
			continue
		}
		var d EffectResultData
		err := json.Unmarshal(r.Data, &d)
		if r.Kind == JournalCleanupApplied {
			var cleanup CleanupAppliedData
			err = json.Unmarshal(r.Data, &cleanup)
			d = EffectResultData{Intent: cleanup.Deferred, OK: true}
		}
		if err != nil || !d.OK {
			continue
		}
		var in EffectIntentData
		_ = json.Unmarshal(intents[d.Intent].Data, &in)
		desc := in.Effect.Adapter + " " + in.Effect.Op
		if argv, ok := in.Effect.Args["argv"].([]any); ok && len(argv) > 0 {
			desc += fmt.Sprintf(" %v", argv[0])
			if len(argv) > 1 {
				if s, ok := argv[1].(string); ok && len(s) < 40 {
					desc += " " + s
				}
			}
		}
		out = append(out, fmt.Sprintf("seq %d: %s (state %s)", d.Intent, strings.TrimSpace(desc), intents[d.Intent].State))
	}
	return out
}

// RewindInto restores the latest checkpoint at or before the dying state's
// arrival. Unlike legacy branch-selective rewind, this rolls the whole world
// and whole journal back together to one chronological frontier.
func (e *Engine) RewindInto(run RunID, to, out string) (SnapshotFork, error) {
	if to != "died" {
		return e.rewindToCheckpoint(run, to, out)
	}
	if err := e.CheckBoundaryModelMap(run); err != nil {
		return SnapshotFork{}, err
	}
	var recs []JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return SnapshotFork{}, err
	}
	chain, _, err := diedAt(recs)
	if err != nil {
		return SnapshotFork{}, err
	}
	cutoff := chain[len(chain)-1].from
	// arrival's first record may itself be the before-offer checkpoint.
	if cutoff >= len(recs) {
		cutoff = len(recs) - 1
	}
	for i := cutoff; i >= 0; i-- {
		if recs[i].Kind != JournalWorldSnapshot {
			continue
		}
		var data WorldSnapshotData
		if err := json.Unmarshal(recs[i].Data, &data); err != nil {
			return SnapshotFork{}, err
		}
		if data.Unavailable != "" {
			continue
		}
		return e.ForkSnapshot(run, recs[i].Seq, out)
	}
	return SnapshotFork{}, fmt.Errorf("%w: no historical world snapshot before the dying state; current world cannot replace it", ErrUnavailable)
}

// rewindToCheckpoint forks run at the checkpoint the operator named by its
// journal seq: the prefix up to it, the world from it, everything after it
// left out of the fork (the source is only read). A journaled failure after
// the checkpoint is then run again in the fork, not re-decided from its
// record as --to died does. A seq that is no available checkpoint is
// refused, naming the nearest ones before and after it.
func (e *Engine) rewindToCheckpoint(run RunID, to, out string) (SnapshotFork, error) {
	seq, err := strconv.ParseInt(to, 10, 64)
	if err != nil || seq < 1 {
		return SnapshotFork{}, fmt.Errorf("%w: --to takes died or a checkpoint's journal seq, not %q", ErrRejected, to)
	}
	var recs []JournalRecord
	if err := e.withRecords(run, func(rs []JournalRecord) { recs = rs }); err != nil {
		return SnapshotFork{}, err
	}
	if why := checkpointAt(recs, seq); why != "" {
		return SnapshotFork{}, fmt.Errorf("%w: %s", ErrRejected, why)
	}
	return e.forkSnapshot(run, seq, out, func(d *RewoundData) { d.To, d.OperatorChosen = to, true })
}

// checkpointAt is why record seq of recs is no restorable checkpoint ("" when
// it is), naming the nearest available checkpoints before and after it.
func checkpointAt(recs []JournalRecord, seq int64) string {
	available := func(r JournalRecord) bool {
		var w WorldSnapshotData
		return r.Kind == JournalWorldSnapshot && json.Unmarshal(r.Data, &w) == nil && w.Unavailable == ""
	}
	if seq <= int64(len(recs)) && available(recs[seq-1]) {
		return ""
	}
	why := fmt.Sprintf("record %d is not a checkpoint", seq)
	switch {
	case seq > int64(len(recs)):
		why = fmt.Sprintf("record %d is past the journal's end (%d)", seq, len(recs))
	case recs[seq-1].Kind == JournalWorldSnapshot:
		why = fmt.Sprintf("record %d is not a checkpoint: its world was not captured", seq)
	default:
		why = fmt.Sprintf("record %d (%s) is not a checkpoint", seq, recs[seq-1].Kind)
	}
	name := func(r JournalRecord) string {
		if r.State != "" {
			return fmt.Sprintf("%d (before %s)", r.Seq, r.State)
		}
		return strconv.FormatInt(r.Seq, 10)
	}
	before, after := "", ""
	for _, r := range recs {
		if !available(r) {
			continue
		}
		if r.Seq < seq {
			before = name(r)
		} else if after == "" && r.Seq > seq {
			after = name(r)
		}
	}
	switch {
	case before == "" && after == "":
		return why + "; the run has no available checkpoint"
	case before == "":
		return why + "; the nearest checkpoint is " + after + ", after it"
	case after == "":
		return why + "; the nearest checkpoint is " + before + ", before it"
	}
	return why + "; the nearest checkpoints are " + before + " before it and " + after + " after it"
}
