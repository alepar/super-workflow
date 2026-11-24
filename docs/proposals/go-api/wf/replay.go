package wf

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Replay runs a recorded run's journal against a definition, with no model
// and no adapter (converge E3): a fresh in-memory engine starts def with the
// run's params and options, every read is answered by the run's recorded
// observation and every effect by its recorded result, and each unit the
// engine issues gets the run's recorded action for that instance and state
// (its submissions with their artifacts, its failures, a lost attempt), each
// gate its recorded answer. The source run is only read.
//
// Transitions are compared by instance, state and visit: each time the
// definition leaves a state on a different edge than the run did there, that
// is a difference, and the replay carries on, so a refactor upstream (a state
// added or removed) does not hide what happens at the point of death. It
// stops where the run has no record to feed the definition (a unit, read,
// effect or gate) or the run ends. The report says whether the definition
// passed the state the run died at: a fix for a run defect is shown to take
// the other route there before any model runs, and a kept journal becomes a
// regression case.
//
// A lost attempt is replayed as a transient failure of the same attempt; a
// run that succeeded another (its records copied in) is not replayed.
func Replay(records []JournalRecord, def *Def, sourceRunDir string) (ReplayReport, error) {
	return ReplayWith(records, def, sourceRunDir, ReplayOptions{})
}

// ReplayOptions changes what a replay may answer that the run never recorded.
type ReplayOptions struct {
	// Observe answers a read the recorded run never made: a definition that
	// added a read upstream of the point a kept journal tests (a new early
	// observation) then still reaches that point, deliberately. ok false
	// leaves the read unanswered, which stops the replay as before. Each
	// answer it gives is listed in the report's Injected.
	Observe func(d ObservationDesc) (value json.RawMessage, ok bool)
	// SameKey answers an effect whose adapter, op and key the run applied
	// but whose arguments changed with the run's recorded result: a
	// definition that moved an argument (text to stdin, say) still reaches
	// the point a kept journal tests. Each such effect is listed in the
	// report's Rebound. Off, a changed effect stops the replay.
	SameKey bool
}

// ReplayFixtureEntry is one answer of a replay fixture: a read the run never
// recorded, by adapter and op, whose arguments (as JSON) contain every string
// in Contains, is answered with Value. An entry with no Contains answers every
// unrecorded read of that adapter and op.
type ReplayFixtureEntry struct {
	Adapter  string          `json:"adapter"`
	Op       string          `json:"op"`
	Contains []string        `json:"contains"`
	Value    json.RawMessage `json:"value"`
}

// ReplayFixture reads a fixture file (a JSON list of ReplayFixtureEntry) as a
// ReplayOptions.Observe: the first entry that matches a read answers it. The
// replay lists every read it answered, so a fixture never hides what it fed.
func ReplayFixture(path string) (func(ObservationDesc) (json.RawMessage, bool), error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay fixture: %w", err)
	}
	var entries []ReplayFixtureEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("replay fixture %s: %w", path, err)
	}
	for i, e := range entries {
		if e.Adapter == "" || e.Op == "" || len(e.Value) == 0 {
			return nil, fmt.Errorf("replay fixture %s: entry %d needs adapter, op and value", path, i)
		}
	}
	return func(d ObservationDesc) (json.RawMessage, bool) {
		// Unescaped, so a contains string with <, > or & matches as written.
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(d.Args)
		args := buf.String()
		for _, e := range entries {
			if e.Adapter != d.Adapter || e.Op != d.Op {
				continue
			}
			match := true
			for _, c := range e.Contains {
				if !strings.Contains(args, c) {
					match = false
					break
				}
			}
			if match {
				return e.Value, true
			}
		}
		return nil, false
	}, nil
}

// ReplayWith is Replay with opts.
func ReplayWith(records []JournalRecord, def *Def, sourceRunDir string, opts ReplayOptions) (ReplayReport, error) {
	if len(records) == 0 || records[0].Kind != JournalRunStarted {
		return ReplayReport{}, errors.New("replay: the journal does not start with run_started")
	}
	if err := legacyRefusal("replay", records[0].Run, records); err != nil {
		return ReplayReport{}, err
	}
	records, err := adoptedNames(records, def)
	if err != nil {
		return ReplayReport{}, fmt.Errorf("replay: %w", err)
	}
	d, err := decodeData(records[0])
	if err != nil {
		return ReplayReport{}, fmt.Errorf("replay: %w", err)
	}
	started := d.(*RunStartedData)
	// Raw on-disk journals carry the workspace transport token. Replay has no
	// rooted journal reader, so recover its declared origin before binding paths.
	if records[0].RelativePaths && records[0].Roots != nil {
		started.Workspace = strings.ReplaceAll(started.Workspace, workspacePathToken, records[0].Roots.Workspace)
	}
	if started.Predecessor != "" {
		return ReplayReport{}, fmt.Errorf("replay: %s succeeds %s; replay its first run", records[0].Run, started.Predecessor)
	}
	if started.Def != def.name {
		return ReplayReport{}, fmt.Errorf("replay: the run is of %s, not %s", started.Def, def.name)
	}
	dir, err := os.MkdirTemp("", "wf-replay-")
	if err != nil {
		return ReplayReport{}, err
	}
	defer os.RemoveAll(dir)
	r := &replay{rec: records, def: def, run: records[0].Run, started: started, sourceDir: sourceRunDir, opts: opts,
		oldDir: recordedRunDir(records, sourceRunDir), newDir: filepath.Join(dir, "runs", string(records[0].Run))}
	r.report = ReplayReport{Run: r.run, Then: records[0].DefHash, Now: def.Hash()}
	r.index()
	rep, err := r.drive()
	rep.Verdict = rep.verdict()
	return rep, err
}

