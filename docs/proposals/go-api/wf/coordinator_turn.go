package wf

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type coordinatorAttachment struct {
	Harness string
	Session string
	Segment int64
}

// CoordinatorHandoffFrontier identifies a person's question or effect intent.
type CoordinatorHandoffFrontier struct {
	Instance string
	State    string
	Visit    int
	Effect   int64
}

// CoordinatorHandoffData records one response covering every person-owned
// frontier (Receipt zero), its acknowledgement, or invalidation.
type CoordinatorHandoffData struct {
	Admission   int64
	Receipt     int64
	Frontiers   []CoordinatorHandoffFrontier
	WaitingMaps map[string]int64 `json:",omitempty"` // Map instance to arrival sequence
	Invalidated bool             `json:",omitempty"`
}

// CoordinatorContinuationData records an issued hook decision. A continuation
// decision can be overridden by the native harness after it leaves the engine.
type CoordinatorContinuationData struct {
	Admission int64
	Event     string
	Evaluated int64
	Progress  int64
	Reason    string
}

type coordinatorHandoff struct {
	data         CoordinatorHandoffData
	seq          int64
	acknowledged bool
}

type coordinatorTurnState struct {
	attachment coordinatorAttachment
	generation int64
	progress   int64
	count      int
	streak     int
	last       int64
	stall      int64
	handoff    coordinatorHandoff
	events     map[string]CoordinatorContinuationData
}

// TurnEndDecision describes the coordinator's committed turn boundary.
type TurnEndDecision struct {
	Decision   string   `json:"decision"`
	Reason     string   `json:"reason"`
	Seq        int64    `json:"seq"`
	Progress   int64    `json:"progress"`
	Admission  int64    `json:"admission"`
	WorkingDir string   `json:"workingDir,omitempty"`
	Argv       []string `json:"argv,omitempty"`
}

func matchingAttachment(a coordinatorAttachment, co Coordinator) bool {
	return co.Session != "" && a.Session == co.Session && a.Harness == normalizeHarness(co.Harness)
}

func (t *coordinatorTurnState) attach(d coordinatorAttachment, seq int64) {
	t.attachment, t.generation, t.progress = d, seq, seq
	t.streak, t.stall, t.handoff = 0, 0, coordinatorHandoff{}
	t.events = nil
}

func (c *cycle) admissionOf(co Coordinator) int64 {
	if t := c.st.turn; t != nil && matchingAttachment(t.attachment, co) {
		return t.generation
	}
	return 0
}

// The hand-out sequence identifies the attachment that issued an offer,
// including when a child binds an earlier token after a replacement.
func (c *cycle) displacedBy(co Coordinator, owner string, offer, admission int64) bool {
	if admission == 0 {
		return c.st.supersedes(co.Session, owner)
	}
	return co.Session != "" && owner != "" && owner != co.Session &&
		c.knownCoordinator(owner) && offer < admission
}

func (c *cycle) childCoordinatorReplaced(co Coordinator, u *unit) bool {
	owner := u.childOwner()
	if t := c.st.turn; t != nil && t.generation != 0 {
		selected := Coordinator{Harness: t.attachment.Harness, Session: t.attachment.Session}
		return c.displacedBy(selected, owner, u.childOfferSeq(), t.generation)
	}
	return c.st.replaced(owner) || c.st.supersedes(co.Session, owner)
}

// turnAdmission finds attachment metadata without loading a definition.
func turnAdmission(recs []JournalRecord) (record JournalRecord, armed, paused bool) {
	var closed bool
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		switch r.Kind {
		case JournalPaused, JournalBreakpointHalted:
			paused = true
		case JournalResumed, JournalRewound, JournalCoordinatorDetached:
			closed = true
		case JournalCoordinatorAttached:
			if closed {
				return JournalRecord{}, true, paused
			}
			return r, true, paused
		}
	}
	return JournalRecord{}, false, paused
}

// turnFence rejects stale calls before migration, recovery or driver binding.
func turnFence(recs []JournalRecord, co Coordinator) error {
	r, armed, paused := turnAdmission(recs)
	if !armed && co.ExpectedAdmission == 0 {
		return nil
	}
	if r.Seq == 0 {
		return fmt.Errorf("%w: no current coordinator attachment", ErrRejected)
	}
	var a coordinatorAttachment
	if json.Unmarshal(r.Data, &a) != nil {
		return corrupt(r, "invalid coordinator attachment")
	}
	if !matchingAttachment(a, co) || (co.ExpectedAdmission != 0 && co.ExpectedAdmission != r.Seq) {
		return fmt.Errorf("%w: stale coordinator attachment; use an explicit attach-coordinator before next", ErrRejected)
	}
	if co.ExpectedAdmission != 0 && paused {
		return fmt.Errorf("%w: coordinator continuation reached an operator boundary", ErrRejected)
	}
	return nil
}

