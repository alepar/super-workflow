package wf

import (
	"encoding/json"
	"fmt"
)

// UntilUnitTarget identifies the current agent state of one instance. State is
// the local state name, not the qualified name printed for a child definition.
type UntilUnitTarget struct{ Instance, State string }

// UntilUnitBoundData fixes a single attempt and visit for a fork's lifetime.
// Later commands cannot clear or replace this boundary.
type UntilUnitBoundData struct {
	Target         UntilUnitTarget
	Unit           string
	Attempt, Visit int
	// Answer marks a fanout gate target (BindFanoutTarget): the gate's
	// answer is the fork coordinator's to give. Any other bound gate stays
	// the person's.
	Answer bool `json:",omitempty"`
}

// UntilUnitStatus describes the immutable target and its first decision.
type UntilUnitStatus struct {
	Bound          bool
	Target         UntilUnitTarget
	Unit           string
	Attempt, Visit int
	Settled        bool
	Class          OutcomeClass
	Reason         string
}

// BindUntilUnit restricts all subsequent progression to the target attempt.
// It must be called at an agent's ready/issued frontier with no effects of
// its own pending, no other unit live and no command in flight anywhere.
// Other instances' pending effects that have not started do not refuse it:
// they are outside the boundary and never run under it.
func (e *Engine) BindUntilUnit(run RunID, target UntilUnitTarget) error {
	return e.withRun(run, func(c *cycle) error { return c.bindUntilUnit(target) })
}

// BindUntilUnitName resolves a current or next unit name without parsing it.
func (e *Engine) BindUntilUnitName(run RunID, name string) error {
	return e.bindUntilUnitName(run, name, false)
}

// BindFanoutTarget binds a fanout fork to its target. A gate target's
// answer is then the fork coordinator's to give (UntilUnitBoundData.Answer).
func (e *Engine) BindFanoutTarget(run RunID, t FanoutTarget) error {
	return e.bindUntilUnitName(run, t.Unit, t.Gate)
}

func (e *Engine) bindUntilUnitName(run RunID, name string, answer bool) error {
	return e.bindUntilUnitNameChecked(run, name, answer, nil)
}

func (e *Engine) bindUntilUnitNameChecked(run RunID, name string, answer bool, check func([]JournalRecord) error) error {
	return e.withRunChecked(run, check, func(c *cycle) error {
		if b := c.st.untilUnit; b != nil && b.Unit == name {
			return nil
		}
		for _, id := range c.st.order {
			in := c.st.insts[id]
			if c.st.unitName(in) == name || (in.Unit != nil && in.Unit.Name == name) {
				if _, gate := in.node().kind.(*gateKind); answer && !gate {
					return fmt.Errorf("%w: until-unit: fanout gate target %q is not a gate", ErrRejected, name)
				}
				return c.bindUntilUnitAs(UntilUnitTarget{Instance: id, State: in.Cur}, answer)
			}
		}
		return fmt.Errorf("%w: until-unit: no current or next unit %q", ErrRejected, name)
	})
}

// coordinatorAnswers reports whether in is the fanout gate target this fork
// is bound to: its answer is the coordinator's, not a person's.
func (c *cycle) coordinatorAnswers(in *instance) bool {
	b := c.st.untilUnit
	return b != nil && b.Answer && in != nil && b.Target.Instance == in.ID && b.Target.State == in.Cur
}

func (c *cycle) bindUntilUnit(target UntilUnitTarget) error { return c.bindUntilUnitAs(target, false) }

func (c *cycle) bindUntilUnitAs(target UntilUnitTarget, answer bool) error {
	if b := c.st.untilUnit; b != nil {
		if b.Target == target {
			return nil
		}
		return fmt.Errorf("%w: until-unit boundary is already bound to %s", ErrRejected, b.Unit)
	}
	if err := c.st.untilUnitRefusal(target, false); err != nil {
		return err
	}
	in := c.st.insts[target.Instance]
	name := c.st.unitName(in)
	if in.Unit != nil && in.Unit.Token == in.Token {
		name = in.Unit.Name
	}
	return c.emit(in, JournalUntilUnitBound, 0, UntilUnitBoundData{Target: target, Unit: name, Attempt: in.Attempt, Visit: in.Visits[in.Cur], Answer: answer})
}

