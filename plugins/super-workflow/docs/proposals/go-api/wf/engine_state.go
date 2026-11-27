package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Instance phases.
const (
	phaseReady        = "ready"      // the state's deterministic work is next
	phaseIssued       = "issued"     // an agent instruction is open
	phaseTransition   = "transition" // an edge is chosen; its effects and the transition are next
	phaseDone         = "done"       // the instance reached a terminal
	phaseHalted       = "blocked"    // the instance halted with a blocked status
	phaseBackoff      = "backoff"    // a class retry is waiting for its due time
	phaseBudgetStoped = "budget_stopped"
)

// runState is the pure fold of a run's journal.
//
// Owns: the instances (the run itself, Sub children, Map children) with their
// current state, counters, arrivals, attempts, accepted and computed outputs,
// observations, open instruction, chosen edge and halt status. apply has the
// one switch over JournalKind; a new record kind is one case there.
// Does not: persist anything, read a clock, run a closure, call an adapter or
// decide anything: every decision was journaled by the Engine, so folding the
// same records always reaches the same state.
// Extend by: a new record kind is a payload type in JournalPayloads and one
// case in apply; callers never mutate a runState.
type runState struct {
	// harnessChecks is the latest preflight of each child harness
	// (harness_checked), keyed by harness.
	harnessChecks map[string]HarnessCheckedData
	// checkedSegment is the start of the segment the latest harness_checked
	// was journaled in: a bound segment without one (a crash between its
	// coordinator record and its preflight) checks on its next call.
	checkedSegment int64

	// forked: the run was made by a rewind or a snapshot restore (a
	// rewound record), so no adapter call of it may reach a real remote.
	forked       bool
	cleanups     map[int64]queuedCleanup
	untilUnit    *UntilUnitBoundData
	untilUnitSeq int64
	def          *Def
	hash         string
	started      *RunStartedData
	ended        bool
	outcome      Outcome
	term         string
	paused       bool
	breakpoint   *BreakpointStatus
	turn         *coordinatorTurnState
	insts        map[string]*instance
	order        []string

	// Run budget measures, derived from the journal: instruction_issued
	// records, submitted token counts and run_started's stamp.
	turns, tokens int
	startedAt     time.Time

	// The run-wide unit name index (see unit); derivable, so not in the digest.
	names      map[string]unitRef   // every name ever handed out
	retired    map[string]bool      // names no longer in use
	endedUnits map[string]endedUnit // final state and claim of each retired name
	// acceptedClaims maps the claim that bound a unit to that unit, for each
	// unit whose own done was accepted: a retry of that done with that claim
	// is told it finished.
	acceptedClaims map[string]acceptedClaim
	// failedClaims maps the claim that bound a unit to that unit and the
	// failure, for each unit its child's own failed ended (a record
	// marked ChildFailed): an identical repeat is told it is recorded.
	failedClaims map[string]failedClaim
	unitTokens   map[string]int    // tokens each unit's attempt spent: done --usage, plus usage attached by started --usage
	degraded     []DegradedData    // every degraded record, in journal order
	nameToken    map[string]string // name to the internal token it maps to

	// coordinators is the harness each coordinator session was handed units
	// under (handed_out); "" is a session-less caller.
	coordinators map[string]string
	// coordOrder is the order in which named coordinator sessions first took
	// units: a later session replaced every earlier one.
	coordOrder map[string]int
	// lastCoordinator is the latest named drive recorded in journal order.
	lastCoordinator string
	// childSessions identifies sessions named by the run's own child offers.
	childSessions map[string]bool
	// refusedChildSessions tracks self-caller refusals in the current segment.
	refusedChildSessions map[string]bool
	// forkOrphans are the units live at the run's latest fork point (a
	// rewound record): their children ran in the source run, never in this
	// one, so the fork's cycles lose them as orphans.
	forkOrphans map[string]bool
	// segments are the run's harness segments in journal order:
	// each opens at a boundary (run start, a resume, a rewind or fork) and is
	// bound by its first coordinator call; the last is the current one.
	segments []harnessSegment
	// issuedAt counts the instruction_issued records per instance|state
	//: a replay gives the n-th issue there the recorded n-th's
	// models.
	issuedAt map[string]int
	// shape is the shape of the definition the run is at (run_started, then
	// each migrated that carries one); nil for a journal from before shapes.
	shape map[string]string
	// coordLastAt is when each named coordinator session last took units,
	// and pausedAt when wf stop last stopped the run: a coordinator stopped
	// since its last hand-out is not live.
	coordLastAt map[string]time.Time
	pausedAt    time.Time
	// fetchedCall is, per coordinator session, the latest next call one of
	// whose hand-outs a child fetched: an unfetched unit handed to
	// that session by an earlier call was passed over (unstarted).
	fetchedCall map[string]int64

	// detach is the lowest detach mode any intent launched on: a step-down is
	// sticky for the run.
	detach string
}

