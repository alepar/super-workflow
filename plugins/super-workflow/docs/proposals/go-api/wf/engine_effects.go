package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// applyExternals applies the chosen edge's external effects in order: bind,
// effect_intent, Adapters.Apply, effect_result. A failed effect skips the
// edge's remaining external effects; transit then stops the edge (edgeFailed)
// unless it is Optional. An intent with no result (a crash between intent and
// result) is recovered first: checked by Key and not re-applied when done,
// re-applied when not done, Uncertain otherwise. waiting is true when an
// Uncertain effect holds the instance for a settlement, or when the instance
// took a fallback edge or halted instead. A Check error is transient: the
// effect stays open, nothing is journaled, and the next drive checks again.
func (c *cycle) applyExternals(in *instance, ex []EffectData) (waiting bool, err error) {
	p := in.Pending
	for idx := p.Done; idx < len(ex) && !p.Skip; idx = p.Done {
		if p.Open != 0 {
			if resolved, err := c.recoverEdgeIntent(in, p.Open, idx, p.OpenEffect); err != nil || !resolved {
				return true, err
			}
			if in.Pending != p {
				return true, nil
			}
			continue
		}
		bound, err := c.scope(in).bindEffect(ex[idx])
		if err == nil && bound.Args != nil {
			err = c.checkForkValue(bound.Args, "effect "+bound.Adapter+"."+bound.Op)
		}
		if err != nil {
			return true, c.halt(in, err.Error())
		}
		blocked, err := c.worktreeRemovalBlocked(bound)
		if err != nil {
			return true, err
		}
		if blocked {
			if err := c.emit(in, JournalCleanupDeferred, 0, CleanupDeferredData{Index: idx, Effect: bound}); err != nil {
				return true, err
			}
			continue
		}
		if err := c.emit(in, JournalEffectIntent, 0, EffectIntentData{Index: idx, Effect: bound}); err != nil {
			return true, err
		}
		if err := c.applyEdgeEffect(in, c.recs[len(c.recs)-1].Seq, idx, bound); err != nil {
			return true, err
		}
	}
	return false, nil
}

// applyEffect performs the effect of the intent with Seq intent and journals
// its effect_result. Unavailable and failed are recorded as such; neither is an
// error of the cycle.
func (c *cycle) applyEffect(in *instance, intent int64, idx int, e EffectData) error {
	return c.applyAs(in, intent, idx, e, false)
}

// maxExitError bounds the stderr carried into a failed shell edge effect's error.
const maxExitError = 2000

// applyEdgeEffect is applyEffect for an effect on an edge. A shell command that
// exits non-zero is a failed effect_result there (its Result is kept), because
// nothing on an edge reads an exit code: exit-as-data stays for Command states.
func (c *cycle) applyEdgeEffect(in *instance, intent int64, idx int, e EffectData) error {
	return c.applyAs(in, intent, idx, e, true)
}

// cutResult cuts rr's streams to their tails, writing the full output to
// <run dir>/effects/<intent>[.<idx>].out; ok is false when nothing was cut.
// Observations (shell.Observe) are never cut: their output is data a caller
// parses, not a receipt.
func (c *cycle) cutResult(rr RunResult, intent int64, idx int) (RunResult, bool) {
	out, cutOut := CutStream(rr.Stdout)
	errs, cutErr := CutStream(rr.Stderr)
	dir := c.resultDir()
	if (!cutOut && !cutErr) || dir == "" {
		// Without a run dir there is nowhere to keep the full copy: the journal
		// keeps it whole rather than lose it.
		return rr, false
	}
	name := strconv.FormatInt(intent, 10)
	if idx > 0 {
		name += "." + strconv.Itoa(idx)
	}
	p := filepath.Join(dir, "effects", name+".out")
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil || os.WriteFile(p, FullOutput(rr.Stdout, rr.Stderr), 0o644) != nil {
		return rr, false
	}
	rr.Output, rr.Stdout, rr.Stderr = p, out, errs
	return rr, true
}

