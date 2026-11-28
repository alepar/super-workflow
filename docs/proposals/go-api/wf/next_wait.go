package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// DefaultNextWait is how long next blocks with nothing for the coordinator
// when its harness's tool limit is unknown (H2: a known limit, less a margin,
// is used instead).
const DefaultNextWait = 100 * time.Second

// NextMore follows the count of ready units a next left out because its
// output would not fit the harness's limit.
const NextMore = "run next again once these are started"

// NextWaitCapEnv caps the wait a next picks for itself (the harness's or the
// default, never an explicit --timeout) when set to a duration: a test
// suite sets it so no test blocks for a harness's whole tool limit by
// accident.
const NextWaitCapEnv = "WF_NEXT_WAIT"

// capWait is d, or NextWaitCapEnv's duration when that is shorter.
func capWait(d time.Duration) time.Duration {
	if c, err := time.ParseDuration(os.Getenv(NextWaitCapEnv)); err == nil && c > 0 && c < d {
		return c
	}
	return d
}

// NextResult is what next <n> --wait returns: the instructions for the
// coordinator (units to spawn, a gate, the ended, stopped or waiting one),
// a halt that waits on a human, stalled units, or nothing before the timeout.
type NextResult struct {
	Instructions []Instruction
	Blocked      *BlockedView
	Stalled      []StalledView
	Timeout      bool
	// Waited is how long a call that timed out blocked.
	Waited time.Duration
	// Harness is the caller's, or the run's when the caller's environment
	// shows none: the spawn lines are rendered in its form.
	Harness string
	// Freed: a claimed unit settled while ready units, or unclaimed offers,
	// wait: the coordinator has room again.
	Freed bool
	// Offer is how a call with n left out counted (E3); nil when n was given.
	Offer *OfferView
	// Relisted: the instructions are the caller's own units it was offered
	// and never started, offered again at the end of a wait (J4).
	Relisted bool
	// More is how many ready units did not fit this call's output under the
	// caller's harness limit: they are offered by the next call.
	More int
	// workspace is the run's, read in the cycle that produced the result:
	// the command lines it is rendered with need no second lock.
	workspace string
	// childRunning is a started child of this coordinator in the result's
	// snapshot. An offer alone does not establish that a child is running.
	childRunning  bool
	continuations *int // issued Stop decisions for an attached, ended run
}

// BlockedView is a halt the run waits on a human for.
type BlockedView struct {
	State, Child, Reason string
}

// StalledView is one claimed unit the coordinator is asked about: its lease
// ran out, or its watched transcript went quiet, and the engine cannot tell
// whether its child still works. Claim is the token its child fetched with,
// to tell which child it is.
type StalledView struct {
	Name    string
	Since   time.Time
	Claim   string    `json:",omitempty"`
	Recheck time.Time `json:",omitzero"` // when next asks again if this question is not answered
	// Unprogressed is how many stall episodes in a row the coordinator
	// answered active with no engine event from the child between them. At
	// unprogressedAsk or more the question asks for evidence of work.
	Unprogressed int `json:",omitempty"`
	// Asks is how many times next has put this episode's question, this one
	// included: above 1, the earlier asks went unanswered (inconclusive).
	Asks int `json:",omitempty"`
}

// unprogressedAsk is the count of active answers in a row with no progress
// from which the stall question asks for evidence: one such answer counts
// once, the second in a row changes the question.
const unprogressedAsk = 2