// untilUnitRefusal is why target cannot be bound, nil when it can. At a
// fork point (forkPoint) the live units are the fork's orphans, lost before
// its first bind, so they do not refuse it: a fanout plan asks this of an
// offer's checkpoint.
func (st *runState) untilUnitRefusal(target UntilUnitTarget, forkPoint bool) error {
	in := st.insts[target.Instance]
	if st.ended || in == nil || in.Settled || target.State == "" || in.Cur != target.State || (in.Phase != phaseReady && in.Phase != phaseIssued) {
		return fmt.Errorf("%w: until-unit target is not a current ready/issued agent state", ErrRejected)
	}
	switch in.node().kind.(type) {
	case *agentKind:
	case *gateKind:
		// A person gate waits ready for its answer; the answer settles it.
		if in.Phase != phaseReady {
			return fmt.Errorf("%w: until-unit gate target is not waiting for its answer", ErrRejected)
		}
	default:
		return fmt.Errorf("%w: until-unit target is not an agent or a person gate", ErrRejected)
	}
	if in.Pending != nil || in.Cmd != nil {
		return fmt.Errorf("%w: until-unit target has pending transition or command effects", ErrRejected)
	}
	for _, other := range st.insts {
		// A command in flight anywhere keeps writing the world, and under the
		// boundary nothing would take its result. A fork point has none
		// (checkpoint); a direct bind waits for it.
		if other.Cmd != nil && other.Cmd.Result == nil {
			return fmt.Errorf("%w: until-unit: a command is in flight in %s; bind once it has finished", ErrRejected, other.ID)
		}
		if !forkPoint && other.ID != in.ID && other.Unit != nil && other.Unit.State.live() {
			return fmt.Errorf("%w: until-unit has unrelated live unit %s", ErrRejected, other.Unit.Name)
		}
	}
	return nil
}

// UntilUnitStatus reads the bound attempt's verdict without progressing the run.
func (e *Engine) UntilUnitStatus(run RunID) (UntilUnitStatus, error) {
	var result UntilUnitStatus
	err := e.withRun(run, func(c *cycle) error { result = c.untilUnitStatus(); return nil })
	return result, err
}

func (c *cycle) untilUnitStatus() UntilUnitStatus {
	b := c.st.untilUnit
	if b == nil {
		return UntilUnitStatus{}
	}
	result := UntilUnitStatus{Bound: true, Target: b.Target, Unit: b.Unit, Attempt: b.Attempt, Visit: b.Visit}
	for _, rec := range c.recs {
		if rec.Seq <= c.st.untilUnitSeq || rec.Instance != b.Target.Instance || rec.State != b.Target.State || rec.Attempt != b.Attempt {
			continue
		}
		switch rec.Kind {
		case JournalAccepted, JournalCompletedByObservation, JournalGateAnswered, JournalAutoAnswered:
			result.Settled, result.Class = true, Accepted
		case JournalRefused, JournalSubmissionRefused, JournalRouteOffCandidates, JournalPromptOffCandidates, JournalBudgetStopped, JournalAttemptLost, JournalWithdrawn, JournalBackoffScheduled, JournalRecoveryOffered, JournalLeaseExpired:
			result.Settled, result.Class = true, rec.Class
			if result.Class == 0 {
				result.Class = Blocked
			}
		}
		if rec.Kind == JournalHeartbeat {
			var heartbeat HeartbeatData
			if json.Unmarshal(rec.Data, &heartbeat) == nil && heartbeat.Status == "stalled" {
				result.Settled, result.Class = true, Blocked
			}
		}
		if result.Settled {
			result.Reason = fmt.Sprintf("until-unit %s: %s", b.Unit, result.Class)
			return result
		}
	}
	if in := c.st.insts[b.Target.Instance]; in == nil || in.Cur != b.Target.State || in.Phase == phaseTransition || in.Phase == phaseDone || in.halted() || in.Phase == phaseBackoff {
		result.Settled, result.Class = true, Blocked
		result.Reason = fmt.Sprintf("until-unit %s: target ended without an accepted decision", b.Unit)
	}
	return result
}