func (c *cycle) applyAs(in *instance, intent int64, idx int, e EffectData, edge bool) error {
	args, err := c.resolvePathValues(e.Args)
	if err != nil {
		return err
	}
	res, aerr := c.e.reg.ApplyAt(c.workspace(), EffectContext{Run: c.run, Instance: in.ID, State: c.st.qualified(in), Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, ExternalEffect{Adapter: e.Adapter, Op: e.Op, Args: args.(map[string]any), Key: keyOfData(e)})
	rd := EffectResultData{Intent: intent, Index: idx, OK: aerr == nil, Outcome: "applied"}
	switch {
	case errors.Is(aerr, ErrUnavailable):
		rd.Outcome, rd.Error = "unavailable", aerr.Error()
	case aerr != nil:
		rd.Outcome, rd.Error = "failed", aerr.Error()
		// A failed Apply may still report (a conflicting merge's
		// post_done_mutation): keep it on the effect_result too.
		if len(res) > 0 && json.Valid(res) {
			rd.Result = res
		}
	case len(res) > 0:
		if json.Valid(res) {
			rd.Result = res
		} else {
			rd.Result, _ = json.Marshal(string(res))
		}
		var rr RunResult
		if e.Adapter == "shell" && e.Op == "run" && json.Unmarshal(rd.Result, &rr) == nil {
			// An inline command's streams are cut as the detached runner cuts
			// them, its full output kept beside the effect's other files.
			if cut, ok := c.cutResult(rr, intent, idx); ok {
				rr = cut
				rd.Result, _ = json.Marshal(rr)
			}
		}
		if edge && e.Adapter == "shell" && json.Unmarshal(rd.Result, &rr) == nil && rr.Exit != 0 {
			rd.OK, rd.Outcome = false, "failed"
			rd.Error = fmt.Sprintf("exit %d: %s", rr.Exit, tail(strings.TrimSpace(rr.Stderr), maxExitError))
		}
	}
	if err := c.emit(in, JournalEffectResult, 0, rd); err != nil {
		return err
	}
	if pm := postDoneMutationOf(res); pm != nil {
		pm.Unit = in.Cur
		if p := unitPath(in.ID); p != "" {
			pm.Unit = p + "/" + in.Cur
		}
		return c.emit(in, JournalPostDoneMutation, 0, *pm)
	}
	return nil
}

// postDoneMutationOf is the post_done_mutation an Apply result reports, if
// any (see PostDoneMutationReport).
func postDoneMutationOf(res json.RawMessage) *PostDoneMutationData {
	var r PostDoneMutationReport
	if len(res) == 0 || res[0] != '{' || json.Unmarshal(res, &r) != nil {
		return nil
	}
	return r.PostDoneMutation
}

// recoverEdgeIntent settles the edge effect whose intent has no result. A
// NonIdempotent effect is Uncertain with no check; a keyed one is checked: done
// journals a "done" result, not done is applied, unknown is Uncertain, and a
// check error is transient (the effect stays open, unjournaled, and is checked
// again by the next drive). resolved is false when the effect is Uncertain (held, or the state
// took its OnClass(Uncertain) route).
func (c *cycle) recoverEdgeIntent(in *instance, intent int64, idx int, e EffectData) (resolved bool, err error) {
	if e.NonIdempotent {
		return false, c.uncertain(in, fmt.Sprintf("effect %d (%s.%s) is not idempotent and has an intent with no result", intent, e.Adapter, e.Op))
	}
	args, resolveErr := c.resolvePathValues(e.Args)
	if resolveErr != nil {
		return false, resolveErr
	}
	st, cerr := c.e.reg.CheckAt(c.workspace(), EffectContext{Run: c.run, Instance: in.ID, State: c.st.qualified(in), Sandboxed: c.st.forked, SandboxBin: c.sandboxBin()}, ExternalEffect{Adapter: e.Adapter, Op: e.Op, Args: args.(map[string]any), Key: keyOfData(e)})
	if cerr != nil {
		c.markTransient(in, cerr)
		return false, nil
	}
	switch {
	case cerr == nil && st == EffectDone:
		return true, c.emit(in, JournalEffectResult, 0, EffectResultData{Intent: intent, Index: idx, OK: true, Outcome: "done"})
	case cerr == nil && st == EffectNotDone:
		blocked, err := c.worktreeRemovalBlocked(e)
		if err != nil {
			return false, err
		}
		if blocked {
			return true, c.emit(in, JournalCleanupDeferred, 0, CleanupDeferredData{Index: idx, Intent: intent, Effect: e})
		}
		return true, c.applyEdgeEffect(in, intent, idx, e)
	}
	why := fmt.Sprintf("effect %d (%s.%s) has an intent with no result and its check answered %s", intent, e.Adapter, e.Op, EffectUnknown)
	return false, c.uncertain(in, why)
}

// uncertain classes the open effect Uncertain. It is never re-applied: the
// state takes its own or definition-default Uncertain route when present; otherwise
// nothing is journaled and Next yields a manual-verification instruction until
// Settle.
func (c *cycle) uncertain(in *instance, why string) error {
	n := in.node()
	if _, ok := classRoute(n, Uncertain); ok {
		return c.fail(in, n, Uncertain, []string{why}, "")
	}
	return nil
}

func keyOfData(e EffectData) Key {
	switch {
	case e.NonIdempotent:
		return NonIdempotent(e.Reason)
	case e.Key != "":
		return KeyOf(e.Key)
	}
	return nil
}

// openIntent is the instance's effect intent that has no result, if any.
func openIntent(in *instance) (seq int64, e EffectData, ok bool) {
	switch {
	case in.Pending != nil && in.Pending.Open != 0:
		return in.Pending.Open, in.Pending.OpenEffect, true
	case in.Cmd != nil && in.Cmd.Result == nil:
		return in.Cmd.Seq, in.Cmd.Effect, true
	}
	return 0, EffectData{}, false
}

// verification is the manual-verification instruction for the instance's open
// Uncertain effect. It is the coordinator's to settle: it checks the world and
// settles with what it checked as the evidence. Only an effect the
// definition marks outside the run (Outside: a push, a merge to base) is a
// person's (Person).
func (c *cycle) verification(in *instance, seq int64, e EffectData) Instruction {
	args, _ := json.Marshal(e.Args)
	key := e.Key
	if e.NonIdempotent {
		key = "none (non-idempotent: " + e.Reason + ")"
	}
	if why, transient := c.transient[in.ID]; transient {
		// A check that failed for now is run again on the next call: nothing
		// here is the coordinator's to settle, so it carries no settle line.
		return Instruction{
			Run: c.run, State: c.st.qualified(in), Kind: InstructionManualVerification, Attempt: in.Attempt,
			Prompt: fmt.Sprintf("Effect %d of %s has an intent with no result, and its check could not be read now.\nadapter: %s\nop: %s\nargs: %s\nkey: %s\nCheck failed transiently: %s; it is checked again on the next call.", seq, c.st.qualified(in), e.Adapter, e.Op, args, key, why),
			Child:  in.ID, Effect: seq, Workspace: c.workspace(), Transient: true, Person: c.declaredOutside(in, e),
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Effect %d of %s may or may not have been applied: its intent is journaled but its result is not, and it cannot be re-run safely.\n", seq, c.st.qualified(in))
	fmt.Fprintf(&b, "adapter: %s\nop: %s\nargs: %s\nkey: %s\n", e.Adapter, e.Op, args, key)
	settle := c.commandLine(c.e.opts.SettleLine(c.run, seq))
	outside := c.declaredOutside(in, e)
	if outside {
		settle += " --evidence <evidence> --operator <name> --reason <why>"
		b.WriteString("It acts outside the run. Verify in the world whether it took effect, then settle it as applied or not applied, the person's verdict as <settlement>, with what they checked as <evidence>, from their own session:\n" + settle)
	} else {
		settle += " --evidence <evidence>"
		b.WriteString("Check in the world whether it took effect, in the run's workspace: the files it writes (the paths in its args: present, and written by this run?), the git state it would leave (git status, the branch and its log), and any record it would create. ")
		b.WriteString("Then settle it yourself: <settlement> is --applied or --not-applied, and <evidence> is what you checked and what it showed, quoted for the shell; it is kept with the settlement. ")
		if in.Cmd != nil && in.Cmd.Seq == seq {
			b.WriteString("A command settled as applied also takes --exit N and, when its stdout routes, --stdout-file f. ")
		}
		b.WriteString("Run:\n" + settle)
	}
	if note, ok := c.detached[in.ID]; ok && note.why != "" {
		fmt.Fprintf(&b, "\n%s.", note.why)
	}
	return Instruction{
		Run: c.run, State: c.st.qualified(in), Kind: InstructionManualVerification, Attempt: in.Attempt,
		Prompt: b.String(), Submit: settle, Child: in.ID, Effect: seq, Workspace: c.workspace(),
		Person: outside,
	}
}

// settle journals the settlement of the open effect intent seq. A command's
// routing reads its exit code and stdout, so applying one needs rr (SettleRun).
// A not-applied settlement is a failed step, never a dead end: a keyed,
// idempotent command not yet re-issued on this visit is issued again once,
// with nothing spent and whether or not the state has a retry policy (nothing
// happened, so nothing was spent). Any other not-applied effect (a second one,
// an unkeyed or non-idempotent command, an edge effect) classes the step
// Blocked: it takes the state's OnClass(Blocked) route, the cause bound as
// {blockedCause}; a state with no Blocked route halts with the exhaustion halt
// (notAppliedHalt).
func (c *cycle) settle(seq int64, applied bool, rr *RunResult, evidence string, by *Operator) error {
	var in *instance
	for _, id := range c.st.order {
		if s, _, ok := openIntent(c.st.insts[id]); ok && s == seq {
			in = c.st.insts[id]
		}
	}
	if in == nil {
		for _, r := range c.recs {
			if r.Kind == JournalEffectIntent && r.Seq == seq {
				if ch := c.st.insts[r.Instance]; ch != nil && ch.Settled {
					return fmt.Errorf("%w: effect %d: the child is settled", ErrRejected, seq)
				}
			}
			if r.Kind == JournalEffectSettled {
				var d EffectSettledData
				if json.Unmarshal(r.Data, &d) == nil && d.Intent == seq {
					return fmt.Errorf("%w: effect %d is already settled", ErrRejected, seq)
				}
			}
		}
		return fmt.Errorf("%w: run %s has no open effect intent %d", ErrRejected, c.run, seq)
	}
	if err := c.checkUntilUnitMutation(in); err != nil {
		return err
	}
	if by != nil {
		// SettleAs: what was checked comes with the settlement, and an effect
		// outside the run is an operator's to settle, never a coordinator
		// session's.
		if strings.TrimSpace(evidence) == "" {
			return fmt.Errorf("%w: settle: effect %d: say what you checked and what it showed: --evidence <evidence>", ErrRejected, seq)
		}
		_, open, _ := openIntent(in)
		if c.declaredOutside(in, open) {
			if err := by.refusal("settle", c.run, coordinatorIdentities(c.recs), settlingOutside); err != nil {
				return err
			}
		}
	}
	edge := in.Pending != nil && in.Pending.Open == seq
	switch {
	case applied && !edge && rr == nil:
		return fmt.Errorf("%w: effect %d is a command whose routing reads its exit code and stdout: settle --applied --exit N [--stdout-file f]", ErrRejected, seq)
	case edge && rr != nil:
		return fmt.Errorf("%w: effect %d is an edge effect: it has no exit code to settle", ErrRejected, seq)
	}
	d := EffectSettledData{Intent: seq, Applied: applied, Result: rr, Evidence: strings.TrimSpace(evidence)}
	if by != nil {
		d.Operator, d.Reason, d.Harness, d.Session = strings.TrimSpace(by.Name), strings.TrimSpace(by.Reason), by.Harness, by.Session
	}
	if !applied {
		why := fmt.Sprintf("effect %d settled as not applied", seq)
		switch {
		case !edge && c.reissuable(in):
			d.Refused, d.Reissued = &RefusedData{Reasons: []string{why}}, true
		default:
			rd, err := c.decide(in, in.node(), Blocked, []string{why}, "")
			if err != nil {
				return err
			}
			if rd.Halt && rd.Plan == nil {
				if err := c.notAppliedHalt(in, &rd); err != nil {
					return err
				}
			}
			d.Refused = &rd
		}
	}
	from := c.st.qualified(in)
	if err := c.emit(in, JournalEffectSettled, 0, d); err != nil {
		return err
	}
	if d.Refused != nil {
		if err := c.noteExhausted(in, from, *d.Refused); err != nil {
			return err
		}
	}
	return c.drive()
}

// notAppliedHalt turns the Blocked halt of a not-applied effect at a state
// with no Blocked route into the state's declared exhausted edge, when it has
// a retry policy (C3: the declared route wins); a state with none keeps the
// plain halt.
func (c *cycle) notAppliedHalt(in *instance, rd *RefusedData) error {
	if r := retryOf(in.node()); r != nil {
		rd.Halt = false
		rd.Exhausted = ExhaustedBudget // recorded by noteExhausted
		return c.routeTo(in, rd, r.exhausted)
	}
	return nil
}

// reissuable reports whether the open command intent of in may be issued again
// for free after a not-applied settlement: its effect is keyed (so idempotent)
// and no not-applied settlement re-issued it on this visit.
func (c *cycle) reissuable(in *instance) bool {
	e := in.Cmd.Effect
	return e.Key != "" && !e.NonIdempotent && !in.Reissued
}

// tail is s cut to its last n bytes (on a rune boundary).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// declaredOutside reports whether the open effect e of in is outside the
// run. The open edge effect is the plan's external effect at the pending
// position (Pending.Done), the planned effect with its key template: its
// Outside flag says so; otherwise the definition the run is on now decides,
// by the same effect declared on that edge (an intent journaled before its
// effect was marked carries no flag). An effect whose declaration cannot be
// found on that edge (a changed edge, a removed state) is a person's: a
// person settles what is unclear.
func (c *cycle) declaredOutside(in *instance, e EffectData) bool {
	if e.Outside {
		return true
	}
	if in.Pending == nil || in.Pending.Open == 0 {
		return false // a command is never outside the run
	}
	n := in.node()
	if n == nil || n.def == nil {
		return true
	}
	pos := in.Pending.Done
	for _, step := range in.Pending.Plan.Steps {
		for _, x := range step.Effects {
			if x.Kind == "inc" {
				continue
			}
			if pos > 0 {
				pos--
				continue
			}
			if x.Outside {
				return true
			}
			from := n.def.find(step.From)
			if from == nil {
				return true
			}
			v, ok := stateViewOf(Inspect(c.st.def), from)
			if !ok || step.Index < 0 || step.Index >= len(v.Edges) {
				return true
			}
			declared := x.Adapter + "." + x.Op + " key=" + renderedKey(x)
			for _, s := range v.Edges[step.Index].Effects {
				if s == declared || strings.HasPrefix(s, declared+" ") {
					return strings.HasSuffix(s, outsideMarker)
				}
			}
			return true
		}
	}
	return true
}

// renderedKey is the key of a planned effect as render spells it.
func renderedKey(x EffectData) string {
	switch {
	case x.NonIdempotent:
		return "non-idempotent"
	case x.Key == "":
		return "?"
	}
	return x.Key
}

// stateViewOf is the view of node n in v or its children.
func stateViewOf(v DefView, n *node) (StateView, bool) {
	for _, s := range v.States {
		if s.node == n {
			return s, true
		}
	}
	for _, ch := range v.Children {
		if s, ok := stateViewOf(ch, n); ok {
			return s, true
		}
	}
	return StateView{}, false
}