// NextWait is next <n> --wait (C1): the one call of the coordinator's loop.
// It hands out up to n units to co and returns as soon as the run has
// something for the coordinator: units to spawn, a human gate or manual
// verification, a halt or holds that wait on a human, a stalled unit, a stop
// or the end. A child finishing is not an event when nothing new became
// ready (C2a), and neither is a wait state or a running command, which the
// engine drives itself. Otherwise it blocks until timeout and reports it.
func (e *Engine) NextWait(run RunID, n int, co Coordinator, timeout time.Duration) (NextResult, error) {
	if timeout <= 0 {
		timeout = capWait(DefaultNextWait)
	}
	// The admitted session is live while next blocks.
	var release func()
	var stop chan struct{}
	defer func() {
		if release != nil {
			release()
		}
		if stop != nil {
			close(stop)
		}
	}()
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	claimedAtStart := -1
	for {
		var res NextResult
		var done bool
		var next, now time.Time
		var changed <-chan struct{}
		err := e.withCoordinatorNext(run, co, func(c *cycle) error {
			if release == nil {
				release = e.holdCoordinatorLock(run, co.Session)
			}
			ins, err := c.handOutCycle(n, co)
			if err != nil {
				return err
			}
			res.Harness, res.workspace = c.caller.Harness, c.workspace()
			res.Offer, res.More = c.offer, c.more
			for _, in := range ins {
				if in.Kind != InstructionWait {
					res.Instructions = append(res.Instructions, in)
				}
			}
			if h, ok := c.heldAt(); ok && len(res.Instructions) == 0 {
				res.Blocked = &BlockedView{State: c.st.qualified(h), Child: h.ID, Reason: h.Reason}
			}
			if len(res.Instructions) == 0 && res.Blocked == nil {
				now := c.now()
				for _, id := range c.st.order {
					v, err := c.askStalled(c.st.insts[id], now)
					if err != nil {
						return err
					}
					if v != nil {
						res.Stalled = append(res.Stalled, *v)
					}
				}
			}
			counts := c.unitCounts()
			res.childRunning = c.hasRunningChild()
			if claimedAtStart < 0 {
				claimedAtStart = counts.Running
			} else if counts.Running < claimedAtStart && counts.Ready > 0 {
				res.Freed = true
			}
			// next 0 asks for no units: one pass, nothing to wait for.
			done = n == 0 || len(res.Instructions) > 0 || res.Blocked != nil || len(res.Stalled) > 0 || res.Freed
			if done {
				if c.st.ended && c.st.turn != nil {
					res.continuations = &c.st.turn.count
				}
				return c.prepareTurnHandoff(res.Instructions)
			}
			// Every deadline after the time this pass judged eligibility by
			// (handAt) is scheduled, one the clock already crossed since then
			// at once: it was not judged. One at or before handAt was, so it
			// is no reason to wake (it waits on capacity or another session).
			now = c.now()
			for _, d := range c.deadlines(now) {
				if d.Due.After(c.handAt) && (next.IsZero() || d.Due.Before(next)) {
					next = d.Due
				}
			}
			if stop != nil {
				close(stop)
			}
			stop = make(chan struct{})
			changed = e.j.Watch(run, stop)
			return nil
		})
		switch {
		case errors.Is(err, ErrRunLocked):
			// A child's done holds the run: wait its turn.
			select {
			case <-limit.C:
				return NextResult{Timeout: true, Waited: timeout}, nil
			case <-time.After(waitLockRetry):
			}
			continue
		case err != nil:
			return NextResult{}, err
		case done:
			return res, nil
		}
		var wake <-chan time.Time
		var t *time.Timer
		if !next.IsZero() {
			t = time.NewTimer(max(0, next.Sub(now)) + time.Millisecond)
			wake = t.C
		}
		timedOut := false
		select {
		case <-changed:
		case <-wake:
		case <-limit.C:
			timedOut = true
		}
		if t != nil {
			t.Stop()
		}
		if timedOut {
			// J4: a whole wait passed with nothing else and co's own units
			// still unclaimed: their spawns were refused or forgotten (a
			// spawned child fetches well inside one wait). They come back,
			// with fresh tokens; no clock on the unit, nothing lost.
			var again []Instruction
			more := 0
			workspace := ""
			err := e.withCoordinatorNext(run, co, func(c *cycle) error {
				c.caller = co
				var err error
				again, err = c.relist(co)
				more, workspace = c.more, c.workspace()
				res.childRunning = c.hasRunningChild()
				return err
			})
			if errors.Is(err, ErrRunLocked) {
				return NextResult{Timeout: true, Waited: timeout}, nil // busy to the end of the wait
			}
			if err != nil {
				return NextResult{}, err
			}
			if len(again) > 0 {
				return NextResult{Instructions: again, Relisted: true, Harness: co.Harness, More: more, workspace: workspace, childRunning: res.childRunning}, nil
			}
			return NextResult{Timeout: true, Waited: timeout, childRunning: res.childRunning}, nil
		}
	}
}

