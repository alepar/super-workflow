package wf

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// The recovery offer (loud-failures B2, coordinator-protocol F1) hands a
// failed unit to the coordinator, which chooses how to recover it on
// evidence, before the unit takes its failure route. It has two causes:
//
//   - exhaustion: an agent state spent its budget, stalled or hit the lost
//     cap. The exhaustion halt carries Recovery; restart grants one more
//     attempt (at most RecoveryRestarts per unit in a run) and give-up takes
//     the exhausted or stalled edge. Offered once per state visit.
//   - ended without done: a claimed child's transcript ended with no done or
//     failed. The attempt stays open (recovery_offered); resume continues the
//     same child with a fixer prompt where the harness can, restart loses the
//     attempt and offers the unit again, give-up takes the Blocked route.
//   - a failure with no route: an agent state's attempt failed Blocked (its
//     child's failed --class blocked, a blocked check) on a state with no
//     Blocked route, or failed on a state with no retry policy. The halt
//     carries Recovery and no Exhausted kind (haltOffered), with the cause
//     as the child stated it. Restart queues a fresh attempt that is shown
//     the cause (the same RecoveryRestarts); give-up takes the state's
//     exhausted edge when it declares one, and otherwise halts the instance
//     with the cause for a person, as every such failure did before.
//
// Every answer is journaled as a degraded record of kind recovery.

// recoveryInstruction is in's recovery instruction, when it has an open offer
// the coordinator can answer.
func (c *cycle) recoveryInstruction(in *instance) (Instruction, bool) {
	l := func(verb string, args ...string) string { return c.verbLine(verb, args...) }
	var v RecoveryView
	var note string
	switch {
	case in.Unit != nil && in.Unit.Recovery != nil && in.Unit.State.claimed() && !in.Settled:
		o := in.Unit.Recovery
		v = RecoveryView{Unit: o.Unit, Cause: o.Cause, Transcript: o.Transcript, LastTool: o.LastTool, LastMessage: o.LastMessage,
			Result: o.Result, Resume: o.Resume, Commands: map[string]string{
				RecoveryRestart: l("recover", o.Unit, RecoveryRestart),
				RecoveryGiveUp:  l("recover", o.Unit, RecoveryGiveUp),
			}}
		if o.Resume != "" {
			v.Commands[RecoveryResume] = l("recover", o.Unit, RecoveryResume)
			note = resumeNoteOf(harness.For(in.Unit.Claim.Harness))
		}
	case in.Phase == phaseHalted && in.Recovery && in.RecoveryAnswer == "" && !in.Settled && (in.ID == "" || c.mapUndecided(in)):
		name := c.st.qualified(in)
		if in.Unit != nil && in.Unit.Name != "" {
			name = in.Unit.Name
		}
		v = RecoveryView{Unit: name, Cause: in.Reason, Restarts: RecoveryRestarts - in.Restarts[in.Cur], Commands: map[string]string{
			RecoveryRestart: l("recover", name, RecoveryRestart),
			RecoveryGiveUp:  l("recover", name, RecoveryGiveUp),
		}}
		if in.Exhausted == "" {
			// A failure with no route: what give-up does depends on the state.
			v.GiveUp = "halts the unit with this cause for a person to decide"
			if edge := c.stopEdge(in); edge != "" {
				v.GiveUp = "takes the state's fallback edge (" + edge + ")"
			}
		}
	default:
		return Instruction{}, false
	}
	return Instruction{
		Run: c.run, State: c.st.qualified(in), Kind: InstructionRecovery, Attempt: in.Attempt, Name: v.Unit, Child: in.ID,
		Prompt: recoveryText(v, note), Recovery: &v, Workspace: c.workspace(),
	}, true
}

// resumeNoteOf is the one ResumeNote assertion: a's note, or "".
func resumeNoteOf(a harness.Adapter) string {
	if n, ok := a.(harness.ResumeNote); ok {
		return n.ResumeNote()
	}
	return ""
}