// ReplayReport is what Replay found.
type ReplayReport struct {
	// Stale records changed prompts; outputs are still fed and verdicts are unaffected.
	Stale     []ReplayDifference
	Run       RunID
	Then, Now string // the recorded and the replayed definition hashes
	Retraced  int    // transitions the definition took as the run did
	// CoordinatorContinuations counts issued Stop decisions in the source journal.
	CoordinatorContinuations int `json:",omitempty"`
	// Differences are the transitions the definition took otherwise than the
	// run, in the order it took them: Kind "route", or "refusal" for an
	// output an older engine refused with the attempt open that the
	// definition now classes otherwise.
	Differences []ReplayDifference
	// Stop is why the replay ended: Kind "unit", "read", "effect" or "gate"
	// (the run has no record of it), "ended" (the definition's run ended) or
	// "halted".
	Stop ReplayDifference
	// Died is the state the run died at ("" for a run that did not die), and
	// Passed the definition's way out of it when that is not the run's.
	Died, Passed string
	// Verdict says what the replay shows about the point of death (see
	// Verdict*): a definition that passes it, one that ends there as the run
	// did, or a replay that stopped first for want of a record, which shows
	// neither (converge E5).
	Verdict string
	Notes   []string // approximations the replay made (lost attempts)
	// Rebound are the effects ReplayOptions.SameKey answered from a recorded
	// effect with the same key and other arguments, each once by its key.
	Rebound []string
	// Injected are the reads the run never made that ReplayOptions.Observe
	// answered, each once by its full key (adapter, op and arguments), in the
	// order the definition first made them; the text summarizes them.
	Injected []string
}

// ReplayDifference is a point where the definition and the run part.
type ReplayDifference struct {
	Kind, Instance, State string
	Then, Now             string
}

func (d ReplayDifference) where() string {
	if d.Instance != "" {
		return d.Instance + "/" + d.State
	}
	return d.State
}

// The verdicts of a replay of a run that died.
const (
	VerdictPasses       = "passes"        // the definition left the state of death another way
	VerdictFails        = "does not pass" // the definition ended or halted, the run's death not passed
	VerdictInconclusive = "inconclusive"  // the replay stopped short for want of a record
)

// verdict is the report's verdict, "" for a run that did not die. A replay
// that ran out of records (a read, a unit, an effect or a gate the run never
// recorded) before the definition passed the point of death is
// inconclusive, never a failed pass: the live definition may well pass it.
func (r ReplayReport) verdict() string {
	switch {
	case r.Died == "":
		return ""
	case r.Passed != "":
		return VerdictPasses
	case r.Stop.Kind == "ended" && (strings.HasSuffix(r.Stop.Now, "("+string(Success)+")") || strings.HasSuffix(r.Stop.Now, "("+string(Partial)+")")):
		return VerdictPasses // it left the run's path before the death and delivered
	case r.Stop.Kind == "ended" || r.Stop.Kind == "halted":
		return VerdictFails
	}
	return VerdictInconclusive
}

func (r ReplayReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replay %s: recorded on %s, replayed on %s\n", r.Run, r.Then, r.Now)
	fmt.Fprintf(&b, "retraced %d transitions; %d differ\n", r.Retraced, len(r.Differences))
	if r.CoordinatorContinuations > 0 {
		fmt.Fprintf(&b, "recorded coordinator continuations: %d\n", r.CoordinatorContinuations)
	}
	fmt.Fprintf(&b, "%d stale recordings (outputs still fed)\n", len(r.Stale))
	for _, d := range r.Stale {
		fmt.Fprintf(&b, "stale %s: %s -> %s\n", d.where(), d.Then, d.Now)
	}
	for _, d := range r.Differences {
		fmt.Fprintf(&b, "at %s\n  then: %s\n  now:  %s\n", d.where(), d.Then, d.Now)
	}
	switch r.Verdict {
	case VerdictPasses:
		fmt.Fprintf(&b, "died at %s; the definition passes it: %s\n", r.Died, cmp.Or(r.Passed, r.Stop.Now))
	case VerdictFails:
		fmt.Fprintf(&b, "died at %s; the definition does not pass it\n", r.Died)
	case VerdictInconclusive:
		why := map[string]string{
			"read":   "a read the run never made (answer it with --observe)",
			"unit":   "an agent answer the run never gave (a check refused the recorded one, or the state is new)",
			"effect": "an effect the run never applied",
			"gate":   "a gate answer the run never gave",
		}[r.Stop.Kind]
		what := "new " + r.Stop.Kind
		switch {
		case r.Stop.Kind == "read":
			what += " " + r.Stop.Now
		case r.Stop.Kind == "unit" && strings.HasPrefix(r.Stop.Then, "recorded result refused by "):
			what, why = r.Stop.Then, "the run has no other answer to follow"
		}
		fmt.Fprintf(&b, "died at %s; inconclusive: %s at %s: %s\n", r.Died, what, r.Stop.where(), cmp.Or(why, "no record to feed"))
	}
	fmt.Fprintf(&b, "replay stopped (%s) at %s: %s\n", r.Stop.Kind, r.Stop.where(), r.Stop.Now)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, n := range r.Injected {
		fmt.Fprintf(&b, "injected (the run never read it): %s\n", summarize(n))
	}
	for _, n := range r.Rebound {
		fmt.Fprintf(&b, "rebound (same key, other arguments): %s\n", n)
	}
	return b.String()
}