func (c *cycle) hasRunningChild() bool {
	if c.caller.Session == "" {
		return false
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		u := in.Unit
		if in.Settled || u == nil || !u.State.claimed() || u.State == UnitStalled {
			continue
		}
		if u.childOwner() == c.caller.Session {
			return true
		}
	}
	return false
}

// NextNothing is the line next prints when its timeout passed with nothing
// for the coordinator, after "nothing yet after <wait>: " (H2).
const NextNothing = "run next again; next returns as soon as there is something, so sleeping or checking status in between gains nothing"

// NextRelisted heads the units a coordinator was offered and has not
// started, shown again at the end of a wait with nothing else (J4). They
// are the offers it already has, under the same claim tokens (relist).
const NextRelisted = "not started yet: start these too, each only if its earlier start is no longer running (these are the same offers as before, so a second child of one would work under the same claim). " + NextStartAgain

// NextNotStarted heads a unit offered again, under a new claim token,
// because the coordinator was offered it before and no child of it started
// while later offers' children did, or the grace passed. With two
// claims the engine tells the second child the unit is taken, which the
// line promises; a unit shown again under the same claim gets NextSameOffer
// instead. %s is the unit's name.
const NextNotStarted = "not started yet: %s was offered to you earlier and no child of it started; start its child again (if an earlier child of it does start, one of the two is told the unit is taken and stops). " + NextStartAgain

// NextSameOffer heads a unit shown again under the claim token it was
// offered under before (a re-list, Instruction.SameClaim). The engine tells
// two children apart by their claims, so two children of this offer would
// both do the work and neither would be told to stop: the line promises
// nothing of the kind and says when to start one. %s is the unit's name.
const NextSameOffer = "not started yet: %s was offered to you earlier and no child of it fetched its prompt; this is the same offer, under the same claim. Start its child again only if its earlier start is no longer running: two children of it would both do the work, and neither would be told to stop. " + NextStartAgain

// NextStartAgain ends every offer of a unit made again because no child of
// it fetched: the unit stays the coordinator's to start, however often.
const NextStartAgain = "If the same child fails to start repeatedly, say what its spawn printed and keep driving the run."

// NextCountsLead heads the run's exact unit totals under a stopped or ended
// instruction, for the coordinator's report.
const NextCountsLead = "The run's totals: "

// NextAdopted and NextAdoptCurrent are what next --adopt prints: it adopted
// (a migrated record), or the run was already on this binary's definition;
// either way it did nothing else. %s is the coordinator's next line.
const (
	NextAdopted      = "adopted: the run is now on this binary's definition (journaled as migrated). next --adopt hands out nothing and binds no harness; the run's coordinator drives it with: %s"
	NextAdoptCurrent = "nothing to adopt: the run is already on this binary's definition. next --adopt hands out nothing and binds no harness; the run's coordinator drives it with: %s"
)

// NextStartAll heads an offer to a coordinator whose child cap is unknown.
const NextStartAll = "start all of these (if you run fewer children at once, pass next --children <k> and you are offered only what fits)"

// NextFreed is the line blocking next prints when a child settled and ready
// units wait beyond the n it was given.
const NextFreed = "a child finished and units are ready: run next again"