// instance is one running copy of a definition: the run itself ("") or a Sub
// or Map child. Its exported fields are its snapshot (digest).
type instance struct {
	// issuedSegment is the start seq of the segment its latest
	// instruction_issued was recorded in (an earlier revision, unitModels).
	issuedSegment int64
	ID            string
	Parent        string `json:",omitempty"`
	Spawner       string `json:",omitempty"` // the parent's Sub or Map state that spawned it
	DefName       string
	def           *Def
	// arrived is when the instance last arrived at Cur: the At of its latest
	// transition, else of the record that created it. It is unexported, so it
	// stays out of the digest. A record kind that moves an instance to a new
	// state sets it in apply.
	arrived time.Time
	// Track the arrival and which arrival has already paused at a breakpoint.
	arrivalSeq, breakpointArrival int64

	Input     json.RawMessage `json:",omitempty"`
	Cur       string
	Phase     string
	Attempt   int
	Spent     int
	FPs       []string `json:",omitempty"`
	Token     string   `json:",omitempty"`
	Issued    *InstructionIssuedData
	Tokens    map[string]tokenInfo
	Pending   *pendingEdge
	LastEdge  EdgeResult `json:",omitzero"` // what the edge that entered Cur did
	Cmd       *cmdIntent // a Command's journaled intent and result, until it is routed
	Counters  map[string]int
	Visits    map[string]int
	Outputs   map[string]json.RawMessage
	Computed  map[string]json.RawMessage
	Docs      []namedDoc
	Observed  map[string]ObservedData
	Artifacts map[string]ArtifactRef
	Children  map[string]json.RawMessage // child results by parent state
	// ChildOutcomes are the outcomes the Sub children ended with, by parent
	// state (ChildOutcome).
	ChildOutcomes map[string]Outcome `json:",omitempty"`
	AnswerInts    map[string]int     `json:",omitempty"`
	Answers       map[string]string
	Lease         *LeaseClaimedData
	Unit          *unit          `json:",omitempty"` // the current attempt's unit; nil until a handed_out
	Lost          map[string]int `json:",omitempty"` // lost counts on this visit by the unit state lost from
	Backoff       *BackoffScheduledData
	Undecided     *SubmittedData  `json:",omitempty"` // a journaled submission of the open token with no decision yet
	Terminal      string          `json:",omitempty"`
	Outcome       Outcome         `json:",omitempty"`
	Result        json.RawMessage `json:",omitempty"`
	Reason        string          `json:",omitempty"`

	// Refusals counts the submission_refused records of the open instruction
	// and RefusalFP is the last one's fingerprint: every new token (an
	// instruction_issued, so every new attempt and every backoff re-issue)
	// starts at 0, and a token closed with its unit still claimed (a backoff, a
	// budget stop) leaves no refusal open. LastRefusal is the reasons of the last
	// validation refusal of a submission on this state visit: kept across
	// reissue, halt and retried, cleared on state entry and acceptance.
	Refusals    int      `json:",omitempty"`
	RefusalFP   string   `json:",omitempty"`
	LastRefusal []string `json:",omitempty"`
	// LastFailure is the reasons of the failure that scheduled the last
	// backoff on this state visit (an unavailable working directory or
	// adapter): the wait text and status show it while the instance waits. A
	// halt shows its own Reason. Cleared on state entry and acceptance.
	LastFailure []string `json:",omitempty"`
	// LastBlocked is the reasons of the halt on this state visit that was
	// offered to the coordinator as a recovery with no exhaustion (a Blocked
	// failure with no route): the attempt a restart queues is shown them.
	// Kept across the coordinator's give-up halt and a person's retry of it;
	// cleared by any other failure, on state entry and on acceptance.
	LastBlocked []string `json:",omitempty"`

	// Exhausted is the kind of the exhaustion halt the instance stands on
	// (RefusedData.Exhausted): set by that halt, cleared by every other halt,
	// by retried (retry and stop) and on state entry. Only a journal written
	// before exhaustion always took the declared route (C3) has one.
	Exhausted string `json:",omitempty"`
	// Recovery is set while the halt the instance stands on is a recovery
	// offer to the coordinator: an exhaustion halt's (B2), or, with no
	// Exhausted kind, a failure that has no route (haltOffered). RecoveryUsed is set once the
	// state visit's offer was answered (reset on state entry); Restarts
	// counts the restarts answered per state in the run (RecoveryRestarts).
	Recovery bool `json:",omitempty"`
	Stales   int  `json:",omitempty"` // free Stale re-issues on this state visit (StaleReissues)
	// RecoveryAnswer is the coordinator's journaled answer to the exhaustion
	// offer, not yet applied; RecoveryRecorded once its degraded record is.
	// Both clear when the retried record lands.
	RecoveryAnswer   string `json:",omitempty"`
	RecoveryRecorded bool   `json:",omitempty"`
	// CapPending is the reason of a lost cap journaled with no refused
	// record after it yet (finishLostCaps); the refused record clears it.
	CapPending   string         `json:",omitempty"`
	RecoveryUsed bool           `json:",omitempty"`
	Restarts     map[string]int `json:",omitempty"`

	// BlockedCause is the {blockedCause} binding: the reasons of the Blocked
	// failure whose OnClass(Blocked) route entered Cur, carried on that edge
	// (pendingEdge.BlockedCause) and set on its transition; every other
	// transition clears it. Reissued is set when a not-applied keyed command
	// was issued again for free on this visit (G4), so the next one is Blocked;
	// reset on state entry.
	BlockedCause string `json:",omitempty"`
	Reissued     bool   `json:",omitempty"`

	// Child instances (Sub entry or Map item) only: Entry is the parent's
	// arrival count at the spawning state, Key the Map item key, Settled that the
	// parent journaled child_settled, Class how the child settled and
	// BudgetStopped that the run budget stopped it. LastFail is the class of the
	// instance's last failed attempt. SourceReads counts a Map's journaled
	// source reads per entry ("<state>#<entry>").
	Entry         int            `json:",omitempty"`
	Key           string         `json:",omitempty"`
	Settled       bool           `json:",omitempty"`
	Cancelled     bool           `json:",omitempty"` // settled by its parent's cancel: outside every join count
	Class         OutcomeClass   `json:",omitempty"`
	BudgetStopped bool           `json:",omitempty"`
	LastFail      OutcomeClass   `json:",omitempty"`
	SourceReads   map[string]int `json:",omitempty"`
}

type tokenInfo struct {
	State   string
	Attempt int
}

type namedDoc struct {
	Name string
	Raw  json.RawMessage
}

// pendingEdge is the folded "in transition" window: the chosen edge, how many
// of its external effects have a result, the intent still open and whether a
// failed effect skips the rest.
type pendingEdge struct {
	Plan       EdgePlan
	Done       int
	Open       int64      // Seq of an effect_intent with no result yet, 0 for none
	OpenEffect EffectData `json:",omitzero"` // the resolved effect of the open intent
	Skip       bool
	Results    []EdgeEffect `json:",omitempty"` // the outcome of each external effect with a result, in order
	// BlockedCause is set when a Blocked failure chose this edge (its
	// OnClass(Blocked) route): the cause the target state binds as
	// {blockedCause}.
	BlockedCause string `json:",omitempty"`
	// Failure is set when a failure decision chose this edge (a class,
	// exhausted or stalled route): a failed effect on it halts (edgeFailed).
	Failure bool `json:",omitempty"`
}

// externals counts the external effects of the chosen plan.
func (p *pendingEdge) externals() int {
	n := 0
	for _, s := range p.Plan.Steps {
		for _, ef := range s.Effects {
			if ef.Kind != "inc" {
				n++
			}
		}
	}
	return n
}

// stopped reports whether the edge's effects stop the transition: the first
// effect that did not succeed (effectFailed), when the step that carries it is
// not Optional. class is Unavailable for an unavailable or uncertain effect,
// else Blocked; why
// names the effect and its error.
func (p *pendingEdge) stopped() (class OutcomeClass, why string, failed bool) {
	var optional []bool // per external effect, in apply order
	for _, s := range p.Plan.Steps {
		for _, ef := range s.Effects {
			if ef.Kind != "inc" {
				optional = append(optional, s.Optional)
			}
		}
	}
	for i, e := range p.Results {
		if !effectFailed(e.Outcome) {
			continue
		}
		if i < len(optional) && optional[i] {
			// The failure is allowed, but it skipped the effects after it:
			// one of a step that is not Optional stops the edge.
			for j := len(p.Results); j < len(optional); j++ {
				if !optional[j] {
					return Blocked, fmt.Sprintf("%s.%s %s: %s (an effect that is not Optional was skipped after it)", e.Adapter, e.Op, e.Outcome, e.Error), true
				}
			}
			return 0, "", false
		}
		// An unavailable effect never ran, and an uncertain one (its call timed
		// out) is retried on the Unavailable schedule too: the retry checks it
		// by its Key before applying again. A refused one (a sandboxed run)
		// would be refused again, so it is Blocked like a failed one.
		class = Blocked
		if e.Outcome == "unavailable" || e.Outcome == "uncertain" {
			class = Unavailable
		}
		return class, fmt.Sprintf("%s.%s %s: %s", e.Adapter, e.Op, e.Outcome, e.Error), true
	}
	return 0, "", false
}