type replayAction struct {
	promptHash, promptShapeHash string
	kind                        string // "submit", "validate", "fail", "lost", "missing"
	issue                       int    // the unit's issue (attempt) the action was recorded on
	output                      json.RawMessage
	artifacts                   map[string]ArtifactRef
	class                       FailureClass
	reason                      string
}

type replayStep struct{ to, label string }

type replayBreakpoint struct {
	instance, state string
	visit           int
}

type replay struct {
	rec     []JournalRecord
	def     *Def
	opts    ReplayOptions
	run     RunID
	started *RunStartedData
	// oldDir is the run directory the journal's paths name, which a copied
	// state dir no longer is; sourceDir is where the run's files are now.
	oldDir, newDir, sourceDir string

	actions map[string][]replayAction     // instance|state -> the agent's actions, in order
	answers map[string][]GateAnsweredData // instance|state -> gate answers
	steps   map[string]replayStep         // instance|state|visit -> where the run went
	reads   map[string][]ObservedData     // read key -> recorded observations
	effects map[string][]EffectResultData
	byKey   map[string][]EffectResultData // "adapter.op key=<key>" -> recorded results, for SameKey
	refused map[string]string             // instance|state -> why the definition refused its recorded result

	breakpointResumes map[replayBreakpoint]time.Time

	engine *Engine
	j      Journal
	now    time.Time
	seen   int            // new journal records already compared
	visits map[string]int // instance|state -> times the definition left it
	miss   *ReplayDifference
	// diedInst and diedState are the instance and state the run died at.
	diedInst, diedState string
	report              ReplayReport
}

func replayKey(instance, state string) string { return instance + "|" + state }

// recordedRunDir is the run directory the journal's own paths name (a
// prompt_fetched record's PromptDir lies under it), else fallback.
func recordedRunDir(recs []JournalRecord, fallback string) string {
	for _, rec := range recs {
		if rec.Kind != JournalPromptFetched {
			continue
		}
		var d PromptFetchedData
		if json.Unmarshal(rec.Data, &d) == nil {
			if i := strings.Index(d.PromptDir, "/runs/"+string(rec.Run)+"/"); i >= 0 {
				return d.PromptDir[:i+len("/runs/")+len(rec.Run)]
			}
		}
	}
	return fallback
}

func lastSegment(s string) string { return s[strings.LastIndex(s, "/")+1:] }

