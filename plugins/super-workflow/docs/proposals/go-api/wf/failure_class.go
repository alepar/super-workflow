package wf

import (
	"fmt"
	"strconv"
	"strings"
)

// FailureClass says what an accepted `wf failed` does with the attempt.
//
// Owns: the one decision of what an accepted failure does: FailTransient spends
// a retry and queues the next attempt (or takes the state's failure route);
// FailBlocked takes the state's Blocked route. FailApproval holds the unit
// until its reported action is cleared, without spending a retry or routing.
// Does not: classify an outcome (OutcomeClass stays closed and unchanged; this is
// a separate input to the failure path, not a new outcome class), infer a class
// from the reason text, or grow per definition.
// Extend by: a new class is one value here plus one case in cycle.failToken, the
// only place a FailureClass is matched; the CLI and the prompts pass a class and
// never a route.
// Role: input of Executor.FailedAs; parsed by `wf failed --class`.
type FailureClass string

// Failure classes.
const (
	FailTransient FailureClass = "transient" // the default
	FailBlocked   FailureClass = "blocked"
	FailApproval  FailureClass = "approval"
)

// ParseFailureClass reads a --class value; "" is FailTransient and an unknown
// value is an error.
func ParseFailureClass(s string) (FailureClass, error) {
	switch FailureClass(s) {
	case "", FailTransient:
		return FailTransient, nil
	case FailBlocked, FailApproval:
		return FailureClass(s), nil
	}
	return "", fmt.Errorf("unknown failure class %q (want transient, blocked or approval)", s)
}

// Disposition is what an accepted failure did with the attempt.
type Disposition string

// Dispositions of an accepted failure.
const (
	DispositionQueued   Disposition = "queued"   // the next attempt is queued
	DispositionHeld     Disposition = "held"     // the unit is held for a human
	DispositionRouted   Disposition = "routed"   // the state took its failure route: retries spent, or its Blocked route
	DispositionHalted   Disposition = "halted"   // the instance halted
	DispositionRecovery Disposition = "recovery" // the spent budget is offered to the coordinator (wf recover)
)

// FailureOutcome is what an accepted failure did, read from the folded journal
// after the failure: the CLI prints Message and exits 0, composing nothing.
type FailureOutcome struct {
	Disposition Disposition
	Message     string // "attempt N queued", "held: needs a human: <cause>", ...
	Escalation  string `json:",omitempty"` // "needs-your-decision" when an autonomous run parked the cause
}

// UnitHeldData is the payload of unit_held. ApprovalAction is the exact
// action the child reported, absent on legacy blocked holds.
type UnitHeldData struct {
	Name           string `json:",omitempty"`
	Cause          string
	Parked         bool   `json:",omitempty"`
	ApprovalAction string `json:",omitempty"`
	approvalHold   int64  // journal sequence of the approval report, set by the fold
}

// HeldUnit is a held unit in RunStatus. Child is the instance holding it (""
// is the run itself): `wf retry --child <Child>` releases it.
type HeldUnit struct {
	Child          string `json:",omitempty"`
	Name           string
	Cause          string
	Parked         bool   `json:",omitempty"`
	ApprovalAction string `json:",omitempty"`
	ApprovalHold   int64  `json:",omitempty"`
}

// escalationNeedsDecision labels a parked blocked failure.
const escalationNeedsDecision = "needs-your-decision"

const approvalNeeded = "needs approval"

func (c *cycle) holdApproval(in *instance, action string) (SubmitResult, error) {
	if err := c.emit(in, JournalUnitHeld, 0, UnitHeldData{Name: in.Unit.Name, Cause: approvalNeeded, ApprovalAction: action}); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Class: Accepted, Next: in.Cur, Failure: &FailureOutcome{
		Disposition: DispositionHeld, Message: "needs approval. The unit is held. Report the exact action to your coordinator and end your child task.",
	}}, nil
}

// heldUnit is the instance's unit while a blocked failure holds it, else nil.
func (in *instance) heldUnit() *UnitHeldData {
	if in.Unit != nil && in.Unit.State == UnitHeld {
		return in.Unit.Held
	}
	return nil
}