// recoveryText is the recovery instruction as the coordinator reads it.
// resumeNote, the harness's condition on the resume call, is printed right
// after it; "" prints none.
func recoveryText(v RecoveryView, resumeNote string) string {
	s := fmt.Sprintf("Recover unit %s: %s.", v.Unit, v.Cause)
	if v.Transcript != "" {
		s += "\nIts transcript: " + v.Transcript + "."
		if v.LastTool != "" {
			s += "\nIts last tool call: " + v.LastTool
		}
		if v.LastMessage != "" {
			s += "\nIts last message: " + v.LastMessage
		}
		if v.Result != "" {
			s += "\nIts result file: " + v.Result + "."
		}
		s += "\nDecide on this evidence (read the transcript's end if you need more): resume when the child was close to done, restart when its work is not worth keeping, give up when the unit cannot succeed."
	} else if v.GiveUp != "" {
		s += fmt.Sprintf("\nThat is what stopped the unit and what it said it needs; the state has no route for it, so it is yours to decide. Restart starts a fresh attempt that is shown this cause (%d left for this unit in the run); give up %s.", v.Restarts, v.GiveUp)
	} else {
		s += fmt.Sprintf("\nRestart grants one more attempt (%d left for this unit in the run); give up takes the state's failure edge.", v.Restarts)
	}
	if r, ok := v.Commands[RecoveryResume]; ok {
		s += "\nTo resume, make this harness call first:\n  " + v.Resume
		if resumeNote != "" {
			s += "\n" + resumeNote
		}
		s += "\nthen run:\n  " + r
	}
	s += "\nTo restart, run:\n  " + v.Commands[RecoveryRestart]
	s += "\nTo give up, run:\n  " + v.Commands[RecoveryGiveUp]
	return s
}

// offerEnded journals the recovery offer of in's claimed unit, whose child
// ended without done or failed (F1), with the engine's reading of where it
// stopped.
func (c *cycle) offerEnded(in *instance, cause string) error {
	u := in.Unit
	a := harness.For(u.Claim.Harness)
	var tool, msg string
	if t, ok := a.(harness.Telemetry); ok {
		tool, msg = t.Tail(u.Claim.Transcript)
	}
	d := RecoveryOfferedData{Unit: u.Name, Cause: cause, Transcript: u.Claim.Transcript, LastTool: tool, LastMessage: msg}
	if u.Output != "" {
		d.Result = "missing"
		if p, err := c.unitOutputPath(u); err == nil {
			if _, err := os.Stat(p); err == nil {
				d.Result = "written (" + p + ")"
			}
		}
	}
	// A headless child is a separate process its coordinator cannot send
	// a message to: no resume line, restart or give-up only.
	if line := a.ResumeLine(u.Claim.Agent, c.fixerText(u)); line != "" && u.ChildHarness == "" {
		d.Resume = line
	}
	return c.emit(in, JournalRecoveryOffered, 0, d)
}

// fixerText is the message a resumed child is sent: where it stopped and the
// command that prints its finishing rules again.
func (c *cycle) fixerText(u *unit) string {
	token := u.Bound
	if token == "" && len(u.Offered) > 0 {
		token = u.Offered[len(u.Offered)-1]
	}
	return fmt.Sprintf("You stopped before submitting unit %s. Run %s for the finishing rules, then continue from where you left off and finish.",
		u.Name, c.verbLine("prompt", "--claim", token))
}

// unitOutputPath is the absolute path of u's allocated result file.
func (c *cycle) unitOutputPath(u *unit) (string, error) {
	dir := c.resultDir()
	if dir == "" {
		return "", fmt.Errorf("no run directory")
	}
	return filepath.Join(dir, "out", u.Output), nil
}

// Recover implements Executor: the coordinator's answer to the recovery
// offer of the named unit (its current or last attempt's name, or the
// instance's qualified state).
func (e *Engine) Recover(run RunID, unit, choice string) error {
	return e.withRun(run, func(c *cycle) error {
		if err := c.checkSegment("recover", c.e.opts.caller); err != nil {
			return err
		}
		if c.st.ended {
			return fmt.Errorf("%w: run %s already ended", ErrRejected, run)
		}
		for _, id := range c.st.order {
			in := c.st.insts[id]
			if in.Settled {
				continue
			}
			if u := in.Unit; u != nil && u.Recovery != nil && u.State.claimed() && u.Name == unit {
				if err := c.checkUntilUnitMutation(in); err != nil {
					return err
				}
				return c.recoverEnded(in, choice)
			}
			if _, ok := c.recoveryInstruction(in); ok && in.Recovery {
				if (in.Unit != nil && in.Unit.Name == unit) || c.st.qualified(in) == unit {
					if err := c.checkUntilUnitMutation(in); err != nil {
						return err
					}
					return c.recoverExhausted(in, choice)
				}
			}
		}
		return fmt.Errorf("%w: unit %s has no open recovery offer", ErrRejected, unit)
	})
}

// recoverExhausted answers an exhaustion offer: it journals the choice
// (recovery_answered), and applyRecoveryAnswers records it and journals the
// retried that carries it out (restart: one more attempt; give up: the
// exhausted or stalled edge), now or, after a crash, on the next drive.
func (c *cycle) recoverExhausted(in *instance, choice string) error {
	if choice != RecoveryRestart && choice != RecoveryGiveUp {
		return fmt.Errorf("%w: an exhaustion offer is answered restart or give-up, not %q", ErrRejected, choice)
	}
	if choice == RecoveryGiveUp && c.stopEdge(in) == "" && in.Exhausted != "" {
		return fmt.Errorf("%w: state %s has no fallback edge to give up to", ErrRejected, in.Cur)
	}
	name := c.st.qualified(in)
	if in.Unit != nil && in.Unit.Name != "" {
		name = in.Unit.Name
	}
	if err := c.emit(in, JournalRecoveryAnswered, 0, RecoveryAnsweredData{Unit: name, Choice: choice}); err != nil {
		return err
	}
	return c.drive()
}