// RenderNext is what next prints: per unit an offer to the coordinator naming
// the caller's harness's spawn call, then the unit's brief between two rule
// lines (I1), and each other event with the rule that applies to it (C2). l is the
// verb-line builder of the run.
func RenderNext(r NextResult, harnessName string, l func(verb string, args ...string) string) string {
	// A reserved question is the whole coordinator reply. Do not append stale
	// events, generic stop/resume text, spawn instructions or harness advice.
	if len(r.Instructions) == 1 {
		in := r.Instructions[0]
		if in.Kind == InstructionStopped && in.Question != "" {
			return in.Prompt + "\n"
		}
	}

	var b strings.Builder
	a := harness.For(harnessName)
	if r.Timeout {
		waited := r.Waited.Round(time.Second)
		if r.Waited < 10*time.Second {
			waited = r.Waited.Round(100 * time.Millisecond)
		}
		return r.withDriveReminder(fmt.Sprintf("nothing yet after %s: %s\n", waited, NextNothing))
	}
	if r.Freed && len(r.Instructions) == 0 && r.Blocked == nil && len(r.Stalled) == 0 {
		return r.withDriveReminder(NextFreed + "\n")
	}
	if r.Relisted {
		b.WriteString(NextRelisted + "\n")
	}
	if o := r.Offer; o != nil && hasAgent(r.Instructions) {
		units := 0
		for _, in := range r.Instructions {
			if in.Kind == InstructionAgent {
				units++
			}
		}
		switch {
		case o.Limit > 0 && o.Source == ChildrenSource:
			fmt.Fprintf(&b, "offered %d: you run %d children at once (%s), %d of yours running or starting; start all %d. While that many run, next waits for one to finish\n", units, o.Limit, o.Source, o.Busy, units)
		case o.Limit > 0:
			fmt.Fprintf(&b, "offered %d: your harness runs %d subagents at once (%s), %d of yours running or starting; start all %d\n", units, o.Limit, o.Source, o.Busy, units)
		default:
			b.WriteString(NextStartAll + "\n")
		}
	}
	if note := spawnNoteOf(a, r.Instructions); note != "" {
		b.WriteString(note + "\n")
	}
	for _, in := range r.Instructions {
		switch in.Kind {
		case InstructionAgent:
			b.WriteString(renderUnit(a, in))
		case InstructionHumanGate, InstructionManualVerification:
			if in.CoordinatorAnswers {
				// A fanout fork measures the coordinator's own answer here.
				fmt.Fprintf(&b, "%s %s: this fork measures your answer\n%s\n", in.Kind, in.State, indent(in.Prompt))
				b.WriteString("  Choose the answer yourself, as the person who started the run would, and run the command above with your choice.\n")
				continue
			}
			if in.Kind == InstructionManualVerification && in.Transient {
				fmt.Fprintf(&b, "%s %s: checked again on the next call\n%s\n", in.Kind, in.State, indent(in.Prompt))
				b.WriteString("  Its check failed for now and runs again: do not settle it; run next again.\n")
				continue
			}
			if in.Kind == InstructionManualVerification && !in.Person {
				fmt.Fprintf(&b, "%s %s: yours to settle\n%s\n", in.Kind, in.State, indent(in.Prompt))
				b.WriteString("  Yours to settle, not a person's: check the effect in the world, run the command above with your verdict and what you checked, then next again. If what you can check does not decide it, leave it unsettled and report why: the run waits here.\n")
				continue
			}
			fmt.Fprintf(&b, "%s %s: this needs a person\n%s\n", in.Kind, in.State, indent(in.Prompt))
			b.WriteString("  This needs a human: ask yours and give the command above their answer, or report it and end the session.\n")
		case InstructionEnded:
			fmt.Fprintf(&b, "ended: %s, outcome %s", in.Terminal, in.Outcome)
			if in.Cause != "" {
				fmt.Fprintf(&b, ", cause %s", in.Cause)
			}
			b.WriteString("\n  The run is over: write your final report now, quoting this line.\n")
			if r.continuations != nil {
				fmt.Fprintf(&b, "  automatic continuations: %d\n", *r.continuations)
			}
			if in.Counts != "" {
				fmt.Fprintf(&b, "  %s%s\n", NextCountsLead, in.Counts)
			}
			if in.Resume != "" {
				fmt.Fprintf(&b, "  To resume this run later, run:\n  %s\n", in.Resume)
			}
		case InstructionRecovery:
			fmt.Fprintf(&b, "recovery %s:\n%s\n", in.Name, indent(in.Prompt))
			b.WriteString("  Yours to answer, not a human's: choose on the evidence above and run one of its recover lines (for resume, make the harness call first), then this command again. Never leave it unanswered.\n")
		case InstructionStopped, InstructionWaiting:
			fmt.Fprintf(&b, "%s:\n%s\n", in.Kind, indent(in.Prompt))
			if in.Counts != "" {
				fmt.Fprintf(&b, "  %s%s\n", NextCountsLead, in.Counts)
			}
		}
	}
	if r.More > 0 && hasAgent(r.Instructions) {
		fmt.Fprintf(&b, "%d more are ready: %s\n", r.More, NextMore)
	}
	for _, in := range r.Instructions {
		if in.Advice != "" {
			b.WriteString(in.Advice + "\n")
			break
		}
	}
	if bl := r.Blocked; bl != nil {
		child := []string{}
		if bl.Child != "" {
			child = []string{"--child", bl.Child}
		}
		fmt.Fprintf(&b, "blocked %s: %s\n", bl.State, bl.Reason)
		fmt.Fprintf(&b, "  The run waits on a human: report this cause and end the session. On their answer, run one of these, then this command again.\n  One more attempt:\n  %s\n  Its fallback edge:\n  %s\n",
			l("retry", child...), l("retry", append(child, "--stop")...))
	}
	for _, s := range r.Stalled {
		// Observational only (rule 5): no harness check is printed, and a
		// quiet child may be working inside one long tool call.
		claim := ""
		if s.Claim != "" {
			claim = " (claim " + s.Claim + ")"
		}
		recheck := "its next recheck"
		if !s.Recheck.IsZero() {
			recheck = "its recheck at " + s.Recheck.UTC().Format(time.RFC3339)
		}
		if s.Unprogressed >= unprogressedAsk {
			// A child stuck on a prompt its coordinator could not
			// see kept its lease on bare liveness answers, forever.
			fmt.Fprintf(&b, "stalled %s: you answered active %d times in a row, and its child%s showed the engine no progress in between. Running alone is not enough now: look in the child's own thread.\n", s.Name, s.Unprogressed, claim)
			fmt.Fprintf(&b, "  If you saw it work since the last question (new output, a command still running), say what you saw:\n  %s\n", l("heartbeat", "active", s.Name, "--note", "<what you saw>"))
			fmt.Fprintf(&b, "  If it waits on a pending prompt (an approval or a question), resolve it as your harness allows, then run:\n  %s\n  If it shows an error, or you confirmed it exited without done or failed, run:\n  %s\n  and the unit is offered again.\n",
				l("heartbeat", "active", s.Name), l("heartbeat", "died", s.Name))
			// One cannot-tell instruction per round: the first waits for the
			// recheck, the next is died.
			if s.Asks > 1 {
				fmt.Fprintf(&b, "  You could not tell when last asked: if you still cannot tell (you cannot see its thread, or cannot judge it), run:\n  %s\n", l("heartbeat", "died", s.Name))
			} else {
				fmt.Fprintf(&b, "  If you cannot see its thread or cannot tell, run neither: run next again, and next waits for a sign of life or %s, then asks again.\n", recheck)
			}
			continue
		}
		fmt.Fprintf(&b, "stalled %s: its child%s has shown the engine no sign of life since %s. Is it still running?\n", s.Name, claim, s.Since.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "  Look with your own harness, without stopping it: a quiet child may be inside one long tool call. If it is still working, run:\n  %s\n  If you confirmed it has exited without done or failed, run:\n  %s\n  If you cannot tell (you cannot see it, or your check failed or was refused), run neither and wait for fresh evidence: run next again, and next waits for a sign of life or %s, then asks again. Its claim stays pending meanwhile. Not seeing a child does not mean it is gone.\n",
			l("heartbeat", "active", s.Name), l("heartbeat", "died", s.Name), recheck)
	}
	return r.withDriveReminder(b.String())
}