// AttachCoordinator selects a new generation without driving the workflow.
// Refreshes by the selected session retain their current generation.
func (e *Engine) AttachCoordinator(run RunID, co Coordinator) (generation int64, err error) {
	err = e.withTelemetry(run, func(c *cycle) error {
		if co.Session == "" || c.st.childSessions[co.Session] || c.st.refusedChildSessions[co.Session] || c.st.ended {
			return fmt.Errorf("%w: attach-coordinator requires a named coordinator on a live run", ErrRejected)
		}
		if c.st.paused {
			return fmt.Errorf("%w: resume the run before selecting a coordinator attachment", ErrRejected)
		}
		if err := c.checkTurnDefinition(); err != nil {
			return err
		}
		if live, _ := c.liveCoordinator(co, c.nextSeq()); live != "" {
			return fmt.Errorf("%w: another coordinator is live", ErrRejected)
		}
		if err := c.bindSegment(co); err != nil {
			return err
		}
		if t := c.st.turn; t != nil && t.generation != 0 && matchingAttachment(t.attachment, co) {
			generation = t.generation
			return nil
		}
		if err := c.recordCoordinator(co); err != nil {
			return err
		}
		generation = c.nextSeq()
		return c.emitAs("", "", 0, JournalCoordinatorAttached, 0, coordinatorAttachment{
			Harness: normalizeHarness(co.Harness), Session: co.Session, Segment: c.st.segment().Start,
		})
	})
	return generation, err
}

// DetachCoordinator invalidates only the specified generation of a run. It
// uses the journal directly so a different installed client can switch runs.
// When a pending selection has no generation yet, zero detaches the active
// generation owned by co, if any.
func (e *Engine) DetachCoordinator(run RunID, co Coordinator, generation int64) error {
	return e.detachCoordinator(run, co, generation, false)
}

