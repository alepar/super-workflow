package wf

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Stop journals paused (wf stop): anyone may run it, a human or a
// budget watcher. From then on next offers nothing and returns the stopped
// instruction at once, and the run starts nothing new; children already
// running finish and run done. A stopped or ended run is not stopped again.
func (e *Engine) Stop(run RunID, reason string) error {
	return e.withRun(run, func(c *cycle) error {
		if c.st.ended || c.st.paused {
			return nil
		}
		return c.emitAs("", "", 0, JournalPaused, 0, PausedData{Reason: reason})
	})
}

// Resume clears a stop (wf resume: resumed) and drives the run on. With
// EngineOptions.Adopt (resume --adopt) the run is first carried onto a changed
// definition (E1), and a run that is not stopped is only adopted. Resume is
// the programmatic call; the CLI's resume is ResumeAs.
func (e *Engine) Resume(run RunID) error { return e.resume(run, nil) }

// Operator is who clears a stop (ResumeAs) or settles an effect outside the
// run (SettleAs): the name it gives
// itself and why, and its identity, (Harness, Session), from its own
// environment. A harness caller must show its session; Harness "generic" is
// a caller whose environment shows no harness, with no session: it is never
// a coordinator session, which is always a harness session.
type Operator struct{ Name, Reason, Harness, Session string }

// operatorAct is what an operator does that a coordinator session may not:
// clear a stop, settle an effect outside the run.
type operatorAct int

const (
	clearingAStop operatorAct = iota
	settlingOutside
)

// refusal is why op may not do act on run, or nil. own are the identities
// that may not do it: the run's coordinator sessions. A recorded identity with no harness matches its
// session in any harness.
func (op Operator) refusal(verb string, run RunID, own []operatorIdentity, act ...operatorAct) error {
	outside := len(act) > 0 && act[0] == settlingOutside
	if strings.TrimSpace(op.Name) == "" || strings.TrimSpace(op.Reason) == "" {
		if outside {
			return fmt.Errorf("%w: %s: an effect outside the run is settled by the run's operator, who names itself and why: --operator <name> --reason <why>", ErrRejected, verb)
		}
		return fmt.Errorf("%w: %s: run %s is stopped: clearing a stop names who clears it and why: --operator <name> --reason <why>", ErrRejected, verb, run)
	}
	session := strings.TrimSpace(op.Session)
	if op.Harness == "" || op.Harness != "generic" && session == "" {
		return fmt.Errorf("%w: %s: the operator's harness session is not known from its environment, so it cannot be told from the run's coordinator: run it from your own session", ErrRejected, verb)
	}
	for _, k := range own {
		if session != "" && k.Session == session && (k.Harness == "" || k.Harness == op.Harness) {
			if outside {
				return fmt.Errorf("%w: %s: this session coordinates run %s, and a coordinator does not settle an effect outside the run: report it and leave it unsettled; the run's operator, another session, settles it", ErrRejected, verb, run)
			}
			return fmt.Errorf("%w: %s: this session coordinates run %s, and a coordinator does not clear its own stop: report that the run is stopped and run nothing further; another session clears it as the run's operator", ErrRejected, verb, run)
		}
	}
	return nil
}

// operatorIdentity is one session's identity: its harness and session.
type operatorIdentity struct{ Harness, Session string }

// coordinatorIdentities are the (harness, session) of every coordinator
// record in recs.
func coordinatorIdentities(recs []JournalRecord) []operatorIdentity {
	var out []operatorIdentity
	for _, r := range recs {
		var d CoordinatorData
		if r.Kind == JournalCoordinator && json.Unmarshal(r.Data, &d) == nil && d.Session != "" {
			out = append(out, operatorIdentity{Harness: d.Harness, Session: d.Session})
		}
	}
	return out
}

// ResumeAs is Resume by an operator session (wf resume --operator --reason).
// Clearing a stop takes a named operator and a reason, journaled on the
// resumed record; the one session refused is a coordinator session of the
// run, which never clears its own stop. Nothing asks whether a person is at
// the terminal: any other session, in any harness, may operate the run.
// Adopting a run that is not stopped needs neither.
func (e *Engine) ResumeAs(run RunID, op Operator) error { return e.resume(run, &op) }