// index sorts the recorded run into what the replay hands out.
func (r *replay) index() {
	r.actions, r.answers, r.steps = map[string][]replayAction{}, map[string][]GateAnsweredData{}, map[string]replayStep{}
	r.reads, r.effects, r.byKey = map[string][]ObservedData{}, map[string][]EffectResultData{}, map[string][]EffectResultData{}
	r.refused = map[string]string{}
	var pendingBreakpoint *replayBreakpoint
	left := map[string]int{}
	prompts := map[string]PromptFetchedData{}
	intents := map[int64]EffectData{}
	running := map[string]bool{} // instance -> an attempt has started since its last issue
	issues := map[string]int{}   // instance|state -> its instruction_issued records so far
	for i, rec := range r.rec {
		key := replayKey(rec.Instance, rec.State)
		switch rec.Kind {
		case JournalCoordinatorContinued:
			r.report.CoordinatorContinuations++
		case JournalInstructionIssued:
			delete(prompts, key)
			running[rec.Instance] = false
			issues[key]++
		case JournalAttemptStarted:
			running[rec.Instance] = true
		case JournalPromptFetched:
			var d PromptFetchedData
			if json.Unmarshal(rec.Data, &d) == nil {
				prompts[key] = d
			}
		case JournalSubmitted:
			var d SubmittedData
			if json.Unmarshal(rec.Data, &d) == nil {
				r.actions[key] = append(r.actions[key], replayAction{kind: "submit", issue: issues[key], output: d.Output, artifacts: d.Artifacts, promptHash: cmp.Or(d.PromptHash, prompts[key].PromptHash), promptShapeHash: cmp.Or(d.PromptShapeHash, prompts[key].PromptShapeHash)})
			}
		case JournalSubmissionRefused:
			// An older engine refused the submission and left the attempt
			// open; a child now checks such an output with validate (C5).
			// The refusal answers a submission of its own attempt only: an
			// earlier attempt's spent submission stays a submit. A first
			// missing-output refusal answers no submission and adds nothing;
			// a second one spends the attempt (JournalRefused below).
			var d SubmissionRefusedData
			if json.Unmarshal(rec.Data, &d) != nil || d.Fingerprint == missingOutputFP {
				continue
			}
			if acts := r.actions[key]; len(acts) > 0 && acts[len(acts)-1].kind == "submit" && acts[len(acts)-1].issue == issues[key] {
				acts[len(acts)-1].kind = "validate"
				acts[len(acts)-1].reason = strings.Join(d.Reasons, "; ")
			}
		case JournalRefused:
			var d RefusedData
			if json.Unmarshal(rec.Data, &d) != nil || d.Halt || !running[rec.Instance] || submittedSince(r.rec[:i], rec.Instance) {
				continue
			}
			if d.Submitted {
				// The engine's refusal of a done. One with no output file has
				// no submitted record to replay, and spent the attempt.
				if d.Fingerprint == missingOutputFP {
					r.actions[key] = append(r.actions[key], replayAction{kind: "missing", issue: issues[key], reason: strings.Join(d.Reasons, "; ")})
					running[rec.Instance] = false
				}
				continue
			}
			class, reason := FailTransient, strings.TrimPrefix(strings.Join(d.Reasons, "; "), "abandoned: ")
			if rec.Class == Blocked {
				class = FailBlocked
			}
			r.actions[key] = append(r.actions[key], replayAction{kind: "fail", class: class, reason: reason})
			running[rec.Instance] = false
		case JournalAttemptLost:
			var d AttemptLostData
			_ = json.Unmarshal(rec.Data, &d)
			r.actions[key] = append(r.actions[key], replayAction{kind: "lost", reason: d.Cause})
			running[rec.Instance] = false
		case JournalGateAnswered:
			var d GateAnsweredData
			if json.Unmarshal(rec.Data, &d) == nil {
				r.answers[key] = append(r.answers[key], d)
			}
		case JournalBreakpointHalted:
			var d BreakpointHaltedData
			if json.Unmarshal(rec.Data, &d) == nil {
				pendingBreakpoint = &replayBreakpoint{rec.Instance, d.State, d.Visit}
			}
		case JournalResumed:
			if pendingBreakpoint != nil {
				if r.breakpointResumes == nil {
					r.breakpointResumes = map[replayBreakpoint]time.Time{}
				}
				r.breakpointResumes[*pendingBreakpoint] = rec.At
				pendingBreakpoint = nil
			}
		case JournalTransition:
			var d TransitionData
			if json.Unmarshal(rec.Data, &d) == nil {
				k := replayKey(rec.Instance, rec.State)
				left[k]++
				r.steps[fmt.Sprintf("%s|%d", k, left[k])] = replayStep{d.To, planLabel(r.rec[:i], rec.Instance, rec.State, d.To)}
			}
		case JournalObserved:
			var d ObservedData
			if json.Unmarshal(rec.Data, &d) == nil && !d.Alias {
				k := readKey(d.Desc.Adapter, d.Desc.Op, r.recorded(d.Desc.Args))
				r.reads[k] = append(r.reads[k], d)
			}
		case JournalEffectIntent, JournalCleanupDeferred:
			var d EffectIntentData
			if json.Unmarshal(rec.Data, &d) == nil {
				intents[rec.Seq] = d.Effect
			}
		case JournalEffectResult, JournalCleanupApplied:
			var d EffectResultData
			err := json.Unmarshal(rec.Data, &d)
			if rec.Kind == JournalCleanupApplied {
				var cleanup CleanupAppliedData
				err = json.Unmarshal(rec.Data, &cleanup)
				d = EffectResultData{Intent: cleanup.Deferred, OK: true, Outcome: "done"}
			}
			if err == nil {
				if eff, ok := intents[d.Intent]; ok {
					k := effectReplayKey(eff.Adapter, eff.Op, eff.Key, r.recorded(eff.Args))
					r.effects[k] = append(r.effects[k], d)
					if eff.Key != "" {
						sk := eff.Adapter + "." + eff.Op + " key=" + eff.Key
						r.byKey[sk] = append(r.byKey[sk], d)
					}
				}
			}
		}
	}
}

// submittedSince reports whether instance submitted since its last issue: a
// refusal after a submission is the engine's answer to it, not a failure the
// agent reported.
func submittedSince(recs []JournalRecord, instance string) bool {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Instance != instance {
			continue
		}
		switch recs[i].Kind {
		case JournalInstructionIssued:
			return false
		case JournalSubmitted:
			return true
		}
	}
	return false
}