func (c *cycle) untilUnitAllows(in *instance) bool {
	b := c.st.untilUnit
	return b == nil || (in != nil && in.ID == b.Target.Instance && in.Cur == b.Target.State && !c.untilUnitStatus().Settled)
}

func (c *cycle) untilUnitInstruction() (Instruction, bool) {
	status := c.untilUnitStatus()
	if !status.Settled {
		return Instruction{}, false
	}
	return Instruction{Run: c.run, State: status.Target.State, Child: status.Target.Instance, Name: status.Unit, Kind: InstructionStopped, Cause: status.Reason, Workspace: c.workspace(), Prompt: status.Reason + ". This fork's target attempt is settled. End the session; run no further work.", Counts: c.countsLine()}, true
}

// driveUntilUnit reconciles only the selected child's liveness, then issues or
// decides that agent. Global map requery, child settling and transition effects
// cannot run past the target's first verdict.
func (c *cycle) driveUntilUnit() error {
	if c.st.paused || c.untilUnitStatus().Settled {
		return nil
	}
	if err := c.haltAtBreakpoint(c.st.untilUnit.Target.Instance); err != nil || c.st.breakpoint != nil {
		return err
	}
	for _, reconcile := range []func() error{c.resolveChildren, c.sampleTranscripts, c.loseEndedChildren, c.expireTimers} {
		if err := reconcile(); err != nil {
			return err
		}
		if c.untilUnitStatus().Settled {
			return nil
		}
	}
	b := c.st.untilUnit
	in := c.st.insts[b.Target.Instance]
	if in == nil || in.Cur != b.Target.State || in.Attempt != b.Attempt || in.Visits[in.Cur] != b.Visit {
		return fmt.Errorf("%w: until-unit target frontier changed without a decision", ErrRejected)
	}
	switch in.Phase {
	case phaseReady:
		n := in.node()
		stopped, err := c.budgetGate(in, n)
		if err != nil || stopped {
			return err
		}
		guarded, err := c.takeGuard(in, n)
		if err != nil || guarded {
			return err
		}
		return n.kind.execute(c, in, n)
	case phaseIssued:
		if u := in.Undecided; u != nil && u.Token == in.Token {
			_, err := in.node().kind.(submitter).decide(c, in, in.node(), replayOf(in, u))
			return err
		}
		return nil
	default:
		return fmt.Errorf("%w: until-unit target cannot progress in phase %s", ErrRejected, in.Phase)
	}
}

// checkUntilUnitMutation gates public verbs before their first write. A rejected
// boundary call is not an audit event: it must leave the fork unchanged.
func (c *cycle) checkUntilUnitMutation(in *instance) error {
	if !c.untilUnitAllows(in) {
		return fmt.Errorf("%w: outside immutable until-unit boundary", ErrRejected)
	}
	return nil
}

// checkUntilUnitTelemetry permits late evidence for this exact target attempt,
// including after settlement; it never permits evidence for another unit.
func (c *cycle) checkUntilUnitTelemetry(name string) error {
	if c.st.untilUnit == nil {
		return nil
	}
	ref, ok := c.st.names[name]
	if !ok {
		return fmt.Errorf("%w: unit %s is outside immutable until-unit boundary", ErrRejected, name)
	}
	b := c.st.untilUnit
	if name != b.Unit || ref.Instance != b.Target.Instance || ref.State != b.Target.State || ref.Attempt != b.Attempt {
		return fmt.Errorf("%w: unit %s is outside immutable until-unit boundary", ErrRejected, name)
	}
	return nil
}
