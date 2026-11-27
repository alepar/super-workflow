package wf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors the Engine returns; callers tell them apart with errors.Is.
var (
	// ErrDefChanged: the run was started with a different definition hash and no
	// declared Migration covers it.
	ErrDefChanged = errors.New("definition hash differs from the run's")
	// ErrUnknownDef: the run's definition name is not known to this Engine.
	ErrUnknownDef = errors.New("definition not known to the engine")
	// ErrRejected: an Answer or Settle the run's state does not take (a value
	// outside the gate's Enum, no such open gate or effect). Nothing is journaled.
	ErrRejected = errors.New("rejected")
	// ErrNotSupported: the operation belongs to a later part of the engine.
	ErrNotSupported = errors.New("not yet supported")
)

// EngineOptions configures NewEngine.
type EngineOptions struct {
	// PagedPrompts opts a new run into engine-delivered prompt pages.
	PagedPrompts bool
	// HaltBefore selects the named states that will pause a newly created run on arrival.
	HaltBefore []string
	// Snapshots records adapter-owned world checkpoints before transitions and offers.
	Snapshots bool
	// StateRoot enables relocatable workspace/state-relative journal payloads.
	StateRoot string

	// caller is the CLI process identity, never a records-root override.
	caller        harness.Caller
	childClaim    string
	operatorChild bool
	// oneVerb: the engine serves one CLI verb and exits, so a child-verb
	// check that passed is not run again in it (ChildVerbCheck).
	oneVerb bool

	// Adopt lets a resume carry a run onto a changed definition with no
	// declared Migration when every live state still exists under the same
	// name with the same kind and result type (E1, wf resume --adopt): the
	// adoption is journaled as migrated.
	Adopt bool
	// InstalledBinary is MainConfig.InstalledBinary: a snapshot fork installs
	// the running binary at this workspace-relative path when its restored
	// world lacks it.
	InstalledBinary string
	// SnapshotFiles is MainConfig.SnapshotFiles: workspace-relative files a
	// world snapshot carries even when git ignores them (SnapshotRequest.Include).
	SnapshotFiles []string
	// Assets explicitly selects a definition asset root for this invocation.
	Assets string
	// OnAdopt reports a successfully journaled adoption.
	OnAdopt func(RunID, MigratedData)

	// Harness on creation is the harness the CLI already resolved (the value its
	// PreflightContext carried); on resume only an explicit --harness is set,
	// and empty inherits the journaled one. Inferred here only when empty on
	// creation (library callers).
	Harness string
	// HarnessFromEnv: the CLI read Harness from the starter's environment
	// (RunStartedData.HarnessFromEnv).
	HarnessFromEnv bool
	// HarnessHints are the starter's environment's harness hints
	// (harness.WeakHints), journaled at the run's start.
	HarnessHints []string

	Defs      []*Def            // name to Def lookup for a fresh process
	Workspace string            // journaled in run_started
	ModelMap  map[string]string // journaled in run_started; the CLI's PreflightContext map
	// Headless configures the run's cross-harness children (journaled in
	// run_started).
	Headless *HeadlessConfig
	// HeadlessCheck is the preflight of a child harness for a coordinator
	// harness, run once per segment at its binding (harness_checked); nil
	// checks nothing. The CLI's checks its PATH, login and launch route.
	// spawn is the spawn verb's command exactly as next prints it, its claim
	// the slot <claim>.
	HeadlessCheck func(child, coordinator string, cfg *HeadlessConfig, spawn string) HarnessCheckedData
	// HeadlessProcess starts and checks headless children; nil is the host's.
	HeadlessProcess HeadlessProcess
	// BoundaryModelMap is the --model-map a resume, rewind or fork journals
	// on the segment boundary it opens; nil carries the map on.
	BoundaryModelMap map[string]string
	RunID            RunID                                // --run-id: resumes when its journal exists
	Clock            func() time.Time                     // default time.Now
	Nonce            func() string                        // default crypto/rand hex
	SubmitLine       func(run RunID, token string) string // default "workflow submit <run> --token <t> --output <file>"
	SettleLine       func(run RunID, effect int64) string // default "workflow settle <run> --effect <id> --applied|--not-applied"
	AnswerLine       func(run RunID, gate string) string  // default "workflow answer <run> --gate <state> --value <value>"
	RunBudget        RunBudget                            // journaled on run_started (run --budget over Registration.Budget); zero means no budget
	// ResumeBudget is resume --budget: the limits it names change the run
	// budget in force from this resume on; nil leaves it as it is.
	ResumeBudget *BudgetChange
	VerbLine     func(run RunID, verb string, args ...string) string // default "workflow <verb> <run> <args...>"
	// ResumeLine is the command that starts a successor of the ended run
	// started as started (run on an ended run starts one); "" leaves it unsaid.
	ResumeLine func(started RunStartedData) string
	// GivenParams are the param names the caller gave (run --param), as
	// against those the registration derived; run_started journals them, so
	// a resume line repeats only what was given.
	GivenParams   []string
	HandOutWindow time.Duration // default 2m: a handed unit not started by then is lost
	SpawnWindow   time.Duration // default 3m: a unit claimed by started and never fetched is lost by then (except wave aliases)
	// UnstartedGrace (default 5m) is how long a unit handed to a coordinator
	// session may stay unfetched before that coordinator's next offers it
	// again as never started, when no later offer's fetch showed it
	// was passed over sooner.
	UnstartedGrace time.Duration
	FetchedLease   time.Duration // default 30m: the implicit lease of a fetched unit with no Map lease and no watched transcript, from its last sign of life (claim, fetch, later started, heartbeat, transcript write)
	LostCap        int           // default 5: the fifth loss in one unit state settles the attempt failed
	EffectPoll     time.Duration // default 2s: how often a running detached effect's result file is polled
	EffectRunner   EffectRunner  // nil: .Detached() commands run inline
	// Progress receives a detached command's progress lines while it runs
	// inline because the host's detach mode is none; nil writes none.
	Progress      io.Writer
	ProgressEvery time.Duration          // default 30s: how often an inline detached command reports it is still running
	RunDir        func(run RunID) string // the run's directory for detached result files; nil or "" disables detaching
	StallDefault  time.Duration          // default 20m: the stall threshold of a class with no history
	// TranscriptMtime reports when the transcript at path was last written;
	// default os.Stat's ModTime. fs.ErrNotExist means no write yet; any other
	// error means the path cannot be statted, and the unit is never stalled by
	// mtime.
	TranscriptMtime func(path string) (time.Time, error)
	// ChildLive reports whether a live process carries the child's marker
	// (harness.SettlingEnd.ChildLive). nil asks the child's harness adapter;
	// tests inject it.
	ChildLive func(harness, child string) (bool, error)
	// Delegations maps a qualified gate to the answer the starter hands the
	// coordinator; journaled in run_started.
	// manufactured, when set, marks every run this engine starts as
	// manufactured (RunStartedData.Manufactured). Only wf dev manufacture
	// sets it; it is a label in a writable journal, not authenticated
	// provenance.
	manufactured *ManufacturedData
	Delegations  map[string]string
}

// Engine is the event-sourced Executor.
//
// Owns: run identity (the definition name plus the run key, a supplied id or a
// nonce), the locked fold-drive-append cycle, token validation, the failed
// attempt class table, edge effects and counters, and the journaled decisions
// every closure result becomes.
// Does not: keep state between calls (every call folds the journal), call a
// CLI itself (adapters do, through Adapters), run a closure outside a cycle, or
// switch on a kind: per-kind behaviour is stepKind.execute.
// Extend by: a new kind is a stepKind.execute implementation in its own
// engine_<kind>.go; a new tool is an Adapters registration; callers never
// append journal records.
type Engine struct {
	// recordedReplay is private to ReplayWith's isolated in-memory engine.
	// Recorded outputs are historical evidence, not fresh live submissions.
	recordedReplay bool
	// replayStarted preserves recorded parameter ownership when replaying a
	// definition whose declarations may have changed.
	replayStarted *RunStartedData
	// replaySource, set by ReplayWith, copies a file missing under the
	// replay's run directory from the source run's, reporting whether it did.
	replaySource func(path string) bool
	// replayModels, set by a replay, answers unitModels from the source
	// journal (recordedEventModels): the models of the recorded issue,
	// fetch or hand-out a replayed one stands for.
	replayModels *recordedModels

	j    Journal
	reg  *Adapters
	opts EngineOptions

	mu          sync.Mutex
	defs        map[string]*Def
	depthCache  map[*Def]*defDepths       // handOutOrder's critical depths, computed once per Def
	searchedAt  map[string]time.Time      // resolveChildren: when each transcript root and session was last searched
	stores      map[string]*snapshotStore // checkStoreSnapshot: each store's validated checkpoints
	verbChecked map[string]bool           // ChildVerbCheck: the checks that passed in this process
	endChecked  map[string]time.Time      // loseEndedChildren: each transcript's mtime when last read for its end marker
	endPending  map[string]endPend        // loseEndedChildren: the transcripts that read EndIfSettled, by path

	// afterAppend, when set by a test, sees every record the cycle appends.
	afterAppend func(c *cycle, rec JournalRecord)
}

// endPend is a transcript that read EndIfSettled: the mtime it was read at and
// when its settle is due.
type endPend struct{ mt, due time.Time }

// unstartedGrace is UnstartedGrace's default; a test of the CLI shortens it.
var unstartedGrace = 5 * time.Minute

// NewEngine builds the Executor over a journal and an adapter registry (nil
// means no adapters: every read is unavailable).
func NewEngine(j Journal, reg *Adapters, opts EngineOptions) Executor {
	if opts.Assets != "" {
		opts.Assets = effectiveAssets(nil, opts.Assets)
	}
	if reg == nil {
		reg = NewAdapters()
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.HandOutWindow == 0 {
		opts.HandOutWindow = 2 * time.Minute
	}
	if opts.SpawnWindow == 0 {
		opts.SpawnWindow = 3 * time.Minute
	}
	if opts.FetchedLease == 0 {
		opts.FetchedLease = 30 * time.Minute
	}
	if opts.UnstartedGrace <= 0 {
		opts.UnstartedGrace = unstartedGrace
	}
	if opts.ProgressEvery == 0 {
		opts.ProgressEvery = 30 * time.Second
	}
	if opts.EffectPoll == 0 {
		opts.EffectPoll = 2 * time.Second
	}
	if opts.LostCap == 0 {
		opts.LostCap = 5
	}
	if opts.StallDefault == 0 {
		opts.StallDefault = 20 * time.Minute
	}
	if opts.TranscriptMtime == nil {
		opts.TranscriptMtime = func(path string) (time.Time, error) {
			fi, err := os.Stat(path)
			if err != nil {
				return time.Time{}, err
			}
			return fi.ModTime(), nil
		}
	}
	if opts.Nonce == nil {
		opts.Nonce = func() string {
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				panic("wf: reading random bytes: " + err.Error())
			}
			return hex.EncodeToString(b)
		}
	}
	if opts.VerbLine == nil {
		opts.VerbLine = func(run RunID, verb string, args ...string) string {
			return strings.Join(append([]string{"workflow", verb, string(run)}, args...), " ")
		}
	}
	if opts.SubmitLine == nil {
		opts.SubmitLine = func(run RunID, token string) string {
			return fmt.Sprintf("workflow submit %s --token %s --output <file>", run, token)
		}
	}
	if opts.SettleLine == nil {
		opts.SettleLine = func(run RunID, effect int64) string {
			return fmt.Sprintf("workflow settle %s --effect %d --applied|--not-applied", run, effect)
		}
	}
	if opts.AnswerLine == nil {
		opts.AnswerLine = func(run RunID, gate string) string {
			return fmt.Sprintf("workflow answer %s --gate %s --value <value>", run, gate)
		}
	}

	if opts.Workspace != "" {
		if workspace, err := realPath(opts.Workspace); err == nil {
			opts.Workspace = workspace
		}
	}
	if opts.StateRoot != "" {
		if state, err := realPath(opts.StateRoot); err == nil {
			opts.StateRoot = state
		}
		j = &rootedJournal{Journal: j, workspace: opts.Workspace, state: opts.StateRoot, active: map[RunID]PathRoots{}}
	}
	e := &Engine{j: j, opts: opts, defs: map[string]*Def{}}
	e.reg = reg.withRuntime(engineRuntime{e})
	for _, d := range opts.Defs {
		e.defs[d.name] = d
		d.freeze()
	}
	return e
}