// planLabel is the label of the edge instance's last decision took from state
// to to, "" when no plan names it.
func planLabel(recs []JournalRecord, instance, state, to string) string {
	for i := len(recs) - 1; i >= 0; i-- {
		rec := recs[i]
		if rec.Instance != instance {
			continue
		}
		if rec.Kind == JournalTransition {
			return ""
		}
		var d struct{ Plan *EdgePlan }
		if json.Unmarshal(rec.Data, &d) == nil && d.Plan != nil {
			for _, s := range d.Plan.Steps {
				if s.From == state && s.To == to {
					return s.Label
				}
			}
		}
	}
	return ""
}

func readKey(adapter, op string, args map[string]any) string {
	b, _ := json.Marshal(args)
	return adapter + "." + op + " " + string(b)
}

// effectReplayKey matches an effect to its recorded result by adapter, op,
// key and arguments: a keyed effect whose arguments changed is a different
// operation the run never applied.
func effectReplayKey(adapter, op, key string, args map[string]any) string {
	return readKey(adapter, op, args) + " key=" + key
}

// recorded canonicalizes both recorded and replayed adapter arguments. Raw
// journals mix owned-path transport tokens with absolute argv/domain strings;
// decoded or relocated journals may add another spelling of the same roots.
// This is a matching view only: historical arguments and values stay unchanged.
func (r *replay) recorded(args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	b, err := json.Marshal(args)
	if err != nil {
		return args
	}
	if r.started != nil {
		resolved, _, resolveErr := recordedOwnedPathView(b, &RecordedPathView{Workspace: r.started.Workspace, RunDir: r.newDir})
		if resolveErr == nil {
			b = resolved
		}
	}
	canonicalRun := filepath.Join(statePathToken, "runs", string(r.run))
	aliases := map[string]string{r.oldDir: canonicalRun, r.newDir: canonicalRun}
	if r.started != nil {
		aliases[r.started.Workspace] = workspacePathToken
	}
	if len(r.rec) > 0 && r.rec[0].Roots != nil {
		roots := r.rec[0].Roots
		aliases[roots.Workspace] = workspacePathToken
		aliases[roots.State] = statePathToken
		if roots.State != "" {
			aliases[filepath.Join(roots.State, "runs", string(r.run))] = canonicalRun
		}
	}
	delete(aliases, "")
	keys := make([]string, 0, len(aliases))
	for key := range aliases {
		keys = append(keys, key)
	}
	// Match nested run/state paths before workspace, without cascading replacements.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	pairs := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		from, _ := json.Marshal(key)
		to, _ := json.Marshal(aliases[key])
		pairs = append(pairs, string(from[1:len(from)-1]), string(to[1:len(to)-1]))
	}
	var out map[string]any
	if json.Unmarshal([]byte(strings.NewReplacer(pairs...).Replace(string(b))), &out) != nil {
		return args
	}
	return out
}

// replayWorld answers the replay's adapter calls from the recorded run.
type replayWorld struct {
	r     *replay
	reads map[string]int
	apps  map[string]int
}

func (w *replayWorld) Ops() []string { return []string{"*"} }

func (w *replayWorld) Read(_ string, d ObservationDesc) (json.RawMessage, error) {
	k := readKey(d.Adapter, d.Op, w.r.recorded(d.Args))
	got := w.r.reads[k]
	if len(got) == 0 && w.r.opts.Observe != nil {
		if v, ok := w.r.opts.Observe(d); ok {
			if !slices.Contains(w.r.report.Injected, k) {
				w.r.report.Injected = append(w.r.report.Injected, k)
			}
			return v, nil
		}
	}
	if len(got) == 0 {
		w.r.missed("read", summarize(k), "the run never made this read")
		return nil, fmt.Errorf("replay: the recorded run never read %s", k)
	}
	n := w.reads[k]
	if n >= len(got) {
		n = len(got) - 1 // later reads see the last recorded value
	}
	w.reads[k]++
	o := got[n]
	switch {
	case o.Unavailable:
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, o.Error)
	case o.Error != "":
		return nil, errors.New(o.Error)
	}
	return o.Value, nil
}

func (w *replayWorld) Apply(_ string, e ExternalEffect) (json.RawMessage, error) {
	k := effectReplayKey(e.Adapter, e.Op, e.KeyID(), w.r.recorded(e.Args))
	got := w.r.effects[k]
	if len(got) == 0 && w.r.opts.SameKey && e.KeyID() != "" {
		sk := e.Adapter + "." + e.Op + " key=" + e.KeyID()
		if same := w.r.byKey[sk]; len(same) > 0 {
			k, got = sk, same
			if !slices.Contains(w.r.report.Rebound, sk) {
				w.r.report.Rebound = append(w.r.report.Rebound, sk)
			}
		}
	}
	n := w.apps[k]
	if n >= len(got) {
		w.r.missed("effect", summarize(k), "the run never applied this effect")
		return nil, fmt.Errorf("replay: the recorded run never applied %s", k)
	}
	w.apps[k]++
	if res := got[n]; !res.OK {
		return nil, errors.New(res.Error)
	}
	return got[n].Result, nil
}

func (w *replayWorld) Check(string, ExternalEffect) (EffectStatus, error) { return EffectNotDone, nil }