// applyExhaustedAnswer records and carries out in's journaled exhaustion
// answer: the degraded record unless it is journaled already, then retried.
func (c *cycle) applyExhaustedAnswer(in *instance) error {
	name := c.st.qualified(in)
	if in.Unit != nil && in.Unit.Name != "" {
		name = in.Unit.Name
	}
	rd := RetriedData{Child: in.ID, Reason: in.Reason}
	action := "restart: one more attempt (" + strconv.Itoa(RecoveryRestarts-in.Restarts[in.Cur]-1) + " restarts left)"
	if in.RecoveryAnswer == RecoveryGiveUp && in.Exhausted == "" && c.stopEdge(in) == "" {
		// A failure with no route and no fallback edge: giving up halts the
		// instance with its cause, for a person, as it halted before the
		// offer existed. The halt record carries the answer out.
		if !in.RecoveryRecorded {
			if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: name, Kind: DegradedRecovery, Cause: in.Reason, Action: "give up: halted with its cause"}); err != nil {
				return err
			}
		}
		return c.emit(in, JournalRefused, in.LastFail, RefusedData{Reasons: append(slices.Clone(in.LastBlocked), gaveUpReason), Halt: true})
	}
	if in.RecoveryAnswer == RecoveryGiveUp {
		p, err := c.stopPlan(in)
		if err != nil {
			return err
		}
		rd.Answer, rd.Plan = RetryStop, &p
		action = "give up: " + c.stopEdge(in)
	}
	if !in.RecoveryRecorded {
		if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: name, Kind: DegradedRecovery, Cause: in.Reason, Action: action}); err != nil {
			return err
		}
	}
	return c.emit(in, JournalRetried, 0, rd)
}

// gaveUpReason ends the cause of a halt the coordinator's give-up left.
const gaveUpReason = "the coordinator gave up"

// recoverEnded answers the offer of a child that ended without done: it
// journals the choice (recovery_answered), and applyRecoveryAnswers records
// it and acts on it, now or, after a crash, on the next drive.
func (c *cycle) recoverEnded(in *instance, choice string) error {
	u := in.Unit
	o := u.Recovery
	d := RecoveryAnsweredData{Unit: u.Name, Choice: choice}
	switch choice {
	case RecoveryResume:
		if o.Resume == "" {
			return fmt.Errorf("%w: harness %s cannot resume a child: restart or give-up", ErrRejected, u.Claim.Harness)
		}
		if mt, err := c.e.opts.TranscriptMtime(u.Claim.Transcript); err == nil {
			d.Mtime = mt.UTC().Format(time.RFC3339Nano)
		}
	case RecoveryRestart, RecoveryGiveUp:
	default:
		return fmt.Errorf("%w: a recovery is answered resume, restart or give-up, not %q", ErrRejected, choice)
	}
	if err := c.emit(in, JournalRecoveryAnswered, 0, d); err != nil {
		return err
	}
	return c.drive()
}

// applyRecoveryAnswers records and carries out each journaled ended-child
// answer not yet applied: the degraded record, then the loss (restart) or
// the Blocked failure (give up); resume needs nothing more.
func (c *cycle) applyRecoveryAnswers() error {
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if !in.Settled && in.RecoveryAnswer != "" && in.Phase == phaseHalted && in.Recovery {
			if err := c.applyExhaustedAnswer(in); err != nil {
				return err
			}
			continue
		}
		u := in.Unit
		if in.Settled || u == nil || u.RecoveryAnswer == "" || !u.State.live() {
			continue
		}
		choice, cause := u.RecoveryAnswer, u.RecoveryCause
		action := map[string]string{RecoveryResume: "resume: the same child continues", RecoveryRestart: "restart: offered again", RecoveryGiveUp: "give up: its Blocked route"}[choice]
		if !u.RecoveryRecorded {
			if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: u.Name, Kind: DegradedRecovery, Cause: cause, Action: action}); err != nil {
				return err
			}
		}
		switch choice {
		case RecoveryRestart:
			if err := c.loseUnit(in, LostCauseDied, true); err != nil {
				return err
			}
		case RecoveryGiveUp:
			if err := c.failAs(in, in.node(), Blocked, []string{cause + "; " + gaveUpReason}, "", failGaveUp); err != nil {
				return err
			}
		}
	}
	return nil
}