func (e *Engine) detachCoordinator(run RunID, co Coordinator, generation int64, requireHistory bool) error {
	unlock, err := e.j.Lock(run)
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := e.j.Records(run)
	if err != nil {
		return err
	}
	if requireHistory {
		var known bool
		for _, rec := range recs {
			if rec.Kind != JournalCoordinatorAttached {
				continue
			}
			var a coordinatorAttachment
			if json.Unmarshal(rec.Data, &a) != nil {
				return corrupt(rec, "invalid coordinator attachment")
			}
			if matchingAttachment(a, co) {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%w: the previous journal has no attachment for this native session", ErrRejected)
		}
	}
	r, _, _ := turnAdmission(recs)
	if r.Seq == 0 || (generation != 0 && r.Seq != generation) {
		return nil
	}
	var a coordinatorAttachment
	if json.Unmarshal(r.Data, &a) != nil {
		return corrupt(r, "invalid coordinator attachment")
	}
	if !matchingAttachment(a, co) {
		return nil
	}
	last := recs[len(recs)-1]
	hash := last.DefHash
	if last.Kind == JournalMigrated {
		var migration MigratedData
		if json.Unmarshal(last.Data, &migration) != nil {
			return corrupt(last, "invalid migration")
		}
		hash = migration.ToHash
	}
	return e.j.Append(JournalRecord{Run: run, Kind: JournalCoordinatorDetached,
		At: e.opts.Clock(), DefHash: hash, Data: r.Data})
}

// MayEndTurn reads committed records without the lock or recovery machinery.
func (e *Engine) MayEndTurn(run RunID, co Coordinator) (TurnEndDecision, error) {
	result := TurnEndDecision{Decision: "unavailable", Reason: "journal_unavailable"}
	reader, ok := e.j.(committedReader)
	if !ok {
		return result, errNotLockFree
	}
	recs, err := reader.ReadCommitted(run)
	if err != nil {
		return result, err
	}
	c, err := e.turnCycle(run, recs)
	if err != nil {
		return result, err
	}
	return c.mayEndTurn(co), nil
}

func (e *Engine) turnCycle(run RunID, recs []JournalRecord) (*cycle, error) {
	if len(recs) == 0 {
		return nil, ErrUnknownRun
	}
	def, err := e.defFor(recs[0])
	if err != nil {
		return nil, err
	}
	st, err := foldRun(recs, def)
	if err != nil {
		return nil, err
	}
	c := &cycle{e: e, run: run, recs: recs, st: st}
	if err := c.checkTurnDefinition(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cycle) checkTurnDefinition() error {
	if have, want := c.st.hash, definitionHashAssets(c.st.def, c.selectedAssetRoots(c.st.def)); have != want {
		return fmt.Errorf("%w: turn query requires the run's pinned definition", ErrDefChanged)
	}
	return nil
}

func (c *cycle) mayEndTurn(co Coordinator) TurnEndDecision {
	d := TurnEndDecision{Decision: "unavailable", Reason: "inactive", Seq: c.nextSeq() - 1, WorkingDir: c.workspace()}
	t := c.st.turn
	if t == nil || t.generation == 0 || !matchingAttachment(t.attachment, co) ||
		(co.ExpectedAdmission != 0 && co.ExpectedAdmission != t.generation) {
		return d
	}
	d.Progress, d.Admission, d.WorkingDir = t.progress, t.generation, c.workspace()
	d.Decision = "allow"
	switch {
	case c.st.ended:
		d.Reason = "ended"
	case c.st.paused:
		d.Reason = "operator_boundary"
	case c.st.untilUnit != nil && c.untilUnitStatus().Settled:
		d.Reason = "bounded_fork_settled"
	case t.stall != 0:
		d.Reason = "continuation_stalled"
	case c.turnHandoffCurrent(t.handoff) && t.handoff.acknowledged:
		d.Reason = "person_handoff"
	case c.heldAtInstance(c.st.insts[""]) && c.turnOnlyFrontiers([]*instance{c.st.insts[""]}, nil):
		d.Reason = "run_halted"
	default:
		d.Decision, d.Reason = "continue", "coordinator_work"
	}
	return d
}

// RecordCoordinatorStop linearises allow and continue alike. Event IDs belong
// to one bridge invocation and may be reused to retry that invocation.
func (e *Engine) RecordCoordinatorStop(run RunID, co Coordinator, event, workspace string) (result TurnEndDecision, err error) {
	if event == "" {
		return result, fmt.Errorf("%w: missing coordinator stop invocation ID", ErrRejected)
	}
	err = e.withTelemetry(run, func(c *cycle) error {
		if err := c.checkTurnDefinition(); err != nil {
			return err
		}
		result = c.mayEndTurn(co)
		if workspace != "" && result.WorkingDir != workspace {
			result.Decision, result.Reason = "unavailable", "inactive"
			return nil
		}
		if result.Decision != "continue" {
			return nil
		}
		t := c.st.turn
		if prior, ok := t.events[event]; ok {
			if prior.Progress != t.progress {
				result.Decision, result.Reason = "unavailable", "stale_event"
			}
			return nil
		}
		d := CoordinatorContinuationData{Admission: t.generation, Event: event,
			Evaluated: result.Seq, Progress: t.progress, Reason: result.Reason}
		if t.streak >= 5 {
			result.Decision, result.Reason = "allow", "continuation_stalled"
			d.Reason = result.Reason
			return c.emitAs("", "", 0, JournalCoordinatorStopStalled, 0, d)
		}
		return c.emitAs("", "", 0, JournalCoordinatorContinued, 0, d)
	})
	return result, err
}

func (d *TurnEndDecision) command(bin, stateDir string, run RunID, assets string, named bool) {
	if d.Decision != "continue" {
		return
	}
	fence := []string{"--expected-admission", strconv.FormatInt(d.Admission, 10), "--state-dir", stateDir}
	if assets != "" {
		fence = append(fence, "--assets", assets)
	}
	if named {
		d.Argv = append([]string{bin, "wf", "next"}, append(fence, "--run-id", string(run))...)
	} else {
		d.Argv = append([]string{bin, "next", string(run)}, fence...)
	}
}

func (e *Engine) withCoordinatorNext(run RunID, co Coordinator, f func(*cycle) error) error {
	return e.withRunChecked(run, func(recs []JournalRecord) error {
		return turnFence(recs, co)
	}, func(c *cycle) error {
		c.coordinatorNext = true
		return f(c)
	})
}

// turnProgress checks the prospective mutation and then its narrow state
// after apply. The journal's sequence alone does not establish progress.
func (st *runState) turnProgress(r JournalRecord, data any, in *instance) func() bool {
	switch d := data.(type) {
	case *ChildSpawnedData:
		old := st.insts[d.ID]
		return func() bool { return old == nil && st.insts[d.ID] != nil }
	case *ChildSettledData:
		child := st.insts[d.ID]
		if child != nil && !child.Settled {
			return func() bool { return child.Settled }
		}
	case *RunEndedData:
		if in != nil && in.Phase != phaseDone {
			return func() bool { return in.Phase == phaseDone }
		}
	}
	if in == nil {
		return nil
	}
	switch r.Kind {
	case JournalTransition:
		before := in.arrivalSeq
		return func() bool { return before != in.arrivalSeq }
	case JournalAttemptStarted, JournalPromptFetched, JournalLeaseClaimed:
		if u := in.Unit; u != nil && !u.State.claimed() {
			return func() bool { return u.State.claimed() }
		}
	case JournalAccepted, JournalCompletedByObservation, JournalRefused, JournalRouteOffCandidates,
		JournalPromptOffCandidates, JournalAttemptLost, JournalBudgetStopped, JournalGateAnswered,
		JournalAutoAnswered, JournalRecoveryAnswered, JournalRetried:
		phase, attempt, pending, recovery := in.Phase, in.Attempt, in.Pending, in.RecoveryAnswer
		u := in.Unit
		terminal := u == nil || u.State.terminal()
		return func() bool {
			return phase != in.Phase || attempt != in.Attempt || pending != in.Pending ||
				recovery != in.RecoveryAnswer || (!terminal && u.State.terminal())
		}
	case JournalEffectResult, JournalEffectSettled:
		seq, _, open := openIntent(in)
		if !open {
			return nil
		}
		var intent int64
		switch d := data.(type) {
		case *EffectResultData:
			if d.Outcome == "unavailable" {
				return nil // completion or halt counts on its own recorded disposition
			}
			intent = d.Intent
		case *EffectSettledData:
			intent = d.Intent
		}
		if seq == intent {
			return func() bool {
				next, _, stillOpen := openIntent(in)
				return !stillOpen || next != seq
			}
		}
	case JournalCleanupApplied:
		d := data.(*CleanupAppliedData)
		if _, ok := st.cleanups[d.Deferred]; ok {
			return func() bool { _, still := st.cleanups[d.Deferred]; return !still }
		}
	}
	return nil
}

// turnHandoffCurrent checks only committed facts. Next records any
// non-requery Maps it found waiting on their existing children.
func (c *cycle) turnHandoffCurrent(h coordinatorHandoff) bool {
	data := h.data
	if h.seq == 0 || data.Admission != c.st.turn.generation || len(data.Frontiers) == 0 {
		return false
	}
	frontiers := make([]*instance, 0, len(data.Frontiers))
	for _, boundary := range data.Frontiers {
		in := c.st.insts[boundary.Instance]
		if in == nil || in.Settled || in.Cur != boundary.State {
			return false
		}
		if boundary.Effect != 0 {
			seq, effect, ok := openIntent(in)
			if !ok || seq != boundary.Effect || !c.declaredOutside(in, effect) {
				return false
			}
		} else {
			if in.Phase != phaseReady || in.Visits[boundary.State] != boundary.Visit || c.coordinatorAnswers(in) {
				return false
			}
			n := in.node()
			if n == nil {
				return false
			}
			gate, ok := n.kind.(*gateKind)
			if !ok {
				return false
			}
			if gate.reservation == Reserved && len(data.Frontiers) == 1 {
				return true // next gives a reserved gate the whole response
			}
		}
		frontiers = append(frontiers, in)
	}
	return c.turnOnlyFrontiers(frontiers, data.WaitingMaps)
}

func (c *cycle) turnOnlyFrontiers(frontiers []*instance, waitingMaps map[string]int64) bool {
	waiting := make(map[*instance]bool, len(frontiers))
	for _, in := range frontiers {
		waiting[in] = true
		for child := in; child.ID != ""; {
			parent := c.st.insts[child.Parent]
			if parent == nil || parent.Cur != child.Spawner || parent.Phase != phaseReady ||
				parent.Pending != nil || parent.Visits[parent.Cur] != child.Entry {
				break
			}
			n := parent.node()
			if n == nil {
				break
			}
			switch k := n.kind.(type) {
			case *subKind:
				// A Sub waits for the child from its current entry.
			case *mapKind:
				if arrival, ok := waitingMaps[parent.ID]; k.requery || !ok || arrival != parent.arrivalSeq {
					parent = nil
				}
			default:
				parent = nil
			}
			if parent == nil {
				break
			}
			waiting[parent] = true
			child = parent
		}
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if !waiting[in] && !in.Settled && in.Phase != phaseDone {
			return false
		}
	}
	return true
}

func (e *Engine) turnArmed(run RunID) (bool, error) {
	reader, ok := e.j.(committedReader)
	if !ok {
		return false, errNotLockFree
	}
	recs, err := reader.ReadCommitted(run)
	if err != nil {
		return false, err
	}
	_, armed, _ := turnAdmission(recs)
	return armed, nil
}