// summarize shortens a read or effect key for the report: each long string
// argument (an inline script) is cut on its own, so the arguments after it,
// which name the read (a script's $0, a path), stay visible; the whole is
// then capped.
func summarize(s string) string {
	head, rest, ok := strings.Cut(s, " ")
	var args any
	if ok && json.Unmarshal([]byte(strings.SplitN(rest, " key=", 2)[0]), &args) == nil {
		var cut func(any) any
		cut = func(v any) any {
			switch x := v.(type) {
			case string:
				if len(x) > 48 {
					return x[:40] + "..."
				}
			case []any:
				for i := range x {
					x[i] = cut(x[i])
				}
			case map[string]any:
				for k := range x {
					x[k] = cut(x[k])
				}
			}
			return v
		}
		b, _ := json.Marshal(cut(args))
		s = head + " " + string(b)
		if _, key, found := strings.Cut(rest, " key="); found {
			s += " key=" + key
		}
	}
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}

// missed records the first thing the run has no record for.
// It aborts the engine call (replayStarved), so the definition takes no
// failure route the run's want of a record would cause.
func (r *replay) missed(kind, now, then string) {
	if r.miss == nil {
		r.miss = &ReplayDifference{Kind: kind, Then: then, Now: now}
	}
	panic(replayStarved{})
}

// replayStarved unwinds an engine call the recorded run cannot feed.
type replayStarved struct{}

// replayObserved, set only by tests, sees the replayed journal when a replay
// ends.
var replayObserved func([]JournalRecord)