// heldUnits lists the held units of unsettled instances in instance order.
func (c *cycle) heldUnits() []HeldUnit {
	var out []HeldUnit
	for _, id := range c.st.order {
		if in := c.st.insts[id]; !in.Settled {
			if h := in.heldUnit(); h != nil {
				out = append(out, HeldUnit{Child: id, Name: in.Unit.Name, Cause: h.Cause, Parked: h.Parked,
					ApprovalAction: h.ApprovalAction, ApprovalHold: h.approvalHold})
			}
		}
	}
	return out
}

// releaseHeld is Retry on an instance whose unit a blocked failure holds: it
// journals retried, which ends the held attempt failed and queues the next one
// with no retry spent, then drives. A run that already ended is rejected.
func (c *cycle) releaseHeld(in *instance, h *UnitHeldData) error {
	if c.st.ended {
		return fmt.Errorf("%w: run %s already ended", ErrRejected, c.run)
	}
	if err := c.emit(in, JournalRetried, 0, RetriedData{Child: in.ID, Reason: "held: " + h.Cause}); err != nil {
		return err
	}
	return c.drive()
}

// waitingOnHolds reports whether the run is waiting on a human because of its
// holds alone: it has not ended or paused, at least one unit is held, and
// nothing else can move: no unit to hand out or live, no gate, wait, backoff or
// verification to act on and no halt waiting for a Retry. Such a run is
// RunBlocked, and next answers it with one waiting instruction.
func (c *cycle) waitingOnHolds() bool {
	if c.st.ended || c.st.paused || len(c.heldUnits()) == 0 || len(c.pendingUnits()) > 0 {
		return false
	}
	if _, halted := c.heldAt(); halted {
		return false
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in.Settled {
			continue
		}
		if in.Unit != nil && in.Unit.State.live() {
			return false
		}
		if _, ok := c.nonAgentInstruction(in); ok {
			return false
		}
	}
	return true
}

// holdFix is the command that releases one held unit once its cause is
// cleared: the command alone, as printed.
func (c *cycle) holdFix(h HeldUnit) string {
	var args []string
	if h.Child != "" {
		args = append(args, "--child", h.Child)
	}
	if h.ApprovalHold != 0 {
		args = append(args, "--approval-hold", strconv.FormatInt(h.ApprovalHold, 10))
	}
	return c.verbLine("retry", args...)
}

// HoldView is one held unit in a waiting instruction: the instance holding it
// (absent for the run itself), the unit, its qualified state, the cause and
// the fix that releases it once the cause is cleared.
type HoldView struct {
	Child          string `json:"child,omitempty"`
	Name           string `json:"name"`
	State          string `json:"state"`
	Cause          string `json:"cause"`
	Fix            string `json:"fix"`
	ApprovalAction string `json:"approvalAction,omitempty"`
}

// waitingInstruction is the one instruction of a run waiting on its holds
// (waitingOnHolds): it lists each held unit with its cause and the fix that
// releases it. The coordinator reports it and ends its session; the run
// continues once the human has cleared each cause and released the unit.
func (c *cycle) waitingInstruction() Instruction {
	var b strings.Builder
	var holds []HoldView
	approval := false
	for _, h := range c.heldUnits() {
		v := HoldView{Child: h.Child, Name: h.Name, Cause: h.Cause, Fix: c.holdFix(h), ApprovalAction: h.ApprovalAction}
		if in := c.st.insts[h.Child]; in != nil {
			v.State = c.st.qualified(in)
		}
		holds = append(holds, v)
		approval = approval || v.ApprovalAction != ""
	}
	if approval {
		b.WriteString("The run is waiting on held units. Clear a blocker or report it and end your turn.")
	} else {
		b.WriteString("The run is waiting on a human: every remaining unit is held. Report each cause and end the session; nothing moves until a held unit is released.")
	}
	for _, v := range holds {
		fmt.Fprintf(&b, "\nheld: %s (%s): %s", v.Name, v.State, v.Cause)
		if v.ApprovalAction != "" {
			fmt.Fprintf(&b, "\n  Reported action: %q. Review it and use your harness's normal approval prompt, or leave it parked. Once cleared, run:\n  %s", v.ApprovalAction, v.Fix)
		} else {
			fmt.Fprintf(&b, "\n  clear the cause, then run:\n  %s", v.Fix)
		}
	}
	return Instruction{Run: c.run, Kind: InstructionWaiting, Prompt: b.String(), Holds: holds, Workspace: c.workspace()}
}