// cycle is one locked read-fold-append pass over a run.
type cycle struct {
	coordinatorNext bool
	turnWaitingMaps map[string]int64
	// forkRoots caches forkSources: the source roots of the forks in this
	// run's history.
	forkRoots *[]ForkSource
	// issuing is the instance whose issue is rendering now: its
	// {{resolvedModels}} and dispatch are the issue's (renderModels).
	issuing *instance
	// issueReads is the input observations the issue in progress read, by
	// canonical descriptor: one read per descriptor, shared by every prompt
	// input over it and by its staleness token. issueToken is its token.
	issueReads map[string]ObservedData
	issueToken string
	// briefBare drops the brief's "This unit runs" line for this call's
	// offers: even the first offer with it would cross the coordinator's
	// output limit (unitsThatFit).
	briefBare          bool
	promptPresentation func(string, string) (string, error)
	e                  *Engine
	run                RunID
	recs               []JournalRecord
	st                 *runState
	// dry folds records into st without appending them (validate): the
	// journal and the after-append hook never see them.
	dry bool
	// lockFree is a read-only cycle over committed records, read with no
	// lock (withReadOnly): any hold on the run lock is another call's.
	lockFree bool

	polled map[string]bool // instances whose Wait already read this cycle

	// transient maps an instance to the error its effect Check returned this
	// cycle: the effect stays open and is checked again on the next drive.
	transient map[string]string

	// detached maps an instance to what its open detached effect told this
	// cycle, polled once per cycle: Next and HandOut read it.
	detached map[string]detachedNote

	// requery is set by HandOut: a Map with Requery re-reads its source once
	// this cycle, and requeried records the sources already re-read (keyed by
	// instance, state and arrival), so one hand-out reads each source once.
	requery   bool
	requeried map[string]bool

	// handCall is the Call of this cycle's hand-outs: the Seq of its first.
	handCall int64
	// handAt is the one time a hand-out pass judges grace eligibility at
	// (unstartedCause); next's wait schedules every deadline after it.
	handAt time.Time

	// unstatable names the units whose transcript could not be statted this
	// cycle (an error other than not-exist): they have no stall deadline.
	unstatable map[string]bool

	// captured is this cycle's last world capture while nothing has changed
	// the world since it (worldUnchanged): a later checkpoint of the cycle
	// names it again instead of copying the world anew. nil otherwise.
	// docs are the documents this cycle's bindings decoded (docValue),
	// guarded by docsMu.
	docs   map[docKey]docEntry
	docsMu sync.Mutex

	captured      *WorldSnapshotData
	capturedStamp string // runDataStamp when captured was taken

	statsAt int         // len(recs) when stats was folded
	stats   *classStats // the estimates, folded lazily

	// caller is the coordinator whose next runs this cycle (HandOutAs); zero
	// for every other verb.
	caller Coordinator

	// searchNow makes resolveChildren search every transcript root this cycle,
	// unthrottled: a unit is about to settle or was just claimed.
	searchNow bool

	// offer is how a next with n omitted chose its count (E3); nil when n
	// was given.
	offer *OfferView
	// more is how many ready units a hand-out left for the next call because
	// its output would not fit the caller's harness (unitsThatFit).
	more int
}

func (c *cycle) markTransient(in *instance, err error) {
	if c.transient == nil {
		c.transient = map[string]string{}
	}
	c.transient[in.ID] = err.Error()
}

func (c *cycle) workspace() string { return c.st.started.Workspace }

// withRun takes the run's lock, folds its journal and runs f.
func (e *Engine) withRun(run RunID, f func(c *cycle) error) error {
	return e.withRunMode(run, false, nil, f)
}

// withDryRun is withRun on a dry cycle from the start: the migration, the
// transcript resolution and every record the verb would write fold into
// memory and none is appended (validate).
func (e *Engine) withDryRun(run RunID, f func(c *cycle) error) error {
	return e.withRunMode(run, true, nil, f)
}

// withRunChecked is withRun with check run on the raw records under the lock,
// before the fold and before any migration or adoption is journaled: a
// refusal there leaves the journal as it was.
func (e *Engine) withRunChecked(run RunID, check func([]JournalRecord) error, f func(c *cycle) error) error {
	return e.withRunMode(run, false, check, f)
}

func (e *Engine) withRunMode(run RunID, dry bool, check func([]JournalRecord) error, f func(c *cycle) error) error {
	unlock, err := e.j.Lock(run)
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := e.j.Records(run)
	if err != nil {
		return err
	}
	if check != nil {
		if err := check(recs); err != nil {
			return err
		}
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return err
	}
	c := &cycle{e: e, run: run, recs: recs, st: st, dry: dry}
	// An ended run is read, never migrated: nothing is written to a terminal
	// journal, and a rebuilt binary can still show its cause and resume line.
	if !st.ended {
		if err := c.migrate(def); err != nil {
			return err
		}
		if err := c.loseForkOrphans(); err != nil {
			return err
		}
	}
	if !dry && len(recs) > 0 && recs[0].Roots != nil {
		from, to := recs[0].Roots.Workspace, e.opts.Workspace
		if from != "" && to != "" && !sameRealPath(from, to) {
			if err := e.reg.relocateWorkspace(from, to); err != nil {
				return err
			}
		}
	}
	return f(c)
}

// withReadOnly runs f on a dry cycle over the run's committed records read
// with no lock (committedReader), for a verb that only reads the run
// (status, explain): it never waits on the lock's holder, which may be the
// process that runs it (an effect calling wf status on its own run, an earlier revision).
// Like withDryRun, it migrates and loses fork orphans in memory only. A
// journal that cannot read without its lock is read under it.
func (e *Engine) withReadOnly(run RunID, f func(c *cycle) error) error {
	return e.readOnly(run, false, f)
}

// errNotLockFree is a strict read-only cycle over a journal (through every
// wrapper) that cannot read the run without its lock.
var errNotLockFree = fmt.Errorf("%w: the journal has no lock-free read", ErrUnavailable)

// readOnly is withReadOnly; strict refuses the fallback to the lock
// (errNotLockFree) for a caller that already holds it (StatusSnapshot).
func (e *Engine) readOnly(run RunID, strict bool, f func(c *cycle) error) error {
	cr, ok := e.j.(committedReader)
	if !ok {
		if strict {
			return errNotLockFree
		}
		return e.withDryRun(run, f)
	}
	recs, err := cr.ReadCommitted(run)
	if errors.Is(err, errNoLockFreeRead) {
		if strict {
			return errNotLockFree
		}
		return e.withDryRun(run, f)
	}
	if err != nil {
		return err
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return err
	}
	c := &cycle{e: e, run: run, recs: recs, st: st, dry: true, lockFree: true}
	if !st.ended {
		if err := c.migrate(def); err != nil {
			return err
		}
		if err := c.loseForkOrphans(); err != nil {
			return err
		}
	}
	return f(c)
}

// withTelemetry runs f on a cycle over the run's folded records under its
// lock, with nothing else journaled: no migration, adoption or workspace
// relocation. A refused verb's audit record is then its only write.
func (e *Engine) withTelemetry(run RunID, f func(c *cycle) error) error {
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
		return ErrUnknownRun
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return err
	}
	return f(&cycle{e: e, run: run, recs: recs, st: st})
}