func (r *replay) drive() (rep ReplayReport, err error) {
	defer func() {
		if replayObserved != nil && r.engine != nil {
			_ = r.engine.withRecords(r.run, replayObserved)
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(replayStarved); !ok {
				panic(p)
			}
			r.compare() // the engine call stopped short of the missing record
			rep, err = r.report, nil
		}
	}()
	r.now = r.rec[0].At
	if r.now.IsZero() {
		r.now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	r.visits = map[string]int{}
	if chain, died, err := diedAt(r.rec); err == nil && len(chain) > 0 {
		r.report.Died = died
		r.diedInst, r.diedState = chain[len(chain)-1].inst, died
		if i := strings.LastIndex(died, " / "); i >= 0 {
			r.diedState = died[i+len(" / "):]
		}
	}
	w := &replayWorld{r: r, reads: map[string]int{}, apps: map[string]int{}}
	reg := NewAdapters()
	if err := reg.Register(w); err != nil {
		return r.report, err
	}
	r.j = MemJournal()
	nonce := 0
	r.engine = NewEngine(r.j, reg, EngineOptions{
		Defs: []*Def{r.def}, RunID: r.run, Harness: r.started.Harness, Workspace: r.started.Workspace,
		ModelMap: r.started.ModelMap, Clock: func() time.Time { return r.now },
		RunDir: func(RunID) string { return r.newDir },
		Nonce:  func() string { nonce++; return fmt.Sprintf("%016x", nonce) },
	}).(*Engine)
	r.engine.recordedReplay = true
	r.engine.replayStarted = r.started
	r.engine.replaySource = r.fromSource
	r.engine.replayModels = recordedEventModels(r.rec)
	if _, err := r.engine.Run(r.def, r.started.Params, r.started.Options); err != nil {
		return r.report, fmt.Errorf("replay: starting the run: %w", err)
	}
	// The hash the replay runs on: with the run's pinned asset roots it is
	// not the binary's own def.Hash().
	_ = r.engine.withRecords(r.run, func(recs []JournalRecord) {
		if len(recs) > 0 {
			r.report.Now = recs[0].DefHash
		}
	})
	co := Coordinator{Harness: r.started.Harness, Session: "replay"}
	var open *Instruction // a unit that validated and has its next action to take
	for step := 0; step < 4*len(r.rec)+64; step++ {
		if r.compare() {
			return r.report, nil
		}
		var ins []Instruction
		if open != nil {
			ins, open = []Instruction{*open}, nil
		} else {
			var err error
			if ins, err = r.engine.HandOutAs(r.run, 1, co); err != nil {
				return r.report, fmt.Errorf("replay: next: %w", err)
			}
		}
		if r.compare() {
			return r.report, nil
		}
		status, err := r.engine.Status(r.run)
		if err != nil {
			return r.report, err
		}
		if status.Phase == RunEnded {
			return r.finish("ended", fmt.Sprintf("ended at %s (%s)", status.Terminal, status.Outcome)), nil
		}
		if b := status.Breakpoint; b != nil {
			key := replayBreakpoint{b.Instance, b.State, b.Visit}
			at, resumed := r.breakpointResumes[key]
			if !resumed {
				r.report.Stop = ReplayDifference{Kind: "breakpoint", Instance: b.Instance, State: b.State, Now: b.summary()}
				return r.report, nil
			}
			if at.After(r.now) {
				r.now = at
			}
			if err := r.engine.Resume(r.run); err != nil {
				return r.report, fmt.Errorf("replay: resuming %s: %w", b.State, err)
			}
			continue
		}
		var in *Instruction
		for i := range ins {
			if ins[i].Kind == InstructionAgent || ins[i].Kind == InstructionHumanGate {
				in = &ins[i]
				break
			}
		}
		if in == nil {
			if next, ok := nextDue(status, r.now); ok {
				r.now = next
				continue
			}
			return r.finish("halted", fmt.Sprintf("%s with nothing to hand out: %s", status.Phase, haltReason(status))), nil
		}
		key := replayKey(in.Child, lastSegment(in.State))
		if in.Kind == InstructionHumanGate {
			answers := r.answers[key]
			if len(answers) == 0 {
				r.missed("gate", in.State+" asks a human", "the run never answered this gate here")
				r.compare()
				return r.report, nil
			}
			r.answers[key] = answers[1:]
			if err := r.engine.AnswerVisit(r.run, GateAnswer{Gate: in.State, Child: in.Child, Visit: in.Visit, Value: answers[0].Answer, IntValue: answers[0].IntValue}); err != nil {
				return r.report, fmt.Errorf("replay: answering %s: %w", in.State, err)
			}
			continue
		}
		acts := r.actions[key]
		if len(acts) == 0 {
			if why := r.refused[key]; why != "" {
				r.missed("unit", in.State+" is issued again", why)
			}
			r.missed("unit", in.State+" is issued", "the run has no further work of this unit")
			r.compare()
			return r.report, nil
		}
		r.actions[key] = acts[1:]
		if again, err := r.act(*in, acts[0]); err != nil {
			return r.report, err
		} else if again {
			open = in
		}
	}
	return r.report, errors.New("replay: no end within the step bound")
}

func nextDue(status RunStatus, now time.Time) (time.Time, bool) {
	var next time.Time
	for _, d := range status.Due {
		if next.IsZero() || d.Due.Before(next) {
			next = d.Due
		}
	}
	if next.IsZero() || !next.After(now) {
		return time.Time{}, false
	}
	return next.Add(time.Nanosecond), true
}

func haltReason(status RunStatus) string {
	for _, in := range status.Instances {
		if in.Reason != "" {
			return in.State + ": " + in.Reason
		}
	}
	return "no reason recorded"
}

// act delivers one recorded action to the unit; true after a validate, when
// the unit's next recorded action follows on the same attempt.
func (r *replay) act(in Instruction, a replayAction) (bool, error) {
	if err := r.engine.Started(r.run, in.Name, Claim{Harness: r.started.Harness, Session: "replay", Agent: in.Name, Context: ContextFresh}); err != nil && !errors.Is(err, ErrRejected) {
		return false, fmt.Errorf("replay: started %s: %w", in.Name, err)
	}
	fetched, err := r.engine.PromptFetch(r.run, in.Name, "")
	if err != nil {
		return false, fmt.Errorf("replay: prompt %s: %w", in.Name, err)
	}
	switch a.kind {
	case "submit", "validate":
		then, now := a.promptHash, fetched.PromptHash
		if a.promptShapeHash != "" {
			then, now = a.promptShapeHash, fetched.PromptShapeHash
		}
		if then != "" && then != now {
			r.report.Stale = append(r.report.Stale, ReplayDifference{Kind: "prompt", Instance: in.Child, State: lastSegment(in.State), Then: then, Now: now})
		}
		if err := r.writeArtifacts(fetched.Prompt, a.artifacts); err != nil {
			return false, err
		}
		if a.kind == "validate" {
			res, err := r.engine.Validate(r.run, in.Name, Submission{Output: a.output})
			if err != nil {
				return false, fmt.Errorf("replay: validate %s: %w", in.Name, err)
			}
			if res.Class == Refused {
				return true, nil // refused as then: the recorded correction follows
			}
			// The definition no longer refuses what the run was refused: the
			// child would have run done on it. The correction the run sent on
			// that attempt has no place here, and the route says where it went.
			r.report.Differences = append(r.report.Differences, ReplayDifference{Kind: "refusal", Instance: in.Child, State: lastSegment(in.State),
				Then: "refused: " + a.reason, Now: res.Class.String()})
			key := replayKey(in.Child, lastSegment(in.State))
			if acts := r.actions[key]; len(acts) > 0 && acts[0].kind == "submit" {
				r.actions[key] = acts[1:]
			}
		}
		var res SubmitResult
		res, err = r.engine.Done(r.run, in.Name, Submission{Output: a.output})
		if err == nil && res.Class == Refused && len(res.Refusal) > 0 {
			// The definition refuses what the run had accepted: if the run
			// has nothing more for this unit, that refusal is why it stops.
			check, msg, _ := strings.Cut(res.Refusal[0], ": ")
			first, _, _ := strings.Cut(msg, "\n")
			r.refused[replayKey(in.Child, lastSegment(in.State))] = fmt.Sprintf("recorded result refused by %s: %s", check, first)
		}
	case "missing":
		// A done with no output file spent the attempt: the first leaves it
		// open, the second spends it. The refusal names this replay's own
		// allocated output, as the run's named its own; a journal with no
		// allocated output keeps the recorded path.
		path, perr := r.engine.allocatedOutput(r.run, in.Name)
		if perr != nil || path == "" {
			path = strings.TrimPrefix(a.reason, "no output file at ")
		}
		for range 2 {
			if _, err = r.engine.refuseMissingOutput(r.run, in.Name, path, nil); !errors.Is(err, errOutputMissing) {
				break
			}
			err = nil
		}
	case "fail":
		_, err = r.engine.FailedAs(r.run, in.Name, a.reason, a.class)
	case "lost":
		r.report.Notes = append(r.report.Notes, fmt.Sprintf("%s: a lost attempt (%s) replayed as a transient failure", in.State, a.reason))
		_, err = r.engine.FailedAs(r.run, in.Name, "replayed lost attempt: "+a.reason, FailTransient)
	}
	if err != nil {
		return false, fmt.Errorf("replay: %s %s: %w", a.kind, in.Name, err)
	}
	return false, nil
}

// writeArtifacts copies each recorded artifact's file to the path this
// replay's engine allocated for it (the prompt's final Artifacts section).
func (r *replay) writeArtifacts(prompt string, refs map[string]ArtifactRef) error {
	if len(refs) == 0 {
		return nil
	}
	marker := "\n## Artifacts\n"
	i := strings.LastIndex(prompt, marker)
	if i < 0 {
		return nil
	}
	block, _, _ := strings.Cut(prompt[i+len(marker):], "\n## ")
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := ""
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "- "+name+": "); ok {
				path = strings.TrimSuffix(p, " (optional: leave it unwritten when there is nothing to record)")
			}
		}
		if path == "" {
			continue
		}
		body, err := os.ReadFile(refs[name].Path)
		if err != nil && r.sourceDir != "" && r.oldDir != "" {
			body, err = os.ReadFile(strings.Replace(refs[name].Path, r.oldDir, r.sourceDir, 1))
		}
		if err != nil {
			r.report.Notes = append(r.report.Notes, fmt.Sprintf("artifact %s: the recorded file %s is gone; replayed empty", name, refs[name].Path))
			body = nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fromSource copies path, a file under the replay's run directory that an
// effect of the run wrote (a replay applies none), from the source run's
// directory, and notes it: the copy is the file's last content, not
// necessarily what the run read then.
func (r *replay) fromSource(path string) bool {
	rel, err := filepath.Rel(r.newDir, path)
	if r.sourceDir == "" || err != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return false
	}
	body, err := os.ReadFile(filepath.Join(r.sourceDir, rel))
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o755) != nil || os.WriteFile(path, body, 0o644) != nil {
		return false
	}
	r.report.Notes = append(r.report.Notes, fmt.Sprintf("prompt file %s: no effect writes it in a replay; read from the source run (its last content)", rel))
	return true
}