// cmdIntent is a Command's journaled effect_intent for the shell run and, once
// journaled, its effect_result. It lives until the result is routed (accepted)
// or the attempt fails. Result nil means the intent is open.
type cmdIntent struct {
	Seq    int64
	Effect EffectData
	Result *EffectResultData
	// At, Max, Rerun and Mode are the effect_intent's stamp, detached maximum
	// duration, the dead launch it re-runs and its detach mode. Dead counts the
	// dead launches of this step attempt before this one.
	At    time.Time     `json:",omitzero"`
	Max   time.Duration `json:",omitempty"`
	Rerun *EffectRerun  `json:",omitempty"`
	Dead  int           `json:",omitempty"`
	Mode  string        `json:",omitempty"` // the detach mode it launches on
	// Started is the detached runner's effect_started record, if journaled.
	Started *EffectStartedData `json:",omitempty"`
}

func newInstance(id, parent string, d *Def) *instance {
	in := &instance{
		ID: id, Parent: parent, DefName: d.name, def: d, Phase: phaseReady, Attempt: 1,
		Tokens: map[string]tokenInfo{}, Counters: map[string]int{}, Visits: map[string]int{},
		Outputs: map[string]json.RawMessage{}, Computed: map[string]json.RawMessage{},
		Observed: map[string]ObservedData{}, Artifacts: map[string]ArtifactRef{},
		Children: map[string]json.RawMessage{}, Answers: map[string]string{},
	}
	if len(d.nodes) > 0 {
		in.Cur = d.nodes[0].name
		in.Visits[in.Cur] = 1
	}
	return in
}