func (e *Engine) defFor(first JournalRecord) (*Def, error) {
	if first.Kind != JournalRunStarted {
		return nil, fmt.Errorf("%w: first record is %s, not run_started", ErrCorruptJournal, first.Kind)
	}
	var d RunStartedData
	if err := json.Unmarshal(first.Data, &d); err != nil {
		return nil, fmt.Errorf("%w: run_started: %v", ErrCorruptJournal, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	def, ok := e.defs[d.Def]
	if !ok {
		return nil, fmt.Errorf("%w: %q (pass it in EngineOptions.Defs)", ErrUnknownDef, d.Def)
	}
	return def, nil
}

// emit appends one record for in and folds it.
func (c *cycle) emit(in *instance, kind JournalKind, class OutcomeClass, data any) error {
	return c.emitAs(in.ID, in.Cur, in.Attempt, kind, class, data)
}

func (c *cycle) emitAs(instance, state string, attempt int, kind JournalKind, class OutcomeClass, data any) error {
	if !c.dry && c.st.started != nil && c.st.started.Snapshots && (kind == JournalTransition || kind == JournalInstructionIssued || kind == JournalGateOffered) {
		if err := c.checkpoint(instance, state, attempt, kind); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshalling %s payload: %w", kind, err)
	}
	rec := JournalRecord{
		Kind: kind, Run: c.run, Instance: instance, State: state, Attempt: attempt,
		Class: class, DefHash: c.st.hash, At: c.e.opts.Clock(), Data: raw,
	}
	if !c.dry {
		if err := c.e.j.Append(rec); err != nil {
			return err
		}
	}
	rec.Seq = int64(len(c.recs) + 1)
	c.recs = append(c.recs, rec)
	if !worldUnchanged[kind] {
		c.captured = nil
	}
	if err := c.st.apply(rec); err != nil {
		return err
	}
	if h := c.e.afterAppend; h != nil && !c.dry {
		h(c, rec)
	}
	return nil
}

// nextSeq is the Seq the next appended record will get.
func (c *cycle) nextSeq() int64 { return int64(len(c.recs) + 1) }

// ---------------------------------------------------------------- Run

// Run implements Executor.
func (e *Engine) Run(d *Def, params map[string]string, options any) (RunID, error) {
	if xs := childNameClashes(d); len(xs) > 0 {
		return "", fmt.Errorf("definition %s (%s): %s; start, re-attach or replay refused (a replay would only repeat the shadowing); existing runs resume through next/resume", d.name, RuleChildNameDuplicate, xs[0])
	}
	e.mu.Lock()
	e.defs[d.name] = d
	e.mu.Unlock()
	d.freeze()
	params = d.normalizeAliases(params)
	if _, ok := params[scratchName]; ok {
		return "", errors.New("scratch is an engine binding (the step's scratch directory), not a run parameter")
	}
	id, err := e.runID(d, params)
	if err != nil {
		return "", err
	}
	var opts json.RawMessage
	if options != nil {
		if opts, err = json.Marshal(options); err != nil {
			return "", fmt.Errorf("marshalling options: %w", err)
		}
	}
	// An ended run is never reopened. A run whose id the run key derived and
	// that has ended is followed by a successor, <id>.2, <id>.3, ...: the first
	// that is live is resumed (attached), the first that does not exist is
	// started with its predecessor's records. An explicit run id is taken as
	// given.
	base, pred := id, (*RunStartedData)(nil)
	for n := 2; ; n++ {
		got, ended, err := e.runAt(d, params, opts, id, pred)
		if err != nil || ended == nil || e.opts.RunID != "" {
			return got, err
		}
		ended.self = id
		pred, id = ended, RunID(fmt.Sprintf("%s.%d", base, n))
	}
}

// successorWorktrees is the Worktrees a successor of pred records: nil for a
// run that succeeds none, else the set (possibly empty).
func successorWorktrees(pred *RunStartedData) *[]string {
	if pred == nil {
		return nil
	}
	w := pred.inherited
	if w == nil {
		w = []string{}
	}
	return &w
}

// endedStart is the run_started payload of an ended run's records, read
// without folding: an ended run is succeeded, never resumed, so a changed
// definition (a rebuilt binary) never stands in the way and nothing is
// migrated into a terminal journal. nil when the run has not ended.
func endedStart(recs []JournalRecord) (*RunStartedData, error) {
	ended := slices.ContainsFunc(recs, func(r JournalRecord) bool { return r.Kind == JournalRunEnded })
	if !ended || len(recs) == 0 {
		return nil, nil
	}
	d, err := decodeData(recs[0])
	if err != nil {
		return nil, err
	}
	started, ok := d.(*RunStartedData)
	if !ok {
		return nil, fmt.Errorf("run %s: its first record is not run_started", recs[0].Run)
	}
	return started, nil
}

// runAt resumes run id when its journal exists, returning its run_started when
// it has ended, or starts it, as the successor of pred when pred is set.
func (e *Engine) runAt(d *Def, params map[string]string, opts json.RawMessage, id RunID, pred *RunStartedData) (RunID, *RunStartedData, error) {
	unlock, err := e.j.Lock(id)
	if err != nil {
		return "", nil, err
	}
	defer unlock()
	recs, err := e.j.Records(id)
	switch {
	case err == nil:
		if len(recs) > 0 && recs[0].Roots != nil && e.opts.Workspace != "" {
			if original := recs[0].Roots.Workspace; original != "" && !sameRealPath(original, e.opts.Workspace) {
				return "", nil, fmt.Errorf("%w: run %s uses workspace %s; continue with next or resume to relocate", ErrRejected, id, original)
			}
		}
		if started, err := endedStart(recs); err != nil || started != nil {
			if started != nil && started.Def != d.name {
				return "", nil, fmt.Errorf("%w: run %s belongs to definition %s, not %s", ErrDefChanged, id, started.Def, d.name)
			}
			if started != nil && e.opts.RunID == "" {
				// What a successor of this run carries on in, read now from
				// its final records: the successor records it (Worktrees).
				if started.inherited, err = e.inheritedWorktrees(id, recs); err != nil {
					return "", nil, fmt.Errorf("run %s's successor: %w", id, err)
				}
			}
			if started != nil && e.opts.RunID != "" {
				if err := e.matchRunBreakpoints(d, started, id); err != nil {
					return "", nil, err
				}
				// An explicit id names this run itself: what the caller
				// passed must still agree with how it was started.
				if err := resumeMatches(id, *started, opts, e.opts.Delegations); err != nil {
					return "", nil, err
				}
			}
			return id, started, err
		}
		def, err := e.defFor(recs[0])
		if err != nil {
			return "", nil, err
		}
		st, err := foldRun(recs, def)
		if err != nil {
			return "", nil, err
		}
		if def.name != d.name {
			return "", nil, fmt.Errorf("%w: run %s belongs to definition %s, not %s", ErrDefChanged, id, def.name, d.name)
		}
		if err := e.matchRunBreakpoints(d, st.started, id); err != nil {
			return "", nil, err
		}
		if err := resumeHarnessMatches(id, *st.started, e.opts); err != nil {
			return "", nil, err
		}
		if err := resumeMatches(id, *st.started, opts, e.opts.Delegations); err != nil {
			return "", nil, err
		}
		c := &cycle{e: e, run: id, recs: recs, st: st}
		if err := c.migrate(d); err != nil {
			return "", nil, err
		}
		return id, nil, nil
	case errors.Is(err, ErrUnknownRun):
	default:
		return "", nil, err
	}
	// A successor carries on how its predecessor was started, unless the
	// caller says otherwise: its options, delegations, model map and
	// workspace. The harness is the current coordinator's.
	modelMap, delegations, workspace := e.opts.ModelMap, e.opts.Delegations, e.opts.Workspace
	snapshots := e.opts.Snapshots
	pagedPrompts := e.opts.PagedPrompts
	var haltBefore []string
	if e.recordedReplay && e.replayStarted != nil {
		haltBefore = e.replayStarted.HaltBefore
	} else {
		haltBefore, err = resolveBreakpoints(d, e.opts.HaltBefore)
		if err != nil {
			return "", nil, err
		}
	}
	if pred != nil {
		snapshots = snapshots || pred.Snapshots
		pagedPrompts = pagedPrompts || pred.PagedPrompts
		if len(opts) == 0 {
			opts = pred.Options
		}
		if len(delegations) == 0 {
			delegations = pred.Delegations
		}
		if len(modelMap) == 0 {
			modelMap = pred.ModelMap
		}
		if pred.Workspace != "" {
			workspace = pred.Workspace
		}
	}
	harness := resolveHarness(e.opts.Harness)
	resolved, unresolved, err := resolveTiers(harness, collectTiers(d), modelMap)
	if err != nil {
		return "", nil, err
	}
	models, efforts := map[string]string{}, map[string]string{}
	for t, tm := range resolved {
		models[t] = tm.Model
		efforts[t] = tm.Effort
		if tm.Effort == "" {
			efforts[t] = EffortHarnessDefault
		}
	}
	headlessCfg := e.opts.Headless
	childModels, err := childTables(d, harness, modelMap, headlessCfg)
	if err != nil {
		return "", nil, err
	}
	if e.recordedReplay && e.replayStarted != nil {
		// Replay never consults the live tier table for what the run
		// recorded: its start models are the recorded ones.
		models, efforts = maps.Clone(e.replayStarted.Models), maps.Clone(e.replayStarted.Efforts)
		childModels, headlessCfg = e.replayStarted.ChildModels, e.replayStarted.Headless
		unresolved = nil
	}
	// Model tiers are hints (A3c): a harness with no model for a tier runs
	// those children on the session's own model, and the run says so.
	var tiersNote *DegradedData
	if !(e.recordedReplay && e.replayStarted != nil) {
		tiersNote = unresolvedTiersNote(harness, unresolved, len(modelMap) > 0)
	}
	runDir := ""
	if e.opts.RunDir != nil {
		runDir = e.opts.RunDir(id)
	}
	previousRunDir := ""
	if pred != nil && e.opts.RunDir != nil {
		previousRunDir = e.opts.RunDir(pred.self)
	}
	var normalizedParams map[string]string
	var paramPaths map[string]PathBase
	if e.recordedReplay && e.replayStarted != nil {
		// Absence of ownership is historical evidence too. Unlike a successor
		// or snapshot opt-in, replay must not type old plain parameters.
		normalizedParams = maps.Clone(e.replayStarted.Params)
		paramPaths = maps.Clone(e.replayStarted.ParamPaths)
		for name, base := range paramPaths {
			if err := validatePathParam(name, normalizedParams[name], base); err != nil {
				return "", nil, err
			}
		}
	} else {
		normalizedParams, paramPaths, err = normalizeSuccessorPathParams(d, params, workspace, runDir, e.opts.StateRoot, previousRunDir, pred)
		if err != nil {
			return "", nil, err
		}
	}
	var predID RunID
	if pred != nil {
		predID = pred.self
		if e.opts.RunDir != nil {
			if err := copyTree(filepath.Join(e.opts.RunDir(predID), "records"), filepath.Join(e.opts.RunDir(id), "records")); err != nil {
				return "", nil, fmt.Errorf("carrying run %s's records to its successor %s: %w", predID, id, err)
			}
		}
	}
	c := &cycle{e: e, run: id, st: &runState{def: d, insts: map[string]*instance{}}}
	c.st.hash = e.definitionHash(d)
	assets, roots := effectiveAssets(d, e.opts.Assets), assetRoots(d, e.opts.Assets)
	if pred != nil && e.opts.Assets == "" && (pred.Assets != "" || len(pred.AssetRoots) > 0) {
		// A successor started with no --assets keeps its predecessor's
		// selection: its selected root and per-definition roots.
		assets = pred.Assets
		roots = (&cycle{e: e, st: &runState{started: pred}}).selectedAssetRoots(d)
		c.st.hash = definitionHashAssets(d, roots)
	}
	if rs := e.replayStarted; e.recordedReplay && rs != nil && (rs.Assets != "" || len(rs.AssetRoots) > 0) {
		// The run's pinned roots bind {assets} and hash the definition, as
		// its own resumes did, not the replaying binary's. A
		// legacy record carries only the selected root.
		assets, roots = rs.Assets, maps.Clone(rs.AssetRoots)
		if len(roots) == 0 {
			roots = assetRoots(d, assets)
		}
		c.st.hash = definitionHashAssets(d, roots)
	}
	build := currentBuild()
	err = c.emitAs("", "", 0, JournalRunStarted, 0, RunStartedData{
		PagedPrompts: pagedPrompts, HaltBefore: haltBefore, Snapshots: snapshots, Def: d.name, Version: d.version, Params: nilIfEmpty(normalizedParams), ParamPaths: paramPaths, Options: opts,
		Assets: assets, AssetRoots: roots, Worktrees: successorWorktrees(pred),
		Harness: harness, HarnessFromEnv: e.opts.HarnessFromEnv && harness == e.opts.Harness,
		Workspace: workspace, ModelMap: modelMap, Models: nilIfEmpty(models), Efforts: nilIfEmpty(efforts), ChildModels: childModels, Headless: headlessCfg, Budget: e.opts.RunBudget,
		Delegations: nilIfEmpty(delegations), Predecessor: predID, Given: given(e.opts.GivenParams, params),
		Detach: e.detachFact(), Shape: defShape(d), Manufactured: e.opts.manufactured, Build: &build,
	})
	if err != nil {
		return "", nil, err
	}
	if dg := preflightDegraded(d.name, c.st.started.Detach); dg != nil {
		if err := c.emitAs("", "", 0, JournalDegraded, 0, *dg); err != nil {
			return "", nil, err
		}
	}
	if tiersNote != nil {
		if err := c.emitAs("", "", 0, JournalDegraded, 0, *tiersNote); err != nil {
			return "", nil, err
		}
	}
	for _, hint := range e.opts.HarnessHints {
		if err := c.emitAs("", "", 0, JournalDegraded, 0, DegradedData{Kind: DegradedHarnessHint, Cause: hint, Action: "the run goes on"}); err != nil {
			return "", nil, err
		}
	}
	return id, nil, c.haltAtBreakpoint("")
}

// given is the sorted subset of names that params holds.
func given(names []string, params map[string]string) []string {
	var out []string
	for _, n := range names {
		if _, ok := params[n]; ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// copyTree copies the regular files and directories under src to dst; a
// missing src copies nothing.
func copyTree(src, dst string) error {
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, raw, 0o644)
	})
}

// resumeLine is the command that continues an ended run as a successor.
func (c *cycle) resumeLine() string {
	if c.e.opts.ResumeLine == nil || c.st.started == nil {
		return ""
	}
	started := *c.st.started
	started.self = c.run // the line continues this run's chain (--continue)
	return c.commandLine(c.e.opts.ResumeLine(started))
}

// normalizeAliases returns params with each alias name replaced by its param:
// the old name's value binds the new name unless the new name was given too.
// Input normalization only, so the run key and run_started never see old names.
func (d *Def) normalizeAliases(params map[string]string) map[string]string {
	if len(d.aliases) == 0 {
		return params
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	for old, name := range d.aliases {
		v, ok := out[old]
		if !ok {
			continue
		}
		delete(out, old)
		if _, has := params[name]; !has {
			out[name] = v
		}
	}
	return out
}

// resumeMatches keeps a resume from silently dropping what the caller passed:
// no options and no delegations inherit the journaled ones; any others must
// equal them.
func resumeMatches(id RunID, started RunStartedData, options json.RawMessage, delegations map[string]string) error {
	if len(options) > 0 && !sameJSON(started.Options, options) {
		return fmt.Errorf("run %s was started with different options; resume without --options to keep the journaled ones", id)
	}
	if len(delegations) > 0 && !maps.Equal(started.Delegations, delegations) {
		return fmt.Errorf("run %s was started with different delegations; resume without --delegate to keep the journaled ones", id)
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// runID: a supplied id, else the definition name plus a hash of the run-key
// values, else the definition name plus a nonce.
func (e *Engine) runID(d *Def, params map[string]string) (RunID, error) {
	return deriveRunID(d, params, e.opts.RunID, e.opts.Nonce)
}

// deriveRunID is runID without an engine, so the run handler can look up a
// saved run before it builds one.
func deriveRunID(d *Def, params map[string]string, explicit RunID, nonce func() string) (RunID, error) {
	if explicit != "" {
		return explicit, nil
	}
	if len(d.runKey) == 0 {
		return RunID(d.name + "-" + nonce()), nil
	}
	h := sha256.New()
	for _, p := range d.runKey {
		v, ok := params[p.name]
		if !ok {
			return "", fmt.Errorf("run key parameter %q not supplied", p.name)
		}
		fmt.Fprintf(h, "%s=%q\n", p.name, v)
	}
	return RunID(d.name + "-" + hex.EncodeToString(h.Sum(nil))[:12]), nil
}

// ---------------------------------------------------------------- Next, Status

// Next implements Executor. It is the wave alias over the paged state machine
// (spec 4.7): every pending unit (of child, when given) is handed out in
// handOutOrder and claimed at once under the synthetic session "wave", and the
// prompt rides inline, so the unit is running when Next returns. A repeat Next
// journals nothing and shows the same tokens.
func (e *Engine) Next(run RunID, child string) ([]Instruction, error) {
	var out []Instruction
	err := e.withCoordinatorNext(run, Coordinator{}, func(c *cycle) error {
		// Asking for work re-reads a Requery Map's source, as HandOut does: a
		// task its blocker's close made ready is offered now (item 96). A next
		// for one child's units asks for no new work.
		c.requery = child == ""
		if child != "" && c.st.insts[child] == nil {
			return fmt.Errorf("run %s has no instance %q", run, child)
		}
		if err := c.drive(); err != nil {
			return err
		}
		if c.st.breakpoint != nil && !c.st.ended {
			out = []Instruction{c.stoppedInstruction()}
			return nil
		}
		if ins, done := c.untilUnitInstruction(); done {
			out = []Instruction{ins}
			return nil
		}
		if c.st.ended {
			out = []Instruction{c.endedInstruction()}
			return nil
		}
		if c.waitingOnHolds() {
			out = []Instruction{c.waitingInstruction()}
			return nil
		}
		for _, in := range c.handOutOrder(c.pendingUnits()) {
			if child != "" && in.ID != child {
				continue
			}
			_, offered, err := c.handOut(in)
			if err != nil {
				return err
			}
			if !offered {
				continue
			}
			if err := c.claimForWave(in); err != nil {
				return err
			}
		}
		for _, id := range c.st.order {
			in := c.st.insts[id]
			if !c.untilUnitAllows(in) || in.Settled || (child != "" && id != child) {
				continue
			}
			if ins, ok := c.nonAgentInstruction(in); ok {
				out = append(out, ins)
				continue
			}
			if in.Phase != phaseIssued || in.Issued == nil || in.Unit == nil {
				continue
			}
			is := in.Issued
			out = append(out, Instruction{
				Run: run, State: c.st.qualified(in), Kind: InstructionAgent, Attempt: in.Attempt,
				Name: in.Unit.Name, Class: c.st.qualified(in),
				Token: is.Token, Prompt: is.Prompt, Inputs: is.Inputs, Submit: is.Submit, Child: id,
				Dispatch: is.Dispatch, GitAuthority: is.GitAuthority, Workspace: is.Workspace, Context: ContextFresh,
				PreviousRefusal: previousRefusal(in),
			})
		}

		if c.st.started.PagedPrompts {
			for i := range out {
				if out[i].Kind == InstructionAgent {
					out[i].Prompt = c.verbLine("prompt", out[i].Name)
					out[i].Inputs = nil
				}
			}
		}
		c.advise(out)
		return nil
	})
	return out, err
}

// advise sets the bootstrap's detach-none line on every instruction in out
// once the run has stepped down to none mid-run (an intent journaled on none;
// a run that started there was told by its bootstrap): its long commands now
// run inline inside next, which needs a long tool timeout. Next and HandOut
// return the ended and holds-only waiting instructions before it.
func (c *cycle) advise(out []Instruction) {
	if c.st.detach != DetachNone {
		return
	}
	for i := range out {
		out[i].Advice = detachNoneAdvice
	}
}

// nonAgentInstruction is the instruction of an instance that needs no
// subagent: a manual verification, a backoff wait or a presented gate or wait.
// ok is false when the instance has none.
func (c *cycle) nonAgentInstruction(in *instance) (Instruction, bool) {
	if ins, ok := c.recoveryInstruction(in); ok {
		return ins, true
	}
	// After drive an open effect intent is Uncertain: it waits for a human.
	if seq, e, ok := openIntent(in); ok && in.Phase != phaseHalted {
		if note, ok := c.detached[in.ID]; ok && note.wait {
			return *c.waitInstruction(in, note.why, c.e.opts.EffectPoll), true
		}
		return c.verification(in, seq, e), true
	}
	switch in.Phase {
	case phaseBackoff:
		if w := c.backoffInstruction(in); w != nil {
			return *w, true
		}
	case phaseReady:
		if c.st.breakpointBlocks(in) {
			return Instruction{}, false
		}
		if p, ok := in.node().kind.(presenter); ok {
			if ins := p.present(c, in, in.node()); ins != nil {
				return *ins, true
			}
		}
	}
	return Instruction{}, false
}

// LockState reads run's lock without taking it or reading the journal; a
// journal with no lock file to probe reports it free with no record.
func (e *Engine) LockState(run RunID) (LockState, error) {
	if err := validRunID(run); err != nil {
		return LockState{}, err
	}
	if h, ok := e.j.(interface{ lockState(RunID) LockState }); ok {
		return h.lockState(run), nil
	}
	return LockState{}, nil
}

// HandOut implements Executor: HandOutAs for a caller the engine cannot
// identify.
func (e *Engine) HandOut(run RunID, n int) ([]Instruction, error) {
	return e.HandOutAs(run, n, Coordinator{})
}

// driver is the harness the run's current segment is driven from (J1,
// an earlier revision): its first coordinator call's; "" while unbound (segmentDriver).
func (e *Engine) driver(run RunID) (h string, err error) {
	err = e.withDryRun(run, func(c *cycle) error { // a read: it never migrates or drives
		h, _ = c.segmentDriver()
		return nil
	})
	return h, err
}

// segmentDriver is the current segment's binding: its first coordinator
// call's harness and records root, else, in the run's first segment only, the
// harness the run was started under when the starter's environment showed a
// known one (J1). A segment a resume, a rewind or a fork opened has no seed.
func (c *cycle) segmentDriver() (h, root string) {
	cur := c.st.segment()
	if cur == nil {
		return "", ""
	}
	if cur.Harness != "" {
		return cur.Harness, cur.Root
	}
	if cur.Cause == SegmentStart && c.st.started != nil && c.st.started.HarnessFromEnv {
		if rh, legacy := recordedHarness(*c.st.started); !legacy && rh != "generic" {
			return rh, ""
		}
	}
	return "", ""
}

// AdoptOnly carries run onto the engine's definition as resume --adopt does
// (a declared Migration, else an adoption when EngineOptions.Adopt is set)
// and does nothing else: under the run's lock it folds and
// migrates, with no workspace relocation, no drive, no coordinator met (so
// no harness bound) and no unit handed out. adopted reports whether a
// migration was journaled; next is the coordinator's next line, built
// without a status read. An ended run is refused: it is never migrated.
func (e *Engine) AdoptOnly(run RunID) (adopted bool, next string, err error) {
	unlock, err := e.j.Lock(run)
	if err != nil {
		return false, "", err
	}
	defer unlock()
	recs, err := e.j.Records(run)
	if err != nil {
		return false, "", err
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return false, "", err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return false, "", err
	}
	c := &cycle{e: e, run: run, recs: recs, st: st}
	if st.ended {
		return false, "", fmt.Errorf("%w: adopt: run %s has ended, and an ended run is never adopted: rewind it (wf dev rewind --to died) and adopt the fork", ErrRejected, run)
	}
	before := len(c.recs)
	if err := c.migrate(def); err != nil {
		return false, "", err
	}
	return len(c.recs) > before, c.verbLine("next"), nil
}

// knownCoordinator reports whether session's next has already taken run as
// its coordinator (a coordinator record).
func (e *Engine) knownCoordinator(run RunID, session string) bool {
	known := false
	_ = e.withRun(run, func(c *cycle) error {
		_, known = c.st.coordOrder[session]
		return nil
	})
	return known
}

// Status implements Executor.
func (e *Engine) Status(run RunID) (RunStatus, error) { return e.status(run, false) }

// status is Status; strict reads the run lock-free or not at all
// (readOnly), for a caller that holds its lock.
func (e *Engine) status(run RunID, strict bool) (RunStatus, error) {
	var rs RunStatus
	err := e.readOnly(run, strict, func(c *cycle) error {
		rs = RunStatus{usesTracker: c.st.def.usesTracker(), PendingCleanups: c.pendingCleanups(), Workspace: c.workspace(), Work: c.st.work(), Run: run, Def: c.st.started.Def, Phase: RunRunning, Outcome: c.st.outcome, Terminal: c.st.term,
			Options: c.st.started.Options, Delegations: c.st.started.Delegations}
		rs.Harness, rs.LegacyHarness = recordedHarness(*c.st.started)
		rs.Predecessor = c.st.started.Predecessor
		rs.Manufactured = c.st.started.Manufactured
		if b := c.st.started.Build; b != nil {
			rs.Build, rs.BuildDiffers = b, b.differs(currentBuild())
		}
		if t := c.st.turn; t != nil {
			rs.CoordinatorTurn = &CoordinatorTurnStatus{Continuations: t.count, LastSeq: t.last, StallSeq: t.stall}
		}
		rs.Stopped = c.st.paused && !c.st.ended
		rs.Budget = c.st.budget
		if rs.Stopped && c.st.budgetStop != "" {
			rs.BudgetStop, rs.BudgetResume = c.st.budgetStop, c.budgetResumeLine()
		}
		if rs.Stopped {
			rs.Breakpoint = c.st.breakpoint
		}
		if f := c.runDetach(); f != nil {
			rs.Detach = f.Mode
		}
		for _, h := range slices.Sorted(maps.Keys(c.st.harnessChecks)) {
			rs.Children = append(rs.Children, c.st.harnessChecks[h])
		}
		if c.st.ended {
			rs.Resume = c.resumeLine()
		}
		rs.Tokens = c.st.tokens
		rs.Degraded = slices.Clone(c.st.degraded)
		for i, s := range c.st.segments {
			v := SegmentView{Start: s.Start, Cause: s.Cause, Harness: s.Harness, Units: len(s.units)}
			if i+1 < len(c.st.segments) {
				v.End = c.st.segments[i+1].Start - 1
			}
			if s.Harness != "" {
				sm := shown(c.segmentModels(i, s.Harness)) // bound: its journaled models, no refusal
				v.ModelMap, v.Models, v.ModelsFrom = sm.ModelMap, sm.Models, sm.From
			}
			rs.Segments = append(rs.Segments, v)
		}
		if len(c.st.segments) > 0 {
			// An unbound segment whose map its harness refuses shows none.
			rs.Models = modelsIfAny(c.currentModels(""))
		}
		if c.st.ended {
			rs.Phase = RunEnded
		}
		if _, held := c.heldAt(); held || c.waitingOnHolds() {
			rs.Phase = RunBlocked // the run waits for a Retry: a halt, or holds alone
		}
		for _, id := range c.st.order {
			in := c.st.insts[id]
			rs.Instances = append(rs.Instances, InstanceStatus{
				ID: id, State: in.Cur, Phase: in.Phase, Attempt: in.Attempt, Reason: in.statusReason(),
				Counters: copyCounts(in.Counters),
			})
		}
		if rs.Phase == RunEnded {
			rs.Cause = c.runCause()
		}
		if rs.Phase != RunEnded {
			for _, id := range c.st.order {
				if ins, ok := c.recoveryInstruction(c.st.insts[id]); ok {
					rs.Recoveries = append(rs.Recoveries, *ins.Recovery)
				}
			}
			rs.Held = c.heldUnits()
			rs.Fix, rs.FixNotes = c.fixLines()
		}
		c.statusUnits(&rs, c.now())
		c.statusReasons(&rs, c.now())
		return nil
	})
	return rs, err
}

// fixLines is RunStatus.Fix, each a command alone: for each exhaustion halt
// the run is held at, the retry answer and, when the state has a fallback
// edge, the stop answer (retry --stop); then, for each held unit, its fix
// (holdFix).
func (c *cycle) fixLines() (out, notes []string) {
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if !in.exhaustionHalt() || !c.heldAtInstance(in) {
			continue
		}
		retry := c.verbLine("retry")
		if id != "" {
			retry += " --child " + sink.Quote(id)
		}
		out, notes = append(out, retry), append(notes, "one more attempt")
		if edge := c.stopEdge(in); edge != "" {
			out, notes = append(out, retry+" --stop"), append(notes, "its fallback edge: "+edge)
		}
	}
	for _, h := range c.heldUnits() {
		out, notes = append(out, c.holdFix(h)), append(notes, "releases held "+h.Name+" once its cause is cleared")
	}
	return out, notes
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Settle implements Executor: it goes through SettleAs's checks with no
// settler and no evidence, so it settles nothing.
func (e *Engine) Settle(run RunID, effect int64, applied bool) error {
	return e.withRun(run, func(c *cycle) error { return c.settle(effect, applied, nil, "", &Operator{}) })
}

// SettleRun implements Executor, refused as Settle is.
func (e *Engine) SettleRun(run RunID, effect int64, r RunResult) error {
	return e.withRun(run, func(c *cycle) error { return c.settle(effect, true, &r, "", &Operator{}) })
}

// SettleAs settles the open effect as Settle (rr nil) or SettleRun does,
// for the caller by: evidence, what it checked in the world and what that
// showed, is required and kept in the effect_settled record with who
// settled. An effect outside the run (wf.Outside, by its intent or, for an
// older intent, the definition the run is on) is refused to a coordinator
// session of the run and to an operator that names no one, no reason or no
// identity (Operator).
func (e *Engine) SettleAs(run RunID, effect int64, applied bool, rr *RunResult, evidence string, by Operator) error {
	return e.withRun(run, func(c *cycle) error { return c.settle(effect, applied, rr, evidence, &by) })
}

// ---------------------------------------------------------------- Retry

// Retry implements Executor. An ended run is rejected before anything else:
// the recovery point is the halt before the terminal, and no ended run is
// reopened. stop answers an exhaustion halt (instance.exhaustionHalt) only: it
// journals retried with Answer RetryStop and the fallback plan computed from
// the halt's kind now, and the fold takes that edge. A human retry never sets
// Delegated, so it never spends the visit's delegated retry.
func (e *Engine) Retry(run RunID, child string, stop bool) error {
	return e.retry(run, child, stop, 0)
}

// ReleaseApproval implements Executor.
func (e *Engine) ReleaseApproval(run RunID, child string, holdSeq int64) error {
	if holdSeq <= 0 {
		return fmt.Errorf("%w: expected a positive approval hold sequence", ErrRejected)
	}
	return e.retry(run, child, false, holdSeq)
}

func (e *Engine) retry(run RunID, child string, stop bool, approvalHold int64) error {
	return e.withRun(run, func(c *cycle) error {
		if c.st.ended {
			return fmt.Errorf("%w: run %s already ended", ErrRejected, run)
		}
		in := c.st.insts[child]
		if in == nil {
			return fmt.Errorf("run %s has no instance %q", run, child)
		}
		if err := c.checkUntilUnitMutation(in); err != nil {
			return err
		}
		h := in.heldUnit()
		if approvalHold != 0 || (h != nil && h.approvalHold != 0) {
			if h == nil || in.Settled || h.approvalHold == 0 {
				return fmt.Errorf("%w: no current approval hold for instance %q", ErrRejected, child)
			}
			if approvalHold != h.approvalHold {
				return fmt.Errorf("%w: current approval hold for %s is %d; use its printed retry command", ErrRejected, h.Name, h.approvalHold)
			}
		}
		if h != nil && !in.Settled {
			if stop {
				return fmt.Errorf("%w: instance %q holds a unit, not an exhaustion halt: --stop does not apply", ErrRejected, child)
			}
			return c.releaseHeld(in, h)
		}
		if !in.halted() {
			return fmt.Errorf("%w: instance %q is not halted and holds no unit", ErrRejected, child)
		}
		if child != "" && !c.mapUndecided(in) {
			return fmt.Errorf("%w: the spawner of instance %q already decided", ErrRejected, child)
		}
		rd := RetriedData{Child: child, Reason: in.Reason}
		if stop {
			if !in.exhaustionHalt() {
				return fmt.Errorf("%w: instance %q is not halted by retry exhaustion: --stop does not apply", ErrRejected, child)
			}
			if c.stopEdge(in) == "" {
				return fmt.Errorf("%w: state %s has no fallback edge: --stop does not apply", ErrRejected, in.Cur)
			}
			p, err := c.stopPlan(in)
			if err != nil {
				return err
			}
			rd.Answer, rd.Plan = RetryStop, &p
		}
		if err := c.emit(in, JournalRetried, 0, rd); err != nil {
			return err
		}
		return c.drive()
	})
}

// ---------------------------------------------------------------- Submit, Fail

// Submit implements Executor.
func (e *Engine) Submit(run RunID, s Submission) (SubmitResult, error) {
	if err := e.withDryRun(run, func(c *cycle) error { return c.submissionResultPaths(s) }); err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err := e.withRun(run, func(c *cycle) error {
		var err error
		res, err = c.submit(s)
		return err
	})
	return res, err
}

// submit takes a submission by token: the body of Submit and of Done.
func (c *cycle) submit(s Submission) (SubmitResult, error) {
	res, in, err := c.classify(s)
	if err != nil || in == nil {
		return res, err
	}
	c.driveAfterDecision(in)
	in = c.current(in)
	res.Next = in.Cur
	return res, nil
}

// driveAfterDecision drives after a verb journaled its own decision for in
// (a child's result accepted or refused, a failure recorded). An error from
// that drive (a definition another call adopted meanwhile, the lock lost on
// a yield, a failed append) does not fail the verb: its decision is in the
// journal and stands. It is not hidden either: it is
// printed with the run and the unit, and noted in the journal for the
// coordinator, as far as the journal can still be written.
func (c *cycle) driveAfterDecision(in *instance) {
	err := c.drive()
	if err == nil {
		return
	}
	unit := c.st.unitName(c.current(in))
	if unit == "" {
		unit = in.ID
	}
	text := fmt.Sprintf("run %s, unit %s: your result was recorded; the run hit an error afterwards that the coordinator must see: %v", c.run, unit, err)
	if w := c.e.opts.Progress; w != nil {
		fmt.Fprintln(w, text)
	}
	_ = c.emitAs("", "", 0, JournalNote, 0, NoteData{Text: text}) // best effort: the journal may be what failed
}

// current is in as the cycle's state holds it now: a drive that reloaded
// the cycle (yieldTurn) folded a new state, and in is its instance there.
func (c *cycle) current(in *instance) *instance {
	if now := c.st.insts[in.ID]; now != nil {
		return now
	}
	return in
}

// classify is submit up to its verdict: the token check, then the state's
// submit (or the replayed decision of a journaled one). It never drives, so
// no edge effect runs; validate calls it on a dry cycle. A nil instance is a
// rejection already journaled.
func (c *cycle) classify(s Submission) (SubmitResult, *instance, error) {
	var res SubmitResult
	in := c.st.insts[s.Child]
	if in == nil {
		return res, nil, fmt.Errorf("run %s has no instance %q", c.run, s.Child)
	}
	if s.Child == "" {
		in = c.byToken(s.Token, in)
	}
	if err := c.checkUntilUnitMutation(in); err != nil {
		return res, nil, err
	}
	n := in.node()
	if reason := c.tokenProblem(in, s.Token, s.Attempt); reason != "" {
		return res, nil, c.reject(in, s.Token, reason, &res)
	}
	sub, ok := n.kind.(submitter)
	if !ok {
		return res, nil, c.reject(in, s.Token, fmt.Sprintf("state %s takes no submission", n.name), &res)
	}
	var err error
	if u := in.Undecided; u != nil && u.Token == s.Token {
		// The submission is already journaled: replay its decision.
		res, err = sub.decide(c, in, n, replayOf(in, u))
	} else {
		if err := c.resultRunPaths(s.Output); err != nil {
			return SubmitResult{}, nil, err
		}
		res, err = sub.submit(c, in, n, s)
	}
	return res, in, err
}

// Fail implements Executor.
func (e *Engine) Fail(run RunID, token, reason string) (SubmitResult, error) {
	var res SubmitResult
	err := e.withRun(run, func(c *cycle) error {
		var err error
		res, err = c.failToken(token, reason, FailTransient)
		return err
	})
	return res, err
}

// failToken abandons the attempt that token names: the body of Fail and of
// Failed. It is the only place a FailureClass is matched. A blocked failure
// goes through the class table: a state with a Blocked route (own or the
// definition default) takes it, the cause bound as {blockedCause}; a state
// with none halts (lint rule agent-blocked-route keeps that from happening
// in a lint-clean definition). The
// result's Failure is read from the instance after the journaled records and
// the drive, never composed apart from them.
func (c *cycle) failToken(token, reason string, class FailureClass) (SubmitResult, error) {
	var res SubmitResult
	in := c.byToken(token, c.st.insts[""])
	if err := c.checkUntilUnitMutation(in); err != nil {
		return res, err
	}
	if r := c.tokenProblem(in, token, 0); r != "" {
		return res, c.reject(in, token, r, &res)
	}
	if class == FailApproval {
		return c.holdApproval(in, reason)
	}
	n := in.node()
	before := in.Attempt
	oc, why := failedOutcome(reason, class)
	if err := c.failByChild(in, n, oc, []string{why}, why); err != nil {
		return res, err
	}
	res.Class, res.Refusal = oc, []string{why}
	c.driveAfterDecision(in)
	in = c.current(in)
	res.Next = in.Cur
	switch {
	case in.Phase == phaseHalted && in.Recovery:
		msg := "offered for recovery: " + in.Reason
		if ins, ok := c.recoveryInstruction(in); ok {
			msg += "; answer with one of:\n  " + ins.Recovery.Commands[RecoveryRestart] + "\n  " + ins.Recovery.Commands[RecoveryGiveUp]
		}
		res.Failure = &FailureOutcome{Disposition: DispositionRecovery, Message: msg}
	case in.Phase == phaseHalted:
		res.Failure = &FailureOutcome{Disposition: DispositionHalted, Message: "halted: " + in.Reason}
	case oc == Blocked:
		res.Failure = &FailureOutcome{Disposition: DispositionRouted, Message: "attempt " + strconv.Itoa(before) + " blocked: " + reason + "; next: " + in.Cur}
	case in.Attempt > before && !in.Settled && in.Cur == n.name:
		res.Failure = &FailureOutcome{Disposition: DispositionQueued, Message: fmt.Sprintf("attempt %d queued", in.Attempt)}
	default:
		res.Failure = &FailureOutcome{Disposition: DispositionRouted, Message: "attempt " + strconv.Itoa(before) + " abandoned; next: " + in.Cur}
	}
	return res, nil
}

// failedOutcome is the class and reason a child's failed journals.
func failedOutcome(reason string, class FailureClass) (OutcomeClass, string) {
	if class == FailBlocked {
		return Blocked, reason
	}
	return Refused, "abandoned: " + reason
}

// byToken returns the instance that ever issued token, else def.
func (c *cycle) byToken(token string, def *instance) *instance {
	for _, id := range c.st.order {
		if _, ok := c.st.insts[id].Tokens[token]; ok {
			return c.st.insts[id]
		}
	}
	return def
}

// tokenProblem is "" when token is the instance's open attempt, else why not.
func (c *cycle) tokenProblem(in *instance, token string, attempt int) string {
	if !c.untilUnitAllows(in) {
		return "outside immutable until-unit boundary"
	}
	info, known := in.Tokens[token]
	switch {
	case in.Phase == phaseIssued && in.Token == token:
		if h := in.heldUnit(); h != nil {
			return "held: needs a human: " + h.Cause
		}
		if attempt != 0 && attempt != in.Attempt {
			return fmt.Sprintf("attempt %d does not match the open attempt %d", attempt, in.Attempt)
		}
		return ""
	case !known:
		return "unknown token"
	case in.Phase == phaseIssued && info.State == in.Cur:
		return fmt.Sprintf("superseded: attempt %d of %s was replaced by attempt %d", info.Attempt, info.State, in.Attempt)
	}
	return fmt.Sprintf("stale: attempt %d of %s is already settled", info.Attempt, info.State)
}

func (c *cycle) reject(in *instance, token, reason string, res *SubmitResult) error {
	if err := c.emit(in, JournalRejected, Rejected, RejectedData{Token: token, Reason: reason}); err != nil {
		return err
	}
	*res = SubmitResult{Class: Rejected, Refusal: []string{reason}, Next: in.Cur}
	return nil
}

// ---------------------------------------------------------------- drive

// drive runs deterministic work until every instance waits on an agent, is
// done or halted. A drive that let the lock go to a waiting call and finds
// that call's records starts again from the top, on a cycle reloaded as a
// resumed process loads it (yieldTurn).
func (c *cycle) drive() error {
	for {
		if err := c.driveOnce(); !errors.Is(err, errRedrive) {
			return err
		}
	}
}

// errRedrive is a drive that must start again from the top: another call
// appended to the run while this one had let the lock go, and the cycle has
// been reloaded.
var errRedrive = errors.New("drive again from the top")

func (c *cycle) driveOnce() error {
	if c.st.untilUnit != nil {
		return c.driveUntilUnit()
	}
	if err := c.haltAtBreakpoint(c.st.order...); err != nil {
		return err
	}
	if err := c.drainCleanups(); err != nil {
		return err
	}
	if err := c.resolveChildren(); err != nil {
		return err
	}
	if err := c.sampleTranscripts(); err != nil {
		return err
	}
	if err := c.finishLostCaps(); err != nil {
		return err
	}
	if err := c.applyRecoveryAnswers(); err != nil {
		return err
	}
	if err := c.loseEndedChildren(); err != nil {
		return err
	}
	if err := c.loseGoneHeadless(); err != nil {
		return err
	}
	if err := c.expireTimers(); err != nil {
		return err
	}
	for guard := 0; guard < 100000; guard++ {
		if err := c.drainCleanups(); err != nil {
			return err
		}
		if c.st.ended || c.st.paused {
			return nil
		}
		progressed := false
		for _, id := range slices.Clone(c.st.order) {
			in := c.st.insts[id]
			if in.Settled {
				continue // a settled child never runs again
			}
			before, instances := len(c.recs), len(c.st.order)
			var err error
			switch in.Phase {
			case phaseReady:
				n := in.node()
				if n == nil {
					return fmt.Errorf("%w: instance %q is at unknown state %q", ErrCorruptJournal, id, in.Cur)
				}
				var stopped, guarded bool
				if stopped, err = c.budgetGate(in, n); err == nil && !stopped {
					if guarded, err = c.takeGuard(in, n); err == nil && !guarded {
						err = n.kind.execute(c, in, n)
					}
				}
			case phaseTransition:
				err = c.transit(in)
			case phaseBackoff:
				if d, ok := c.dueFor(in, deadlineBackoff, c.now()); ok && !c.now().Before(d.Due) {
					switch {
					case in.Pending != nil:
						err = c.transit(in) // an edge whose effect was unavailable: retry it
					default:
						var stopped bool
						if stopped, err = c.budgetGate(in, in.node()); err == nil && !stopped {
							err = in.node().kind.execute(c, in, in.node())
						}
					}
				}
			case phaseIssued:
				// A crash between submitted and its decision: decide again from
				// the journaled output, with no new submission.
				if u := in.Undecided; u != nil && u.Token == in.Token {
					if sub, ok := in.node().kind.(submitter); ok {
						_, err = sub.decide(c, in, in.node(), replayOf(in, u))
					}
				}
			}
			if err != nil {
				return err
			}
			if len(c.recs) > before {
				if in.arrivalSeq > int64(before) {
					if err := c.haltAtBreakpoint(id); err != nil || c.st.breakpoint != nil {
						return err
					}
				}
				if err := c.haltAtBreakpoint(c.st.order[instances:]...); err != nil || c.st.breakpoint != nil {
					return err
				}
				progressed = true
				// A step is whole in the journal: another call waiting for
				// the run goes first, and its records are read before the
				// next step.
				if err := c.yieldTurn(); err != nil {
					return err // errRedrive too: the cycle was reloaded
				}
			}
		}
		if !progressed {
			return nil
		}
	}
	return errors.New("engine did not settle")
}

// yieldedHook, when set, runs each time a drive has the run lock back after
// letting it go to a waiting call (tests).
var yieldedHook func(RunID)

// lockYielder is a journal whose held run lock can go to a call waiting for
// it and come back (the file journal's turnstile).
type lockYielder interface {
	othersWaiting(run RunID) bool
	yieldLock(run RunID) error
}

// yieldTurn lets the run lock go between two drive steps when another call
// waits for it: a done's chain of merges, closes and snapshots no
// longer keeps a child's prompt out for its whole length. Each step is whole
// in the journal. When the other call appended nothing, the drive goes on.
// When it appended anything, the drive does not trust what it holds in
// memory: as after a crash, the cycle is reloaded from the journal with the
// definition loaded afresh (reload) and errRedrive starts the drive again
// from the top, where an until-unit bound or a changed definition is met as
// a resumed process meets it. A dry cycle never yields.
func (c *cycle) yieldTurn() error {
	y, ok := c.e.j.(lockYielder)
	if c.dry || !ok || !y.othersWaiting(c.run) {
		return nil
	}
	before := len(c.recs)
	if err := y.yieldLock(c.run); err != nil {
		return err
	}
	if h := yieldedHook; h != nil {
		h(c.run)
	}
	recs, err := c.e.j.Records(c.run)
	if err != nil {
		return err
	}
	switch {
	case len(recs) < before:
		return fmt.Errorf("%w: run %s has %d records after a yield, %d before", ErrCorruptJournal, c.run, len(recs), before)
	case len(recs) == before:
		return nil
	}
	if c.coordinatorNext {
		if err := turnFence(recs, c.caller); err != nil {
			return err
		}
	}
	if err := c.reload(recs); err != nil {
		return err
	}
	return errRedrive
}

// reload makes c the cycle a call that took the lock now would build: recs
// folded under the definition loaded afresh, migrated or refused as at the
// verb's entry (withRunMode), its fork orphans lost, a coordinator's segment
// bound again, and every note of what this cycle saw before dropped. The
// verb's own parameters (its caller, what it hands out) stay.
func (c *cycle) reload(recs []JournalRecord) error {
	def, err := c.e.defFor(recs[0])
	if err != nil {
		return err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return err
	}
	c.recs, c.st = recs, st
	c.issuing = nil
	c.polled, c.transient, c.detached = nil, nil, nil
	c.requeried, c.unstatable, c.turnWaitingMaps = nil, nil, nil
	c.captured, c.capturedStamp = nil, ""
	c.stats, c.statsAt = nil, 0
	c.docsMu.Lock()
	c.docs = nil
	c.docsMu.Unlock()
	if st.ended {
		return nil
	}
	if err := c.migrate(def); err != nil {
		return err
	}
	if err := c.loseForkOrphans(); err != nil {
		return err
	}
	return c.bindSegment(c.caller)
}

// transit performs the chosen edge: external effects in order, then scope
// resets and counter increments, then the transition. A crash in this window
// resumes from the folded pending edge.
func (c *cycle) transit(in *instance) error {
	p := in.Pending
	if c.lockedOut(in) {
		return nil // another instance holds a lock the target state needs
	}
	var externals []EffectData
	var incs []string
	var to string
	for _, s := range p.Plan.Steps {
		to = s.To
		for _, ef := range s.Effects {
			if ef.Kind == "inc" {
				incs = append(incs, ef.Counter)
			} else {
				externals = append(externals, ef)
			}
		}
	}
	if waiting, err := c.applyExternals(in, externals); err != nil || waiting {
		return err // an Uncertain effect holds the instance, or it left the edge
	}
	if class, why, failed := p.stopped(); failed {
		return c.edgeFailed(in, class, why)
	}
	// B4: an Optional edge going on past a failed effect is recorded, so the
	// report lists it (verify no longer fails such a run).
	action := "went on to " + to + " (Optional edge)"
	for _, s := range p.Plan.Steps {
		if s.RoutedBy != "" {
			action = "went on to " + to + ", which routes it: " + s.RoutedBy
		}
	}
	for _, e := range p.Results {
		if effectFailed(e.Outcome) {
			d := DegradedData{Unit: c.st.qualified(in), Kind: DegradedOptional,
				Cause: fmt.Sprintf("%s.%s %s: %s", e.Adapter, e.Op, e.Outcome, e.Error), Action: action}
			if err := c.emit(in, JournalDegraded, 0, d); err != nil {
				return err
			}
		}
	}
	scoped := map[string][]string{}
	for _, cn := range in.def.counters {
		if cn.scope != "" {
			scoped[cn.scope] = append(scoped[cn.scope], cn.name)
		}
	}
	for _, name := range incs {
		if cs := scoped[name]; len(cs) > 0 {
			if err := c.emit(in, JournalScopeReset, 0, ScopeResetData{Scope: name, Counters: cs}); err != nil {
				return err
			}
		}
	}
	return c.emit(in, JournalTransition, 0, TransitionData{To: to, Incs: incs})
}

// edgeFailed stops a transition whose external effect failed (A1, fail
// closed): the source state takes its failure route for class, as a failed
// attempt of its own would, and the refused record replaces the pending edge
// with the route's plan, a halt or a re-issue of the source state. An
// unavailable effect at a state with no Unavailable route takes the Blocked
// route (with the effect as the cause). An edge that a failure decision chose
// (a failure or exhausted route) whose own effect fails halts: deciding again
// would take the same edge and fail the same way.
func (c *cycle) edgeFailed(in *instance, class OutcomeClass, why string) error {
	reason := "edge effect " + why
	if in.Pending.Failure {
		return c.halt(in, "the failure route's "+reason)
	}
	n := in.node()
	if class == Unavailable {
		// B1: a transient outage is retried on the state's Unavailable
		// schedule (DefaultUnavailableBackoff when it routes none) before
		// anything fails: the backoff's due time re-runs this edge.
		if scheduled, err := c.backoff(in, n, Unavailable, []string{reason}, false); scheduled || err != nil {
			return err
		}
	}
	if _, ok := classRoute(n, class); class == Unavailable && !ok {
		class = Blocked
	}
	rd, err := c.decide(in, n, class, []string{reason}, "")
	if err != nil {
		return err
	}
	rd.Reasons = []string{reason} // no backoff ran: decide's "still unavailable after N retries" does not apply
	return c.emit(in, JournalRefused, class, rd)
}

// ---------------------------------------------------------------- plans

// planOf builds the journaled plan for a chosen edge use: the edge, then the
// MaxVisits over edge of any target whose arrivals are spent. A target's
// budget is read over the counters as transit leaves them for the steps
// planned so far: the arriving transition's scope resets and Incs apply first.
func (c *cycle) planOf(in *instance, u edgeUse, output ...json.RawMessage) (EdgePlan, error) {
	view := in
	if len(output) > 0 {
		// The accepting record publishes its output and plan together. Explicit
		// path templates must see that output before their durable plan is built.
		copy := *in
		copy.Docs = append(append([]namedDoc(nil), in.Docs...), namedDoc{Name: in.Cur, Raw: output[0]})
		view = &copy
	}
	var p EdgePlan
	for depth := 0; depth < 16; depth++ {
		step, err := c.stepOf(view, u)
		if err != nil {
			return p, err
		}
		p.Steps = append(p.Steps, step)
		to := u.e.to
		if to == nil || to.maxVisits == nil || in.Visits[to.name] < evalCounters(to.maxVisits.max, in.countersAfter(p.Steps)) {
			return p, nil
		}
		u = to.maxVisits.over
	}
	return p, errors.New("MaxVisits over edges loop")
}

// countersAfter is the instance's counters as transit leaves them once the
// plan's steps are taken: the counters scoped to any incremented counter are
// zeroed, then every Inc applies, in that order.
func (in *instance) countersAfter(steps []EdgeStep) map[string]int {
	var incs []string
	for _, s := range steps {
		for _, ef := range s.Effects {
			if ef.Kind == "inc" {
				incs = append(incs, ef.Counter)
			}
		}
	}
	if len(incs) == 0 {
		return in.Counters
	}
	out := maps.Clone(in.Counters)
	if out == nil {
		out = map[string]int{}
	}
	for _, name := range incs {
		for _, cn := range in.def.counters {
			if cn.scope == name {
				out[cn.name] = 0
			}
		}
	}
	for _, name := range incs {
		out[name]++
	}
	return out
}

func (c *cycle) stepOf(in *instance, u edgeUse) (EdgeStep, error) {
	if u.e == nil || u.e.from == nil || u.e.to == nil {
		return EdgeStep{}, errors.New("zero edge")
	}
	idx := slices.Index(u.e.from.edges, u.e)
	if idx < 0 {
		return EdgeStep{}, fmt.Errorf("edge %q is not declared on state %s", u.e.label, u.e.from.name)
	}
	s := EdgeStep{From: u.e.from.name, Index: idx, To: u.e.to.name, Label: u.e.label, Optional: u.optional, RoutedBy: u.routedBy}
	for _, ef := range applyOrder(u.effects) {
		var effect ExternalEffect
		var literal bool
		switch x := ef.(type) {
		case incEffect:
			s.Effects = append(s.Effects, EffectData{Kind: "inc", Counter: x.c.name})
			continue
		case ExternalEffect:
			effect = x
		case runtimeEffect:
			effect, literal = x.ExternalEffect, true
		default:
			return s, fmt.Errorf("unknown effect %T", ef)
		}
		d := EffectData{Kind: "external", Adapter: effect.Adapter, Op: effect.Op, Outside: effect.Outside, Literal: literal}
		d.ArgTemplates = effect.argTemplates
		switch k := effect.Key.(type) {
		case nil:
		case nonIdem:
			d.NonIdempotent, d.Reason = true, string(k)
		case textKey:
			d.Key, d.KeyTemplate = k.key(), k.text.spans
		default:
			d.Key = k.key()
		}
		if literal || len(d.ArgTemplates) != 0 || d.KeyTemplate != nil {
			d.BindingVersion = effectBindingVersion
		}
		raw, err := c.marshalPathValueMode(in, effect.Args, d.BindingVersion == 0)
		if err != nil {
			return s, fmt.Errorf("effect %s args: %w", effect.effect(), err)
		}
		if err := json.Unmarshal(raw, &d.Args); err != nil {
			return s, err
		}
		s.Effects = append(s.Effects, d)
	}
	return s, nil
}

func describeUse(u edgeUse) string {
	if u.e == nil {
		return "<zero edge>"
	}
	to := "?"
	if u.e.to != nil {
		to = u.e.to.name
	}
	return fmt.Sprintf("%s -> %s", u.e.label, to)
}

// ---------------------------------------------------------------- failures

type retrier interface{ retryPolicy() *retryData }

func retryOf(n *node) *retryData {
	if r, ok := n.kind.(retrier); ok {
		return r.retryPolicy()
	}
	return nil
}

// classRoute is the fallback edge for a class: the state's own OnClass route,
// else the definition's OnFailure default (C2), which Def.seal added to the
// state as an edge of its own.
func classRoute(n *node, class OutcomeClass) (edgeUse, bool) {
	if k, ok := n.kind.(*mapKind); ok {
		if u, own := k.byClass[class]; own {
			return u, true
		}
	}
	if r := retryOf(n); r != nil {
		if u, ok := r.byClass[class]; ok {
			return u, true
		}
	}
	u, ok := n.defaults[class]
	return u, ok
}

// decide applies the class table to a failed attempt, in order: a class with a
// route (own or the definition default) takes it; Blocked with no route halts;
// Unavailable spends an attempt when the state has a retry policy (and halts
// when it has none); failAs turns such a halt of an agent state into a
// recovery offer to the coordinator (haltOffered); otherwise spend one attempt: a stalled fingerprint or
// spent budget takes the declared fallback edge, else the attempt is
// re-issued. The declared route always wins on exhaustion: nothing here halts
// an agent state for a human (C3).
func (c *cycle) decide(in *instance, n *node, class OutcomeClass, reasons []string, fp string) (rd RefusedData, err error) {
	rd = RefusedData{Reasons: reasons, Fingerprint: fp}
	if u, ok := classRoute(n, class); ok {
		p, err := c.planOf(in, u)
		rd.Plan = &p
		return rd, err
	}
	if class == Stale && in.Stales < StaleReissues {
		return rd, nil // input observations changed: a free re-issue
	}
	r := retryOf(n)
	if class == Blocked || r == nil {
		if class == Unavailable {
			rd.Reasons = append(slices.Clone(reasons), fmt.Sprintf("still unavailable after %d retries", len(DefaultUnavailableBackoff)))
		}
		rd.Halt = true
		return rd, nil
	}
	rd.Spent = true
	fps := append(slices.Clone(in.FPs), fp)
	var take *edgeUse
	kind := ExhaustedBudget
	switch {
	case r.noProgress > 0 && trailingEqual(fps) >= r.noProgress:
		take, kind = &r.exhausted, ExhaustedStalled
		if r.stalled != nil {
			take = r.stalled
		}
	case in.Spent+1 >= in.eval(r.max):
		take = &r.exhausted
	}
	if take != nil {
		return rd, c.exhausted(in, n, &rd, *take, kind)
	}
	return rd, nil
}

// exhausted decides a spent budget, stall or lost cap at n: the recovery
// offer to the coordinator when it is due (B2), else the declared edge take.
// The offer is the exhaustion halt with Recovery set; its answers are Retry
// (restart: one more attempt) and Retry stop (give up: take the edge).
func (c *cycle) exhausted(in *instance, n *node, rd *RefusedData, take edgeUse, kind string) error {
	if c.recoveryDue(in, n) {
		rd.Halt, rd.Exhausted, rd.Recovery = true, kind, true
		return nil
	}
	rd.Exhausted = kind // with a Plan: the declared edge, recorded by noteExhausted
	return c.routeTo(in, rd, take)
}

// noteExhausted journals, after the refused record rd that took a state's
// exhausted (or stalled) edge with no recovery offer, a degraded record of
// kind exhausted (B4): an unrecovered failure the report counts.
func (c *cycle) noteExhausted(in *instance, from string, rd RefusedData) error {
	if rd.Plan == nil || rd.Exhausted == "" || rd.Halt || len(rd.Plan.Steps) == 0 {
		return nil
	}
	name := from
	if in.Unit != nil && in.Unit.Name != "" {
		name = in.Unit.Name
	}
	return c.emit(in, JournalDegraded, 0, DegradedData{Unit: name, Kind: DegradedExhausted,
		Cause: strings.Join(rd.Reasons, "; "), Action: "took " + rd.Plan.Steps[0].Label})
}

// offersRecovery reports whether n's exhaustion is offered to the coordinator
// first: an agent state whose retry policy does not opt out (NoRecovery).
func offersRecovery(n *node) bool {
	_, agent := n.kind.(*agentKind)
	r := retryOf(n)
	return agent && r != nil && !r.noRecovery
}

// haltOffered reports whether a failure of in at n that has no route (a
// Blocked failure with no Blocked route, a failure of a state with no retry
// policy) is offered to the coordinator as a recovery, with its cause,
// instead of halting for a person: an agent state whose retry policy does
// not opt out (NoRecovery), while the unit has restarts left in the run.
// With none left it halts as before, so restart after restart still ends.
// The offer is the halt with Recovery set and no Exhausted kind.
func (c *cycle) haltOffered(in *instance, n *node) bool {
	_, agent := n.kind.(*agentKind)
	r := retryOf(n)
	return agent && (r == nil || !r.noRecovery) && in.Restarts[n.name] < RecoveryRestarts
}

// recoveryDue reports whether the exhaustion of in at n is offered: the state
// offers recovery, this visit's offer is unanswered and the unit has restarts
// left in the run.
func (c *cycle) recoveryDue(in *instance, n *node) bool {
	return offersRecovery(n) && !in.RecoveryUsed && in.Restarts[n.name] < RecoveryRestarts
}

// stopPlan is the fallback plan the stop answer of in's exhaustion halt takes,
// computed from the halt's kind when it is answered: the stalled edge if the
// policy declares one and the halt was a stall, else the exhausted edge.
func (c *cycle) stopPlan(in *instance) (EdgePlan, error) {
	r := retryOf(in.node())
	if r == nil {
		return EdgePlan{}, fmt.Errorf("state %s has no retry policy", in.Cur)
	}
	u := r.exhausted
	if in.Exhausted == ExhaustedStalled && r.stalled != nil {
		u = *r.stalled
	}
	return c.planOf(in, u)
}

// stopEdge is the label of the edge stopPlan takes.
func (c *cycle) stopEdge(in *instance) string {
	r := retryOf(in.node())
	if r == nil {
		return ""
	}
	if in.Exhausted == ExhaustedStalled && r.stalled != nil {
		return r.stalled.e.label
	}
	return r.exhausted.e.label
}

// routeTo sets rd's plan to the edge u takes.
func (c *cycle) routeTo(in *instance, rd *RefusedData, u edgeUse) error {
	p, err := c.planOf(in, u)
	rd.Plan = &p
	return err
}

// exhaust journals the failure route an exhausted retry takes, for an attempt
// that settled failed without spending a retry (the lost cap): the state's
// declared exhausted edge when it has a retry policy, else a halt (the
// instance's Reason, so status shows it). The declared route always wins: the
// engine never halts an agent state for a human on exhaustion (C3).
func (c *cycle) exhaust(in *instance, n *node, reason string) error {
	rd := RefusedData{Reasons: []string{reason}}
	if r := retryOf(n); r != nil {
		if err := c.exhausted(in, n, &rd, r.exhausted, ExhaustedBudget); err != nil {
			return err
		}
	} else {
		rd.Halt = true
	}
	from := c.st.qualified(in)
	if err := c.emit(in, JournalRefused, Refused, rd); err != nil {
		return err
	}
	return c.noteExhausted(in, from, rd)
}

func trailingEqual(fps []string) int {
	n := 0
	for i := len(fps) - 1; i >= 0 && fps[i] == fps[len(fps)-1]; i-- {
		n++
	}
	return n
}

// fail journals a failed attempt of class as refused.
func (c *cycle) fail(in *instance, n *node, class OutcomeClass, reasons []string, fp string) error {
	return c.failAs(in, n, class, reasons, fp, failEngine)
}

// failByChild fails the attempt for the child's own failed verb: its record
// is marked ChildFailed, the only failure an identical repeat is told is
// already recorded.
func (c *cycle) failByChild(in *instance, n *node, class OutcomeClass, reasons []string, fp string) error {
	return c.failAs(in, n, class, reasons, fp, failChild)
}

// failOrigin is who ended an attempt failAs journals.
type failOrigin int

const (
	failEngine    failOrigin = iota // the engine or the coordinator
	failSubmitted                   // a validation refusal of the submitted output
	failChild                       // the child's own failed verb
	failGaveUp                      // the coordinator's give-up of an ended child: a halt it causes is not offered back to it
)

// failSubmitted fails the attempt as Refused for a validation refusal of its
// submitted output: the refused record is marked Submitted, so a later attempt
// is shown its reasons.
func (c *cycle) failSubmitted(in *instance, n *node, reasons []string, fp string) error {
	return c.failAs(in, n, Refused, reasons, fp, failSubmitted)
}

func (c *cycle) failAs(in *instance, n *node, class OutcomeClass, reasons []string, fp string, origin failOrigin) error {
	if scheduled, err := c.backoff(in, n, class, reasons, origin == failChild); scheduled || err != nil {
		return err
	}
	rd, err := c.decide(in, n, class, reasons, fp)
	if err != nil {
		return err
	}
	rd.Submitted, rd.ChildFailed = origin == failSubmitted, origin == failChild
	if rd.Halt && !rd.Recovery && origin != failGaveUp && c.haltOffered(in, n) {
		// The failure has no route: the coordinator is offered the unit
		// with its cause before anything waits on a person.
		rd.Recovery = true
	}
	from := c.st.qualified(in)
	if err := c.emit(in, JournalRefused, class, rd); err != nil {
		return err
	}
	return c.noteExhausted(in, from, rd)
}

// halt journals a blocked halt without consulting any route (used mid-transition).
func (c *cycle) halt(in *instance, reasons ...string) error {
	return c.emit(in, JournalRefused, Blocked, RefusedData{Reasons: reasons, Halt: true})
}

// failErr fails the attempt Blocked on err, unless err is a definition bug
// (A3): an unresolved placeholder, an unbound prompt placeholder or a read of
// an undeclared observation. That ends the unit with a halt whose reason names
// the cause ("definition error: ..."), and never takes the state's Blocked
// route: no agent or human answer fixes the definition.
func (c *cycle) failErr(in *instance, n *node, err error) error {
	if definitionError(err) {
		return c.halt(in, "definition error: "+err.Error())
	}
	return c.fail(in, n, Blocked, []string{err.Error()}, "")
}

// definitionError reports whether err is a definition bug (see failErr).
func definitionError(err error) bool {
	return errors.Is(err, ErrUnresolved) || errors.Is(err, ErrPromptUnbound) || errors.Is(err, ErrUndeclaredObservation)
}

// unsupported classes a step of a kind the engine does not run yet Blocked.
func (c *cycle) unsupported(in *instance, n *node, kind string) error {
	return c.fail(in, n, Blocked, []string{fmt.Sprintf("%s states are %s by the engine", kind, ErrNotSupported)}, "")
}

// ---------------------------------------------------------------- kinds

// kindOps is the typed half of a kind, bound by its constructor.
type kindOps interface {
	execute(c *cycle, in *instance, n *node) error
}

// submitter is implemented by the ops of a kind that takes submissions.
type submitter interface {
	submit(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error)
	// decide takes a submission already journaled as submitted and journals its
	// decision.
	decide(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error)
}

// replayOf is the submission a journaled submitted record stands for.
func replayOf(in *instance, u *SubmittedData) Submission {
	return Submission{Token: u.Token, Output: u.Output, Tokens: u.Tokens, Child: in.ID}
}

func (b base) execute(c *cycle, in *instance, n *node) error { return c.unsupported(in, n, b.name) }

func execOps(ops kindOps, b base, c *cycle, in *instance, n *node) error {
	if ops == nil {
		return b.execute(c, in, n)
	}
	return ops.execute(c, in, n)
}

func (k *agentKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}
func (k *commandKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}
func (k *computeKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}
func (k *terminalKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}

func (k *agentKind) retryPolicy() *retryData   { return k.retry }
func (k *commandKind) retryPolicy() *retryData { return k.retry }
func (k *computeKind) retryPolicy() *retryData { return k.retry }

// submit lets the typed ops of an agent take the submission.
func (k *agentKind) submit(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error) {
	if sub, ok := k.ops.(submitter); ok {
		return sub.submit(c, in, n, s)
	}
	return SubmitResult{}, fmt.Errorf("state %s takes no submission", n.name)
}

// renderPrompt lets the typed ops of an agent render the prompt at fetch time.
func (k *agentKind) renderPrompt(c *cycle, in *instance, n *node, submit string) (string, map[string]string, error) {
	if p, ok := k.ops.(prompter); ok {
		return p.renderPrompt(c, in, n, submit)
	}
	return "", nil, fmt.Errorf("state %s has no prompt", n.name)
}

// decide lets the typed ops of an agent decide a journaled submission.
func (k *agentKind) decide(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error) {
	if sub, ok := k.ops.(submitter); ok {
		return sub.decide(c, in, n, s)
	}
	return SubmitResult{}, fmt.Errorf("state %s takes no submission", n.name)
}

// ---------------------------------------------------------------- routing

// outOfCases reports an agent output whose routed value no case declares when
// the table has no Otherwise: the refusal text, and true. The agent can fix its
// own output, so the caller classes it Refused and spends the retry budget;
// computes and commands keep route's Blocked, their values being code.
func (n *node) outOfCases(out any) (string, bool) {
	t := n.cases
	if t == nil || t.otherwise != nil {
		return "", false
	}
	v := fmt.Sprint(t.field(out))
	declared := make([]string, len(t.cases))
	for i := range t.cases {
		if t.cases[i].value == v {
			return "", false
		}
		declared[i] = t.cases[i].value
	}
	if v == "" {
		return fmt.Sprintf("routed value is missing; expected one of %v", declared), true
	}
	return fmt.Sprintf("routed value %q is not one of %v", v, declared), true
}

// route chooses the edge for an accepted output or computed value. fn is the
// free-form route (nil when the state routes through a case table); out is the
// value the case table's key reads. ok is false when a failure was journaled
// instead (a Blocked step: off-candidate edge, undeclared read, no route).
func (c *cycle) route(in *instance, n *node, fn func(Ctx) edgeUse, out any, ctx *runCtx) (edgeUse, bool, error) {
	var u edgeUse
	var cands []edgeUse
	switch {
	case fn != nil:
		u = fn(ctx)
		if n.route != nil {
			cands = n.route.cands
		}
	case n.cases != nil:
		t := n.cases
		v := fmt.Sprint(t.field(out))
		var hit *caseData
		for i := range t.cases {
			if t.cases[i].value == v {
				hit = &t.cases[i]
				break
			}
		}
		switch {
		case hit != nil && hit.route != nil:
			u, cands = hit.route(ctx), hit.cands
		case hit != nil:
			u, cands = hit.e, []edgeUse{hit.e}
		case t.otherwise != nil:
			u, cands = *t.otherwise, []edgeUse{*t.otherwise}
		default:
			return u, false, c.fail(in, n, Blocked, []string{fmt.Sprintf("no case for value %q and no Otherwise", v)}, "")
		}
	default:
		return u, false, c.fail(in, n, Blocked, []string{"state declares no route"}, "")
	}
	if ctx.blocked {
		return u, false, c.failErr(in, n, ctx.blockedErr())
	}
	for _, cand := range cands {
		if u.e != nil && cand.e == u.e {
			if u.e.when != nil && !c.optionsMatch(*u.e.when) {
				return u, false, c.fail(in, n, Blocked, []string{fmt.Sprintf("route returned %s, whose guard %s does not hold", describeUse(u), u.e.when)}, "")
			}
			return u, true, nil
		}
	}
	var names []string
	for _, cand := range cands {
		names = append(names, describeUse(cand))
	}
	rd, err := c.decide(in, n, Blocked, []string{"route returned an edge outside its declared candidates: " + describeUse(u)}, "")
	if err != nil {
		return u, false, err
	}
	return u, false, c.emit(in, JournalRouteOffCandidates, Blocked, RouteOffCandidatesData{
		Returned: describeUse(u), Candidates: names, RefusedData: rd,
	})
}

// ---------------------------------------------------------------- guards

// routable reports whether n's Route or Cases table can select e.
func (n *node) routable(e *edgeData) bool {
	if n.route != nil {
		for _, u := range n.route.cands {
			if u.e == e {
				return true
			}
		}
	}
	if t := n.cases; t != nil {
		if t.otherwise != nil && t.otherwise.e == e {
			return true
		}
		for _, cd := range t.cases {
			if cd.e.e == e {
				return true
			}
			for _, u := range cd.cands {
				if u.e == e {
					return true
				}
			}
		}
	}
	return false
}

// takeGuard takes, before n runs, the first route-around guard whose predicate
// holds: an edge guarded by When that no Route or Cases table of n selects. A
// Sub or Map starts no child and an agent is issued no instruction. guarded is
// true when one was taken (or the instance halted on a bad plan).
func (c *cycle) takeGuard(in *instance, n *node) (guarded bool, err error) {
	for _, e := range n.edges {
		if e.when == nil || n.routable(e) || !c.optionsMatch(*e.when) {
			continue
		}
		plan, err := c.planOf(in, edgeUse{e: e, effects: e.effects, optional: e.optional})
		if err != nil {
			return true, c.halt(in, err.Error())
		}
		return true, c.emit(in, JournalAccepted, Accepted, AcceptedData{Computed: true, Plan: plan})
	}
	return false, nil
}

// ---------------------------------------------------------------- observations

// readObs reads a bound descriptor through the adapters. When journal is set
// the read is appended as an observed record.
func (c *cycle) readObs(in *instance, d ObservationDesc, purpose string, journal bool) (ObservedData, error) {
	return c.readObsFor(in, d, purpose, "", journal)
}

// readObsFor is readObs for the input observations of the issue token.
func (c *cycle) readObsFor(in *instance, d ObservationDesc, purpose, issue string, journal bool) (ObservedData, error) {
	od := ObservedData{Desc: d, Purpose: purpose, Issue: issue}
	raw, err := c.e.reg.ReadAt(c.workspace(), EffectContext{Run: c.run, Instance: in.ID, State: c.st.qualified(in), Purpose: purpose, Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, d)
	switch {
	case err == nil:
		if json.Valid(raw) {
			od.Value = raw
		} else {
			od.Value, _ = json.Marshal(string(raw))
		}
	case errors.Is(err, ErrUnavailable):
		od.Unavailable, od.Error = true, err.Error()
	default:
		od.Error = err.Error()
	}
	if journal {
		if err := c.emit(in, JournalObserved, 0, od); err != nil {
			return od, err
		}
	}
	return od, nil
}

// obsDigest digests a set of reads; an unavailable read digests as a marker.
func obsDigest(reads []ObservedData) string {
	h := sha256.New()
	for _, o := range reads {
		switch {
		case o.Unavailable:
			fmt.Fprintf(h, "%s\x00unavailable\n", canonDesc(o.Desc))
		case o.Error != "":
			fmt.Fprintf(h, "%s\x00error\n", canonDesc(o.Desc))
		default:
			fmt.Fprintf(h, "%s\x00%s\n", canonDesc(o.Desc), o.Value)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// bindDescs binds and de-duplicates declared descriptors.
func (c *cycle) bindDescs(in *instance, ds []ObservationDesc) ([]ObservationDesc, error) {
	var out []ObservationDesc
	seen := map[string]bool{}
	for _, d := range ds {
		b, err := c.scope(in).bindDesc(d)
		if err != nil {
			return nil, err
		}
		if k := canonDesc(b); !seen[k] {
			seen[k] = true
			out = append(out, b)
		}
	}
	return out, nil
}

// truthy is the DoneWhen test: true, a non-empty string, array or object, a
// non-zero number.
func truthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return false
}

// doneWhenMet reads the state's DoneWhen observation, journals it and reports
// whether the postcondition holds. An unavailable or failed read is not met.
// ok is false when a failure was journaled instead.
func (c *cycle) doneWhenMet(in *instance, n *node) (met, ok bool, err error) {
	if n.doneWhen == nil {
		return false, true, nil
	}
	d, err := c.scope(in).bindDesc(n.doneWhen.obs)
	if err != nil {
		return false, false, c.failErr(in, n, err)
	}
	od, err := c.readObs(in, d, "done_when", true)
	return err == nil && od.Error == "" && !od.Unavailable && truthy(od.Value), true, err
}

// tokenFor derives an attempt token: unique per run, instance, state, attempt
// and journal position, and deterministic.
func (c *cycle) tokenFor(in *instance) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", c.run, in.ID, in.Cur, in.Attempt, c.nextSeq())))
	return hex.EncodeToString(sum[:])[:16]
}