func (e *Engine) resume(run RunID, op *Operator) error {
	// The refusals read the raw records before the fold migrates or adopts
	// anything: a refused resume --adopt must leave the stopped run's
	// journal as it was, readable by the binary that stopped it.
	check := func(recs []JournalRecord) error {
		ended, paused, unbound := false, false, false
		for _, r := range recs {
			switch {
			case r.Kind == JournalRunEnded && r.Instance == "":
				ended = true
			case r.Kind == JournalPaused || r.Kind == JournalBreakpointHalted || r.Kind == JournalBudgetStopped && r.Instance == "":
				paused = true
			case r.Kind == JournalResumed:
				paused, unbound = false, true
			case r.Kind == JournalRewound:
				unbound = true
			case r.Kind == JournalHandedOut:
				// A hand-out to a session-less caller binds nothing but
				// puts a unit in the segment: it is past its boundary.
				unbound = false
			case r.Kind == JournalCoordinator:
				// A coordinator record with a session binds the segment; one
				// that cannot be read counts as bound (fail closed).
				var d CoordinatorData
				if json.Unmarshal(r.Data, &d) != nil || d.Session != "" {
					unbound = false
				}
			}
		}
		if ended && e.opts.Adopt {
			return fmt.Errorf("%w: resume: run %s has ended: an ended run is never resumed; rewind it (wf dev rewind --to died) and adopt the fork", ErrRejected, run)
		}
		if len(e.opts.BoundaryModelMap) > 0 && !paused && !unbound {
			// an earlier revision: no change mid-segment, and nothing adopted first. A
			// segment a boundary opened and no coordinator has bound yet is
			// still at its boundary: see below.
			return fmt.Errorf("%w: resume: run %s is not stopped: its --model-map changes only at a segment boundary; stop it (wf stop), then resume it with --model-map", ErrRejected, run)
		}
		if paused && op != nil {
			if err := op.refusal("resume", run, coordinatorIdentities(recs)); err != nil {
				return err
			}
		}
		return nil
	}
	return e.withRunChecked(run, check, func(c *cycle) error {
		if c.st.ended {
			return fmt.Errorf("%w: resume: run %s has ended: an ended run is never resumed", ErrRejected, run)
		}
		modelMap := e.opts.BoundaryModelMap
		if err := checkModelMap(c.st.def, modelMap); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		// At a boundary: the current segment was opened by a resume, rewind or
		// fork and no coordinator has bound it (its first next was refused at
		// bind, say). Re-issuing the verb with a corrected map opens a fresh
		// segment whose map is the corrected one; the abandoned segment holds
		// no units, and nothing was journaled by the refused bind.
		cur := c.st.segment()
		atBoundary := !c.st.paused && len(modelMap) > 0 && cur != nil && cur.Harness == "" && cur.Cause != SegmentStart && len(cur.units) == 0
		if !c.st.paused && !atBoundary && e.opts.Adopt {
			return c.drive()
		}
		if !c.st.paused && !atBoundary {
			return fmt.Errorf("%w: resume: run %s is not stopped", ErrRejected, run)
		}
		rd := ResumedData{ModelMap: modelMap}
		if err := c.resumeBudget(&rd); err != nil {
			return err
		}
		if op != nil && c.st.paused {
			rd.Operator, rd.Reason, rd.Harness, rd.Session = strings.TrimSpace(op.Name), strings.TrimSpace(op.Reason), op.Harness, op.Session
		}
		if err := c.emitAs("", "", 0, JournalResumed, 0, rd); err != nil {
			return err
		}
		return c.drive()
	})
}

// resumeBudget sets the resumed record's budget from resume --budget, and
// refuses a resume that would leave a budget stop with its limit still spent:
// the run would stop again at its next agent instruction.
func (c *cycle) resumeBudget(rd *ResumedData) error {
	b := c.st.budget
	if ch := c.e.opts.ResumeBudget; ch != nil {
		if !c.st.paused {
			return fmt.Errorf("%w: resume: run %s is not stopped: its budget changes only when a resume clears a stop; stop it (wf stop), then resume it with --budget", ErrRejected, c.run)
		}
		b = ch.Over(b)
		rd.Budget = &b
	}
	if c.st.budgetStop == "" {
		return nil
	}
	if why, spent := c.spentUnder(b); spent {
		if rd.Budget == nil {
			return fmt.Errorf("%w: resume: run %s was stopped by its run budget (%s): resume it with --budget raising that limit", ErrRejected, c.run, c.st.budgetStop)
		}
		return fmt.Errorf("%w: resume: run %s: --budget leaves the run budget spent (%s): raise that limit past what the run has spent", ErrRejected, c.run, why)
	}
	return nil
}

// stoppedInstruction is what next returns while the run is stopped.
func (c *cycle) stoppedInstruction() Instruction {
	reason := "The run is stopped (wf stop): start nothing new."
	switch b := c.st.breakpoint; {
	case b != nil:
		reason = "The run is stopped at a " + b.summary() + ". Start nothing new."
	case c.st.budgetStop != "":
		reason = "The run is stopped on its run budget (" + c.st.budgetStop + "), not ended. Start nothing new." +
			" Only the run's operator, another session, raises the budget, with: " + c.budgetResumeLine() + "."
	}
	return Instruction{Run: c.run, Kind: InstructionStopped, Workspace: c.workspace(), Counts: c.countsLine(), Breakpoint: c.st.breakpoint,
		Prompt: reason + " Children already running finish on their own. " +
			"Report that the run is stopped, quoting this line, and end the session; run nothing further. A coordinator does not clear its own stop: the run's operator, another session, does."}
}