func (r NextResult) withDriveReminder(text string) string {
	if r.childRunning || r.Blocked != nil || len(r.Stalled) > 0 {
		// A stall question asks for a look first: the reminder to run next
		// at once would send a coordinator that cannot tell straight back to
		// the same question.
		return text
	}
	for _, in := range r.Instructions {
		switch in.Kind {
		case InstructionHumanGate, InstructionManualVerification, InstructionEnded, InstructionStopped, InstructionWaiting:
			return text
		}
	}
	return text + "continue driving: if no child you started is running, run next before ending your turn.\n"
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ")
}

func hasAgent(ins []Instruction) bool {
	for _, in := range ins {
		if in.Kind == InstructionAgent {
			return true
		}
	}
	return false
}

// spawnNoteOf is a's note on its own spawn refusal, when ins offers a
// native spawn (not a headless one), else "".
func spawnNoteOf(a harness.Adapter, ins []Instruction) string {
	n, ok := a.(harness.SpawnNote)
	if !ok {
		return ""
	}
	for _, in := range ins {
		if in.Kind == InstructionAgent && in.Spawn == "" {
			return n.SpawnNote()
		}
	}
	return ""
}

// renderUnit is one unit as next prints it (I1): the offer, addressed to the
// coordinator, then its brief between two rule lines, addressed to the
// child; no line begins with a command to run here. The hand-out sizes an
// offer by it (handOutCycle).
func renderUnit(a harness.Adapter, in Instruction) string {
	if in.Spawn != "" {
		return renderHeadless(in)
	}
	call := a.SpawnLine(harness.Spawn{Brief: briefSlot, Claim: briefClaim(in.Brief), Model: in.Dispatch.Model, Effort: in.Dispatch.Effort, AgentType: in.Dispatch.AgentType, ErrLog: in.ErrLog})
	call = strings.ReplaceAll(call, strconv.Quote(briefSlot), briefSlotText) // a Go-quoted argument
	call = strings.ReplaceAll(call, jsonQuoted(briefSlot), briefSlotText)    // a JSON string argument
	call = strings.ReplaceAll(call, briefSlot, briefSlotText)                // a worded one
	again := ""
	if in.NotStarted {
		again = fmt.Sprintf(NextNotStarted+"\n", in.Name)
		if in.SameClaim {
			again = fmt.Sprintf(NextSameOffer+"\n", in.Name)
		}
		if sl := startLogOf(a); sl != nil && in.PrevErrLog != "" {
			again += sl.StartLog(in.PrevErrLog) + "\n"
			if len(in.PrevErrTail) > 0 {
				again += "its last lines:\n"
				for _, l := range in.PrevErrTail {
					again += "  " + escapeControl(l) + "\n"
				}
			}
		}
	}
	with := "with " + call + "."
	if strings.Contains(call, "\n") { // a shell command whose brief is its own line (pi)
		with = "by running this as one command, with the text between the lines in place of " + briefSlotText + ", unchanged:\n" + call + "\n"
	} else {
		with += " "
	}
	return again + fmt.Sprintf("start a child agent for %s %sIts whole task is the text between the %s lines, passed verbatim with its command first; the unit is a child's to run, which keeps its work out of your context:\n%s\n%s\n%s\n",
		in.Name, with, briefRule, briefRule, in.Brief, briefRule)
}