// compare checks the definition's new transitions against the run's at the
// same instance, state and visit, and whether it asked for something the run
// has no record of. true when the replay has to stop.
func (r *replay) compare() bool {
	var recs []JournalRecord
	_ = r.engine.withRecords(r.run, func(got []JournalRecord) { recs = got })
	for ; r.seen < len(recs); r.seen++ {
		rec := recs[r.seen]
		if rec.Kind != JournalTransition {
			continue
		}
		var d TransitionData
		if json.Unmarshal(rec.Data, &d) != nil {
			continue
		}
		k := replayKey(rec.Instance, rec.State)
		r.visits[k]++
		now := replayStep{d.To, planLabel(recs[:r.seen], rec.Instance, rec.State, d.To)}
		then, ok := r.steps[fmt.Sprintf("%s|%d", k, r.visits[k])]
		// A recorded label of "" (no plan named it) matches any.
		if ok && then.to == now.to && (then.label == "" || then.label == now.label) {
			r.report.Retraced++
			continue
		}
		thenText := "the run never left this state here"
		if ok {
			thenText = "-> " + then.to + label(then.label)
		}
		r.report.Differences = append(r.report.Differences, ReplayDifference{Kind: "route", Instance: rec.Instance, State: rec.State,
			Then: thenText, Now: "-> " + now.to + label(now.label)})
		// Only another target passes the point of death: a renamed edge to
		// the same state is a difference, not a pass.
		if r.report.Died != "" && r.report.Passed == "" && rec.Instance == r.diedInst && rec.State == r.diedState && (!ok || then.to != now.to) {
			r.report.Passed = rec.State + " -> " + now.to + label(now.label)
		}
	}
	if r.miss != nil {
		// The state that made the call: the newest record names it.
		for i := len(recs) - 1; i >= 0 && r.miss.State == ""; i-- {
			if recs[i].State == "" {
				continue
			}
			r.miss.Instance, r.miss.State = recs[i].Instance, recs[i].State
			var into TransitionData
			if recs[i].Kind == JournalTransition && json.Unmarshal(recs[i].Data, &into) == nil {
				r.miss.State = into.To // the read was the arriving state's
			}
		}
		r.report.Stop = *r.miss
		return true
	}
	return false
}

func label(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf(" (%q)", s)
}

// finish records why a replay that was not starved stopped.
func (r *replay) finish(kind, now string) ReplayReport {
	r.report.Stop = ReplayDifference{Kind: kind, Now: now}
	return r.report
}