// find returns the state called name in d.
func (d *Def) find(name string) *node {
	d.seal()
	for _, n := range d.nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

func (in *instance) node() *node { return in.def.find(in.Cur) }

// eval evaluates a budget over the instance's counters.
func (in *instance) eval(b Budget) int {
	total := 0
	for _, t := range b.terms {
		if t.c != nil {
			total += in.Counters[t.c.name]
		} else {
			total += t.k
		}
	}
	return total
}

// foldRun folds records into a runState. def is the run's root definition.
func foldRun(recs []JournalRecord, def *Def) (*runState, error) {
	st := &runState{def: def, insts: map[string]*instance{}}
	st.initIndex()
	for i, r := range recs {
		if i == 0 && r.Kind != JournalRunStarted {
			return nil, fmt.Errorf("%w: first record is %s, not run_started", ErrCorruptJournal, r.Kind)
		}
		if err := st.apply(r); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func corrupt(r JournalRecord, format string, args ...any) error {
	return fmt.Errorf("%w: record %d (%s): %s", ErrCorruptJournal, r.Seq, r.Kind, fmt.Sprintf(format, args...))
}

// decodeData decodes a record's payload through JournalPayloads and returns a
// pointer to the typed value.
func decodeData(r JournalRecord) (any, error) {
	t, ok := JournalPayloads[r.Kind]
	if !ok {
		return nil, corrupt(r, "unknown record kind")
	}
	p := reflect.New(t)
	if len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, p.Interface()); err != nil {
			return nil, corrupt(r, "undecodable payload: %v", err)
		}
	}
	return p.Interface(), nil
}

// apply folds one record. It is the one switch over JournalKind.
func (st *runState) apply(r JournalRecord) error {
	data, err := decodeData(r)
	if err != nil {
		return err
	}
	if r.Kind == JournalRunStarted {
		d := data.(*RunStartedData)
		if d.Harness != "" && normalizeHarness(d.Harness) != d.Harness {
			return corrupt(r, "invalid persisted harness")
		}
		st.started, st.hash, st.startedAt = d, r.DefHash, r.At
		st.shape = d.Shape
		st.segments = []harnessSegment{{Start: r.Seq, Cause: SegmentStart}}
		root := newInstance("", "", st.def)
		root.arrived, root.arrivalSeq = r.At, r.Seq
		st.insts[""], st.order = root, []string{""}
		return nil
	}
	if st.started == nil {
		return corrupt(r, "record before run_started")
	}
	in := st.insts[r.Instance]
	if in == nil && r.Kind != JournalChildSpawned && r.Kind != JournalMigrated && r.Kind != JournalPaused && r.Kind != JournalResumed && r.Kind != JournalNote && r.Kind != JournalCoordinator && r.Kind != JournalRewound {
		return corrupt(r, "unknown instance %q", r.Instance)
	}
	if r.Kind == JournalInstructionIssued {
		if st.issuedAt == nil {
			st.issuedAt = map[string]int{}
		}
		st.issuedAt[replayKey(r.Instance, r.State)]++
		if in := st.insts[r.Instance]; in != nil {
			in.issuedSegment = st.segment().Start
		}
	}
	if st.turn != nil {
		if changed := st.turnProgress(r, data, in); changed != nil {
			defer func() {
				if changed() {
					st.turn.progress, st.turn.streak, st.turn.stall = r.Seq, 0, 0
				}
			}()
		}
	}
	if st.historicalBackoffLiveness(data) {
		return nil
	}
	switch d := data.(type) {
	case *UntilUnitBoundData:
		if st.untilUnit != nil {
			return corrupt(r, "until-unit boundary already bound")
		}
		if d.Target.Instance != r.Instance || d.Target.State != r.State || in.Cur != d.Target.State || d.Attempt != in.Attempt || d.Visit != in.Visits[in.Cur] || d.Unit == "" {
			return corrupt(r, "until-unit binding does not match frontier")
		}
		st.untilUnit, st.untilUnitSeq = d, r.Seq
	case *InstructionIssuedData:
		in.Phase, in.Token, in.Issued = phaseIssued, d.Token, d
		in.Refusals, in.RefusalFP = 0, ""
		in.Tokens[d.Token] = tokenInfo{State: in.Cur, Attempt: in.Attempt}
		st.turns++
	case *SubmittedData:
		st.tokens += d.Tokens
		if d.Tokens > 0 && in.Unit != nil && in.Unit.Name != "" {
			st.addUnitTokens(in.Unit.Name, d.Tokens)
		}
		if d.Token == in.Token && in.Phase == phaseIssued {
			in.Undecided = d
		}
		for name, a := range d.Artifacts {
			in.Artifacts[name] = a
		}
	case *AcceptedData:
		in.LastRefusal, in.LastFailure, in.LastBlocked = nil, nil, nil
		in.accept(r.State, d.Output, d.Computed)
		if u := in.Unit; u != nil && !u.State.terminal() && u.Bound != "" {
			st.initIndex()
			st.acceptedClaims[u.Bound] = acceptedClaim{Name: u.Name}
		}
		in.endUnit(st, UnitDone)
		in.chose(d.Plan)
	case *CompletedByObservationData:
		in.chose(d.Plan)
	case *RejectedData:
		// An audit record: a rejected token changes nothing.
	case *SubmissionRefusedData:
		// The attempt stays open: token, issued instruction and unit are kept.
		if d.Token != in.Token || in.Phase != phaseIssued {
			return corrupt(r, "submission_refused %q does not match the open instruction", d.Token)
		}
		in.Undecided = nil
		in.Refusals++
		in.RefusalFP, in.LastRefusal = d.Fingerprint, d.Reasons
	case *RefusedData:
		in.failed(st, r.Class, *d)
	case *RouteOffCandidatesData:
		in.failed(st, r.Class, d.RefusedData)
	case *PromptOffCandidatesData:
		in.failed(st, r.Class, d.RefusedData)
	case *CleanupDeferredData:
		if st.cleanups == nil {
			st.cleanups = map[int64]queuedCleanup{}
		}
		st.cleanups[r.Seq] = queuedCleanup{Deferred: r.Seq, Instance: r.Instance, Effect: d.Effect}
		if in.Pending == nil {
			return fmt.Errorf("%w: cleanup without pending edge", ErrCorruptJournal)
		}
		in.Pending.Results = append(in.Pending.Results, EdgeEffect{Adapter: d.Effect.Adapter, Op: d.Effect.Op, Outcome: "deferred"})
		in.Pending.Open, in.Pending.OpenEffect = 0, EffectData{}
		in.Pending.Done = d.Index + 1
	case *CleanupAppliedData:
		delete(st.cleanups, d.Deferred)
	case *SessionStoppedData, *SessionClearedData:
		// History only: journals from before the session stop was removed
		// hold these; they change no state.
	case *GateOfferedData:
		// History only: the offer changes no state.
	case *EffectIntentData:
		if in.Pending != nil {
			in.Pending.Open, in.Pending.OpenEffect = r.Seq, d.Effect
		} else {
			next := &cmdIntent{Seq: r.Seq, Effect: d.Effect, At: r.At, Max: d.Max, Rerun: d.Rerun, Mode: d.Mode}
			st.detach = lowerMode(st.detach, d.Mode)
			if prev := in.Cmd; d.Rerun != nil && prev != nil && prev.Seq == d.Rerun.Of && prev.Result == nil {
				next.Dead = prev.Dead + 1
			}
			in.Cmd = next
		}
	case *EffectResultData:
		if in.Pending != nil {
			e := in.Pending.OpenEffect
			in.Pending.Results = append(in.Pending.Results, EdgeEffect{Adapter: e.Adapter, Op: e.Op, Outcome: d.Outcome, Error: d.Error})
			in.Pending.Open, in.Pending.OpenEffect = 0, EffectData{}
			in.Pending.Done = d.Index + 1
			in.Pending.Skip = in.Pending.Skip || !d.OK
		} else if in.Cmd != nil {
			in.Cmd.Result = d
		}
	case *EffectSettledData:
		switch {
		case in.Pending != nil && in.Pending.Open == d.Intent:
			if e := in.Pending.OpenEffect; d.Applied {
				in.Pending.Results = append(in.Pending.Results, EdgeEffect{Adapter: e.Adapter, Op: e.Op, Outcome: "settled"})
			}
			in.Pending.Open, in.Pending.OpenEffect = 0, EffectData{}
			in.Pending.Done++
			if !d.Applied {
				in.settledNotApplied(st, d)
			}
		case in.Cmd != nil && in.Cmd.Seq == d.Intent && in.Cmd.Result == nil:
			if d.Applied {
				// A settled-applied command completed with the settled result, else exit 0.
				res := settledRunResult
				if d.Result != nil {
					res, _ = json.Marshal(*d.Result)
				}
				in.Cmd.Result = &EffectResultData{Intent: d.Intent, OK: true, Outcome: "settled", Result: res}
			} else {
				in.settledNotApplied(st, d)
			}
		}
	case *ObservedData:
		in.Observed[canonDesc(d.Desc)] = *d
		if d.Purpose == purposeMapSource {
			if in.SourceReads == nil {
				in.SourceReads = map[string]int{}
			}
			in.SourceReads[sourceKey(r.State, in.Visits[r.State])]++
		}
	case *AutoAnsweredData:
		in.Answers[r.State] = d.Answer
		delete(in.AnswerInts, r.State)
		if d.Plan != nil {
			in.chose(*d.Plan)
		}
	case *GateAnsweredData:
		in.Answers[r.State] = d.Answer
		delete(in.AnswerInts, r.State)
		if d.IntValue != nil {
			if in.AnswerInts == nil {
				in.AnswerInts = map[string]int{}
			}
			in.AnswerInts[r.State] = *d.IntValue
		}
		if d.Plan != nil {
			in.chose(*d.Plan)
		}
	case *ScopeResetData:
		for _, c := range d.Counters {
			in.Counters[c] = 0
		}
	case *TransitionData:
		in.arrived, in.arrivalSeq = r.At, r.Seq
		in.endUnit(st, UnitDone)
		in.Lost, in.Unit = nil, nil
		in.LastEdge = EdgeResult{}
		in.BlockedCause = ""
		if p := in.Pending; p != nil {
			in.LastEdge.Effects = p.Results
			in.LastEdge.Skipped = p.externals() - len(p.Results)
			in.BlockedCause = p.BlockedCause
		}
		for _, c := range d.Incs {
			in.Counters[c]++
		}
		in.Cur = d.To
		in.Visits[d.To]++
		in.Attempt, in.Spent, in.FPs = 1, 0, nil
		in.Refusals, in.RefusalFP, in.LastRefusal, in.LastFailure = 0, "", nil, nil
		in.LastBlocked = nil
		in.Exhausted, in.Reissued = "", false
		in.Recovery, in.RecoveryUsed, in.Stales = false, false, 0
		in.Token, in.Issued, in.Pending, in.Backoff, in.Undecided = "", nil, nil, nil, nil
		in.Phase = phaseReady
	case *LeaseClaimedData:
		in.Lease = d
		if u := in.Unit; u != nil && u.State == UnitHanded {
			// lease_claimed folds into attempt_started.
			u.Claim, u.StartedAt, u.LeaseExpires, u.State = &unitClaim{Agent: d.Holder}, r.At, d.Expires, UnitStarted
		}
	case *LeaseExpiredData:
		in.Lease = nil
		in.reissue()
	case *BackoffScheduledData:
		if u := in.Unit; u != nil && !u.State.terminal() {
			if d.ChildFailed {
				st.noteFailedClaim(u, d.Class, d.Reasons)
			}
			in.endUnit(st, UnitFailed)
			if ended, ok := st.endedUnits[u.Name]; ok {
				ended.Backoff = true
				st.endedUnits[u.Name] = ended
			}
		}
		if p := in.Pending; p != nil && len(p.Results) > 0 {
			// An unavailable edge effect (B1): the retry re-applies it and the
			// effects after it.
			p.Results = p.Results[:len(p.Results)-1]
			p.Done, p.Skip = len(p.Results), false
		}
		// Each schedule shows its own failure: one that recorded none (a
		// journal from before Reasons) shows none, never an earlier one.
		in.Backoff, in.Phase, in.LastFailure = d, phaseBackoff, d.Reasons
		in.Token, in.Issued, in.Cmd, in.Undecided = "", nil, nil, nil
		in.Refusals, in.RefusalFP = 0, ""
	case *MigratedData:
		st.started.renameBreakpoints(d.States)
		if st.breakpoint != nil {
			st.breakpoint.State = renameBreakpoint(st.breakpoint.State, d.States)
		}
		if d.AssetRoots != nil {
			st.started.AssetRoots = d.AssetRoots
		}
		if d.Assets != "" {
			st.started.Assets = d.Assets
		}
		if d.Assets != "" && st.started.Params["skillsRoot"] != "" {
			st.started.Params["skillsRoot"] = d.Assets
		}
		st.hash = d.ToHash
		// An adoption records the new shape; a declared Migration none, so
		// the shape is unknown from here on and later adoptions compare
		// names only.
		st.shape = d.Shape
		for _, i := range st.insts {
			names := st.renamesFor(i, d.States)
			cur := i.Cur
			if to, ok := names[cur]; ok {
				cur = to
			}
			i.rename(names, cur)
		}
		for _, i := range st.insts {
			if p := st.insts[i.Parent]; p != nil && i.Spawner != "" {
				if to, ok := st.renamesFor(p, d.States)[i.Spawner]; ok {
					i.Spawner = to
					// The child folded at its spawner's old name, which a
					// swap or reuse may give a sibling now: re-resolve it
					// at the new name.
					if ch := p.def.spawnedChild(to, i.DefName); ch != nil {
						i.def = ch
					}
				}
			}
		}
	case *RunEndedData:
		in.Phase, in.Terminal, in.Outcome, in.Result = phaseDone, d.Terminal, d.Outcome, d.Result
		if d.Terminal != "" {
			in.Cur = d.Terminal
		}
		if r.Instance == "" {
			st.ended, st.outcome, st.term = true, d.Outcome, d.Terminal
		}
	case *ChildSpawnedData:
		parent := st.insts[r.Instance]
		if parent == nil {
			return corrupt(r, "child of unknown instance %q", r.Instance)
		}
		child := parent.def.spawnedChild(d.State, d.Def)
		if child == nil {
			return corrupt(r, "unknown child definition %q", d.Def)
		}
		if st.insts[d.ID] != nil {
			// Two instances never share an id: a second spawn would replace
			// the first in place (sw-ivuk).
			return corrupt(r, "child %q is already spawned", d.ID)
		}
		ci := newInstance(d.ID, r.Instance, child)
		ci.Spawner, ci.Entry, ci.Key = d.State, parent.Visits[d.State], d.Key
		ci.Input = d.Input
		ci.arrived, ci.arrivalSeq = r.At, r.Seq
		st.insts[d.ID] = ci
		st.order = append(st.order, d.ID)
	case *ChildSettledData:
		child := st.insts[d.ID]
		if child == nil {
			return corrupt(r, "unknown child %q", d.ID)
		}
		child.Terminal, child.Outcome, child.Result = d.Terminal, d.Outcome, d.Result
		child.Settled, child.Class, child.Cancelled = true, d.Class, d.Cancelled
		child.Token, child.Issued, child.Undecided = "", nil, nil
		// A settled child is inert: no open edge, command intent or backoff
		// survives, and its unit's name is retired so a late done is rejected.
		child.Pending, child.Cmd, child.Backoff = nil, nil, nil
		switch {
		case d.Running:
			// Settled for the join, attempt still running: the unit stays live.
		case d.Cancelled:
			child.endUnit(st, UnitWithdrawn)
		default:
			child.endUnit(st, UnitFailed)
		}
		if !child.BudgetStopped {
			child.Phase = phaseDone
		}
		// A Map's keyed result is its join (accepted, computed); a Sub's child
		// result is the child's.
		if n := in.def.find(d.State); n == nil || !isMapKind(n) {
			in.Children[d.State] = d.Result
			if in.ChildOutcomes == nil {
				in.ChildOutcomes = map[string]Outcome{}
			}
			in.ChildOutcomes[d.State] = d.childOutcome()
			if len(d.Result) > 0 {
				in.Docs = append(in.Docs, namedDoc{Name: d.State, Raw: d.Result})
			}
		}
	case *BudgetStoppedData:
		in.Phase, in.Reason, in.BudgetStopped = phaseBudgetStoped, d.Reason, true
		in.Token, in.Issued, in.Undecided = "", nil, nil
		in.Refusals, in.RefusalFP = 0, ""
	case *RetriedData:
		if in.heldUnit() != nil {
			// A released hold: the held attempt ends failed and the next is queued,
			// with no retry spent.
			in.endUnit(st, UnitFailed)
			in.reissue()
			break
		}
		if !in.halted() {
			return corrupt(r, "retried instance %q is not halted", r.Instance)
		}
		// retried leaves Spent, FPs and Lost as they are: what a retry of an
		// exhaustion halt grants follows from them (one more attempt on a
		// spent budget or lost cap, the remaining budget on a stall).
		in.Reason, in.Settled, in.Class, in.LastFail = "", false, 0, 0
		in.Exhausted = ""
		in.RecoveryAnswer, in.RecoveryRecorded = "", false
		if in.Recovery {
			// The coordinator answered the visit's recovery offer.
			in.Recovery, in.RecoveryUsed = false, true
			if d.Answer != RetryStop {
				if in.Restarts == nil {
					in.Restarts = map[string]int{}
				}
				in.Restarts[in.Cur]++
			}
		}
		if d.Answer == RetryStop {
			if d.Plan == nil {
				return corrupt(r, "retried stop for %q carries no plan", r.Instance)
			}
			in.chose(*d.Plan)
			break
		}
		in.reissue()
	case *coordinatorAttachment:
		if st.turn == nil {
			st.turn = &coordinatorTurnState{}
		}
		if d.Segment != st.segment().Start || d.Session == "" {
			return corrupt(r, "coordinator attachment does not match the current segment")
		}
		if r.Kind == JournalCoordinatorAttached {
			st.turn.attach(*d, r.Seq)
		} else {
			st.turn.generation, st.turn.stall = 0, 0
		}
	case *CoordinatorHandoffData:
		if st.turn == nil || st.turn.generation != d.Admission {
			return corrupt(r, "handoff has no active coordinator attachment")
		}
		if d.Invalidated && st.turn.handoff.seq == d.Receipt {
			st.turn.handoff = coordinatorHandoff{}
		} else if d.Receipt == 0 {
			st.turn.handoff = coordinatorHandoff{data: *d, seq: r.Seq}
		} else if st.turn.handoff.seq == d.Receipt {
			st.turn.handoff.acknowledged = true
		}
	case *CoordinatorContinuationData:
		if st.turn == nil || st.turn.generation != d.Admission {
			return corrupt(r, "continuation has no active coordinator attachment")
		}
		if st.turn.events == nil {
			st.turn.events = map[string]CoordinatorContinuationData{}
		}
		st.turn.events[d.Event] = *d
		if r.Kind == JournalCoordinatorStopStalled {
			st.turn.stall = r.Seq
			break
		}
		st.turn.count++
		st.turn.streak++
		st.turn.last = r.Seq
	case *PausedData:
		st.paused, st.pausedAt = true, r.At
	case *BreakpointHaltedData:
		if st.paused || in.arrivalSeq != d.ArrivalSeq || in.breakpointArrival == d.ArrivalSeq || in.Phase != phaseReady {
			return corrupt(r, "breakpoint does not match a fresh arrival")
		}
		in.breakpointArrival = d.ArrivalSeq
		st.paused, st.pausedAt = true, r.At
		st.breakpoint = &BreakpointStatus{BreakpointHaltedData: *d, Instance: r.Instance, Seq: r.Seq}
	case *ResumedData:
		if st.turn != nil {
			st.turn.generation, st.turn.stall = 0, 0
		}
		st.paused, st.breakpoint = false, nil
		st.openSegment(r.Seq, SegmentResume)
		st.segment().mapGiven = d.ModelMap
	case *CoordinatorData:
		st.noteCoordinator(d.Session)
		if d.Session != "" {
			st.lastCoordinator = d.Session
		}
		if cur := st.segment(); cur != nil && cur.Harness == "" && d.Session != "" && (d.SegmentStart == 0 || d.SegmentStart == cur.Start) {
			// J1: the segment's first coordinator call binds it, harness and
			// records root (a journal from before segments: the first
			// coordinator call after the boundary).
			cur.Harness, cur.Root, cur.Session = normalizeHarness(d.Harness), d.Root, d.Session
			cur.models = d.Models
		}
	case *WorldSnapshotData:
	// Checkpoint metadata does not change workflow state.
	case *RewoundData:
		if st.turn != nil {
			st.turn.generation, st.turn.stall = 0, 0
		}
		// A fork's record of where it came from: the state is the prefix. A
		// fork is a crash-restart: the units live at it are orphans here.
		// Its adapters run sandboxed from here on (EffectContext.Sandboxed).
		st.forked = true
		st.forkOrphans = map[string]bool{}
		// The source run's headless children are its processes, not the
		// fork's: the fork neither watches nor signals them. Children the
		// fork spawns itself are recorded after this record.
		for _, fin := range st.insts {
			if fin != nil && fin.Unit != nil {
				fin.Unit.Headless, fin.Unit.HeadlessAt = nil, time.Time{}
			}
		}
		for _, in := range st.insts {
			if in.Unit != nil && in.Unit.State.live() {
				st.forkOrphans[in.Unit.Name] = true
			}
		}
		// It opens a harness segment: the fork is bound by its own first
		// coordinator call, neither the source's coordinator nor the harness
		// the source was started under.
		cause := SegmentRewind
		if strings.HasPrefix(data.(*RewoundData).State, "snapshot ") {
			cause = SegmentFork
		}
		st.openSegment(r.Seq, cause)
		st.segment().mapGiven = data.(*RewoundData).ModelMap

	case *NoteData:
		// Free text, except a fork's note of its source's segment binding:
		// that segment keeps the models it recorded.
		if cur := st.segment(); d.Models != nil && cur != nil && cur.Start == d.SegmentStart && cur.models == nil {
			cur.models = d.Models
		}
	case *PostDoneMutationData:
		// An audit record: nothing to fold.
	case *HarnessCheckedData:
		if st.harnessChecks == nil {
			st.harnessChecks = map[string]HarnessCheckedData{}
		}
		st.harnessChecks[d.Harness] = *d
		if cur := st.segment(); cur != nil {
			st.checkedSegment = cur.Start
		}
	case *DegradedData:
		st.degraded = append(st.degraded, *d)
		if _, known := st.coordOrder[d.Session]; d.Session != "" && !known && d.Kind == DegradedChildRefused &&
			(d.SelfCaller || strings.HasPrefix(d.Cause, "prompt: "+childSelfCallerReason+" (")) {
			if st.refusedChildSessions == nil {
				st.refusedChildSessions = map[string]bool{}
			}
			st.refusedChildSessions[d.Session] = true
		}
		if cur := st.segment(); d.Kind == DegradedRootChanged && d.Root != "" && cur != nil {
			// The coordinator moved: later callers are checked against the
			// root it is under now, not the one the segment was bound under.
			cur.Root = d.Root
		}
		if in != nil && d.Kind == DegradedUnstarted {
			// The evidence a re-list of the same offer compares with.
			if u := in.Unit; u != nil && u.State == UnitHanded && u.Name == d.Unit {
				u.Unstarted = d.Cause
			}
		}
		if in != nil && d.Kind == DegradedRecovery {
			// The answer is recorded; it stays pending until its action is
			// journaled (the loss, failure or retried), except resume, which
			// has no further action.
			if u := in.Unit; u != nil && u.RecoveryAnswer != "" && u.Name == d.Unit {
				u.RecoveryRecorded = true
				if u.RecoveryAnswer == RecoveryResume {
					u.RecoveryAnswer, u.RecoveryRecorded = "", false
				}
			}
			if in.RecoveryAnswer != "" {
				in.RecoveryRecorded = true
			}
		}
	case *RecoveryOfferedData:
		if in.Unit == nil || in.Unit.Name != d.Unit || !in.Unit.State.claimed() {
			return corrupt(r, "recovery_offered for %q, which is not the instance's claimed unit", d.Unit)
		}
		in.Unit.Recovery = d
	case *RecoveryAnsweredData:
		if in.Phase == phaseHalted && in.Recovery {
			// An exhaustion offer's answer: applied by the next drive
			// (applyRecoveryAnswers) until its retried record lands.
			in.RecoveryAnswer, in.RecoveryRecorded = d.Choice, false
			break
		}
		if in.Unit == nil || in.Unit.Recovery == nil || in.Unit.Name != d.Unit {
			return corrupt(r, "recovery_answered for %q, which has no open recovery offer", d.Unit)
		}
		in.Unit.RecoveryCause = in.Unit.Recovery.Cause
		in.Unit.Recovery, in.Unit.RecoveryAnswer = nil, d.Choice
		if d.Choice == RecoveryResume {
			in.Unit.ResumedAt = d.Mtime
		}
	default:
		if ok, err := st.applyUnit(r, in, data); ok {
			return err
		}
		return corrupt(r, "no fold case for %T", data)
	}
	return nil
}

func (d *Def) child(name string) *Def {
	for _, c := range d.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (in *instance) accept(state string, out json.RawMessage, computed bool) {
	if computed {
		in.Computed[state] = out
	} else {
		in.Outputs[state] = out
	}
	in.Docs = append(in.Docs, namedDoc{Name: state, Raw: out})
}

// chose holds the instance in transition on a chosen edge.
func (in *instance) chose(p EdgePlan) {
	in.Pending, in.Phase, in.Cmd = &pendingEdge{Plan: p}, phaseTransition, nil
	in.Token, in.Issued, in.Undecided = "", nil, nil
}

func (in *instance) halt(st *runState, reason string) {
	in.endUnit(st, UnitFailed)
	in.Phase, in.Reason, in.Cmd, in.Exhausted, in.Recovery = phaseHalted, reason, nil, "", false
	in.Token, in.Issued, in.Undecided = "", nil, nil
}

// halted is whether the instance stopped on a blocked halt: still halted, or
// settled by its parent as Blocked with no terminal (a halted child is settled
// in the same drive that sees it).
func (in *instance) halted() bool {
	return in.Phase == phaseHalted || (in.Settled && in.Terminal == "" && in.Class == Blocked && !in.BudgetStopped)
}

// exhaustionHalt reports whether the instance is halted by an exhaustion halt
// (an agent state's spent budget, stall or lost cap) and waiting for its
// answer: retry or stop.
//
// Owns: the one test read by subOps.execute and mapOps.execute (such a child
// is not settled as Blocked: its spawner waits), Engine.Retry (--stop is
// accepted only here), status (the fix: lines) and drive (an autonomous run's
// missing delegated retry).
// Does not: change halted(), which every other halt reader keeps using.
// Callers never reconstruct it from Phase, Class or RefusedData.
func (in *instance) exhaustionHalt() bool {
	return in.Phase == phaseHalted && !in.Settled && in.Exhausted != ""
}

// offerOpen reports whether the instance stands on a halt offered to the
// coordinator as a recovery and not answered and applied yet: an exhaustion
// halt's offer or a failure with no route (haltOffered). Its spawner waits
// for the answer, as it does on an exhaustion halt.
func (in *instance) offerOpen() bool {
	return in.Phase == phaseHalted && !in.Settled && in.Recovery
}

func (in *instance) reissue() {
	in.Attempt++
	in.Refusals, in.RefusalFP = 0, ""
	in.Phase, in.Token, in.Issued, in.Cmd, in.Undecided = phaseReady, "", nil, nil, nil
	in.Pending = nil // a re-issue after a failed or halted edge starts the state over
}

// settledNotApplied folds the decision a not-applied settlement journaled
// with it: a free re-issue (Reissued), a fallback edge or a halt.
func (in *instance) settledNotApplied(st *runState, d *EffectSettledData) {
	if d.Refused != nil {
		in.failed(st, Blocked, *d.Refused)
		in.Reissued = in.Reissued || d.Reissued
		return
	}
	in.halt(st, "effect settled as not applied")
}

// rename moves the instance to the state called cur after a Migration and
// re-keys what the fold stored under state names; states maps this instance's
// old state names to new, both unqualified (see renamesFor).
func (in *instance) rename(states map[string]string, cur string) {
	name := func(s string) string {
		if to, ok := states[s]; ok {
			return to
		}
		return s
	}
	in.Cur = cur
	for _, m := range []map[string]json.RawMessage{in.Outputs, in.Computed, in.Children} {
		for old, v := range m {
			if n := name(old); n != old {
				delete(m, old)
				m[n] = v
			}
		}
	}
	for old, v := range in.ChildOutcomes {
		if n := name(old); n != old {
			delete(in.ChildOutcomes, old)
			in.ChildOutcomes[n] = v
		}
	}
	for old, v := range in.Visits {
		if n := name(old); n != old {
			delete(in.Visits, old)
			in.Visits[n] += v
		}
	}
	for old, v := range in.AnswerInts {
		if n := name(old); n != old {
			delete(in.AnswerInts, old)
			in.AnswerInts[n] = v
		}
	}
	for old, v := range in.Answers {
		if n := name(old); n != old {
			delete(in.Answers, old)
			in.Answers[n] = v
		}
	}
	if len(in.Restarts) > 0 {
		rs := map[string]int{}
		for k, v := range in.Restarts {
			rs[name(k)] += v
		}
		in.Restarts = rs
	}
	for k, t := range in.Tokens {
		t.State = name(t.State)
		in.Tokens[k] = t
	}
	for i := range in.Docs {
		in.Docs[i].Name = name(in.Docs[i].Name)
	}
	for k, v := range in.SourceReads {
		if state, entry, ok := strings.Cut(k, "#"); ok {
			if n := name(state); n != state {
				delete(in.SourceReads, k)
				in.SourceReads[n+"#"+entry] += v
			}
		}
	}
	if in.Pending != nil {
		for i := range in.Pending.Plan.Steps {
			s := &in.Pending.Plan.Steps[i]
			s.From, s.To = name(s.From), name(s.To)
		}
	}
}

// settledRunResult is the RunResult a command settled as applied completed with.
var settledRunResult = json.RawMessage(`{"Exit":0,"Stdout":"","Stderr":""}`)

// failed folds a failed attempt the Engine already decided: a fallback edge, a
// halt or a re-issue.
func (in *instance) failed(st *runState, class OutcomeClass, d RefusedData) {
	in.LastFail = class
	in.CapPending = ""
	blocked := in.LastBlocked
	in.LastBlocked = nil
	if class == Stale && d.Plan == nil && !d.Halt && !d.Spent {
		in.Stales++
	}
	if u := in.Unit; u != nil && !u.State.terminal() && d.ChildFailed {
		st.noteFailedClaim(u, class, d.Reasons)
	}
	in.endUnit(st, UnitFailed)
	if d.Submitted {
		in.LastRefusal = d.Reasons
	}
	if d.Spent {
		in.Spent++
	}
	if d.Fingerprint != "" || d.Spent {
		in.FPs = append(in.FPs, d.Fingerprint)
	}
	switch {
	case d.Plan != nil:
		in.chose(*d.Plan)
		in.Pending.Failure = true
		if class == Blocked {
			in.Pending.BlockedCause = strings.Join(d.Reasons, "; ")
		}
	case d.Halt:
		reason := class.String()
		if d.Exhausted != "" && class != Blocked {
			reason = "retry exhausted" // a Blocked exhaustion halt (a not-applied effect) keeps "blocked"
		}
		if len(d.Reasons) > 0 {
			reason += ": " + strings.Join(d.Reasons, "; ")
		}
		gaveUp := in.Phase == phaseHalted && in.Recovery && in.RecoveryAnswer == RecoveryGiveUp
		in.halt(st, reason)
		in.Exhausted, in.Recovery = d.Exhausted, d.Recovery
		// A halt that follows an answered offer is the coordinator's give-up
		// of a failure with no edge to take: the answer is carried out.
		in.RecoveryAnswer, in.RecoveryRecorded = "", false
		switch {
		case d.Recovery && d.Exhausted == "":
			in.LastBlocked = d.Reasons
		case gaveUp:
			in.LastBlocked = blocked
		}
	default:
		in.reissue()
	}
}

// digest is the canonical JSON of the whole instance tree, for comparing two
// folds.
func (st *runState) digest() string {
	snap := struct {
		Hash       string
		Ended      bool
		Outcome    Outcome
		Terminal   string
		Paused     bool
		Breakpoint *BreakpointStatus `json:",omitempty"`
		Instances  []*instance
		Cleanups   map[int64]queuedCleanup `json:",omitempty"`
		Until      *UntilUnitBoundData     `json:",omitempty"`
	}{st.hash, st.ended, st.outcome, st.term, st.paused, st.breakpoint, nil, st.cleanups, st.untilUnit}
	for _, id := range st.order {
		snap.Instances = append(snap.Instances, st.insts[id])
	}
	b, err := json.Marshal(snap)
	if err != nil {
		panic("wf: digest: " + err.Error()) // plain data cannot fail to marshal
	}
	return string(b)
}

// qualified is the instruction's state name: the child definition names joined
// with "/", then the state; the root definition is omitted.
func (st *runState) qualified(in *instance) string {
	return strings.Join(append(st.defPath(in), in.Cur), "/")
}

// defPath is the child definition names from the root down to the instance's
// own definition; empty for the root instance.
func (st *runState) defPath(in *instance) []string {
	var parts []string
	for i := in; i != nil && i.ID != ""; i = st.insts[i.Parent] {
		parts = append([]string{i.DefName}, parts...)
	}
	return parts
}

// renamesFor narrows a Migration's qualified old-to-new state names to the
// instance's own definition: the entries whose key sits directly under its
// definition path, as unqualified old name to the target's last segment.
func (st *runState) renamesFor(in *instance, states map[string]string) map[string]string {
	prefix := strings.Join(st.defPath(in), "/")
	if prefix != "" {
		prefix += "/"
	}
	out := map[string]string{}
	for old, to := range states {
		if rest, ok := strings.CutPrefix(old, prefix); ok && rest != "" && !strings.Contains(rest, "/") {
			out[rest] = to[strings.LastIndex(to, "/")+1:]
		}
	}
	return out
}

// supersedes reports whether coordinator session a replaced session b: a
// first called next after b did (coordinator records, or handed_out in a
// journal older than them), or has not called it yet. A session b that never
// called next replaced nothing and is replaced by nothing. An older
// coordinator coming back supersedes nothing.
func (st *runState) supersedes(a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	ib, ok := st.coordOrder[b]
	if !ok {
		return false // never a coordinator (a child's own id, a manual claim, wave): nothing replaced it
	}
	ia, ok := st.coordOrder[a]
	return !ok || ia > ib
}

// replaced reports whether a later coordinator session took units after
// session s did.
func (st *runState) replaced(s string) bool {
	is, ok := st.coordOrder[s]
	if !ok {
		return false
	}
	for _, i := range st.coordOrder {
		if i > is {
			return true
		}
	}
	return false
}

func (st *runState) noteChildSession(session string) {
	if session == "" {
		return
	}
	if st.childSessions == nil {
		st.childSessions = map[string]bool{}
	}
	st.childSessions[session] = true
}

// noteCoordinator gives a named coordinator session its place in coordOrder
// the first time the journal names it.
func (st *runState) noteCoordinator(session string) {
	if session == "" {
		return
	}
	if _, ok := st.coordOrder[session]; ok {
		return
	}
	if st.coordOrder == nil {
		st.coordOrder = map[string]int{}
	}
	st.coordOrder[session] = len(st.coordOrder)
}

// owner is the coordinator session a unit belongs to: the one its latest
// hand-out went to, else the session its claim names.
func (u *unit) owner() string {
	if u.HandSession != "" {
		return u.HandSession
	}
	if u.Claim != nil {
		return u.Claim.Session
	}
	return ""
}

// ownDef is the name of the definition in runs: the root's for the root
// instance, else the instance's own child definition.
func (st *runState) ownDef(in *instance) string {
	if in.ID == "" || in.DefName == "" {
		return st.def.name
	}
	return in.DefName
}

// Segment causes: what opened a harness segment.
const (
	SegmentStart  = "start"
	SegmentResume = "resume"
	SegmentRewind = "rewind"
	SegmentFork   = "fork"
)

// harnessSegment is one stretch of a run driven from one harness.
// It opens at a boundary record and is bound by its first coordinator call;
// within it every coordinator call must come from that harness, and every
// child verb from the harness of its unit's hand-out.
type harnessSegment struct {
	Start                  int64  // the seq of the boundary record that opened it
	Cause                  string // SegmentStart, SegmentResume, SegmentRewind or SegmentFork
	Harness, Root, Session string // its binding; Harness "" while unbound. Root follows a coordinator's move (DegradedRootChanged)
	units                  map[string]bool
	degradedStart          int // how many degraded records precede this segment
	// mapGiven is the --model-map its boundary record gave, nil
	// when none was given; models is what its binding record journaled,
	// nil while unbound or for a binding from before per-segment maps.
	mapGiven map[string]string
	models   *SegmentModels
}

// segment is the current harness segment; nil before run_started.
func (st *runState) segment() *harnessSegment {
	if len(st.segments) == 0 {
		return nil
	}
	return &st.segments[len(st.segments)-1]
}

// openSegment starts a new, unbound segment at the boundary record seq.
func (st *runState) openSegment(seq int64, cause string) {
	st.refusedChildSessions = nil
	st.segments = append(st.segments, harnessSegment{Start: seq, Cause: cause, degradedStart: len(st.degraded)})
}

// work is the run definition's Describe phrase, "" when it has none.
func (st *runState) work() string {
	if st.def == nil {
		return ""
	}
	return st.def.work
}

// statusReason is the instance's reason in status: its own (a halt's), else
// while it waits on a backoff the failure that scheduled it.
func (in *instance) statusReason() string {
	if in.Reason == "" && in.Phase == phaseBackoff && len(in.LastFailure) > 0 {
		return strings.Join(in.LastFailure, "; ")
	}
	return in.Reason
}

// spawnedChild is the child definition a child_spawned record of d names:
// the one its spawning state runs, so siblings sharing a name (identical in
// shape, perhaps not in closures) each fold to their own. By name when that
// state is gone or runs another definition now: a journal of an older shape.
func (d *Def) spawnedChild(state, name string) *Def {
	if n := d.find(state); n != nil {
		if k, ok := n.kind.(interface{ childDef() *Def }); ok {
			if ch := k.childDef(); ch != nil && ch.name == name {
				return ch
			}
		}
	}
	return d.child(name)
}