// renderHeadless is a unit of another harness as next prints it: the spawn
// verb's command, alone on its line, which starts the child as a headless
// process of that harness and returns. The brief is the engine's to pass,
// so none is printed. A re-offer quotes the end of the earlier child's
// stderr, which the spawn verb wrote.
func renderHeadless(in Instruction) string {
	again := ""
	if in.NotStarted {
		again = fmt.Sprintf(NextNotStarted+"\n", in.Name)
		if in.SameClaim {
			again = fmt.Sprintf(NextSameOffer+"\n", in.Name)
		}
		if in.PrevErrLog != "" && len(in.PrevErrTail) > 0 {
			again += "its earlier launch's stderr (" + in.PrevErrLog + ") ends:\n"
			for _, l := range in.PrevErrTail {
				again += "  " + escapeControl(l) + "\n"
			}
		}
	}
	return again + fmt.Sprintf("start a %s child for %s by running this command, as printed; it starts the child as a headless %s process and returns at once (do not wait for the child, keep running next):\n%s\n",
		in.Dispatch.Family, in.Name, in.Dispatch.Family, in.Spawn)
}

// jsonQuoted is s as a JSON string literal, the form an adapter that takes
// its arguments as JSON (agy) quotes briefSlot in.
func jsonQuoted(s string) string {
	b, _ := json.Marshal(s) // a string always encodes
	return string(b)
}

// briefRule frames a brief in next's output (I1).
const briefRule = "-----"

// The spawn call in an offer names its prompt argument by what fills it, the
// brief below, instead of quoting the brief inside the call: briefSlot is
// rendered by the adapter and replaced by briefSlotText.
const (
	briefSlot     = "\x00brief\x00"
	briefSlotText = "<the text between the lines>"
)
