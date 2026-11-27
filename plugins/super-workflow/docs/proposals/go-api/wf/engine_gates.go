package wf

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/sink"
)

// defaultPoll is the retry-after of a wait instruction when the Wait sets no
// Poll interval.
const defaultPoll = 30 * time.Second

// presenter is implemented by the ops of a kind whose ready instance shows an
// instruction other than an agent's (a gate's question, a wait's retry-after).
type presenter interface {
	present(c *cycle, in *instance, n *node) *Instruction
}

// answerer is implemented by the ops of a kind that takes an answer.
type answerer interface {
	answer(c *cycle, in *instance, n *node, value string, number *int) error
}

func (k *gateKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}
func (k *waitKind) execute(c *cycle, in *instance, n *node) error {
	return execOps(k.ops, k.base, c, in, n)
}

func (k *gateKind) present(c *cycle, in *instance, n *node) *Instruction {
	if p, ok := k.ops.(presenter); ok {
		return p.present(c, in, n)
	}
	return nil
}

func (k *waitKind) present(c *cycle, in *instance, n *node) *Instruction {
	if p, ok := k.ops.(presenter); ok {
		return p.present(c, in, n)
	}
	return nil
}

func (c *cycle) now() time.Time { return c.e.opts.Clock() }

// arrivedAt is when the instance last arrived at its current state: the
// journaled At of its latest transition, else of its start. The fold records it
// as the records apply, so this reads no journal. The gate and wait timeouts
// and backoff counts all run from here, on the journaled clock.
func (c *cycle) arrivedAt(in *instance) time.Time {
	if !in.arrived.IsZero() {
		return in.arrived
	}
	return c.recs[0].At
}

// expireTimers journals what the clock alone decides, once at the top of each
// drive: a gate whose Timeout has passed takes its timeout edge, a lease that
// ran out re-issues the attempt, and a unit whose spawn window
// or lease passed is lost (lose), except a watched running unit at its lease,
// which is reported stalled (leaseExpired). Wait timeouts are decided by the Wait's
// own execution, after it reads its observation. Nothing here sleeps.
func (c *cycle) expireTimers() error {
	now := c.now()
	for _, d := range c.deadlines(now) {
		if now.Before(d.Due) {
			continue
		}
		in := c.st.insts[d.Instance]
		if !c.untilUnitAllows(in) {
			continue
		}
		switch d.Kind {
		case deadlineGateTimeout:
			if g, ok := in.node().kind.(*gateKind); ok {
				if err := c.takeTimeout(in, in.node(), g.timeout, "gate"); err != nil {
					return err
				}
			}
		case deadlineLease:
			// A wave instruction's lease re-issues here; a unit's lease expiry is
			// a loss, or a stall report for a watched running unit.
			switch {
			case in.Unit != nil:
				if err := c.leaseExpired(in, d); err != nil {
					return err
				}
			case in.Lease != nil:
				if err := c.emit(in, JournalLeaseExpired, 0, LeaseExpiredData{Holder: in.Lease.Holder}); err != nil {
					return err
				}
			}
		case deadlineHandOut:
			// Journals nothing: an expired hand-out is re-offered, under the
			// same name, by the next HandOut.
		case deadlineSpawn:
			if err := c.loseExpired(in, d, LostCauseSpawn); err != nil {
				return err
			}
		case deadlineStall:
			if err := c.stall(in, d); err != nil {
				return err
			}
		case deadlineEffectPoll, deadlineEffectMax:
			// Journaled by the detached-effect path.
		case deadlineStallRecheck:
			// Journals nothing: a blocking next wakes for it and asks the
			// stall question again (NextWait).
		case deadlineEndSettle:
			// Journals nothing: loseEndedChildren, earlier in the same drive,
			// resolves or re-arms it.
		case deadlineWaitTimeout, deadlineWaitPoll, deadlineBackoff:
			// Decided in drive, after the wait's observation or the backoff case.
		}
	}
	return nil
}

// takeTimeout journals the timeout edge of a gate or wait as auto_answered with
// the answer "timeout".
func (c *cycle) takeTimeout(in *instance, n *node, u *edgeUse, what string) error {
	plan, err := c.planOf(in, *u)
	if err != nil {
		return c.halt(in, err.Error())
	}
	return c.emit(in, JournalAutoAnswered, Accepted, AutoAnsweredData{
		Answer: "timeout", Park: fmt.Sprintf("%s %s timed out after %s", what, n.name, u.e.timeout), Plan: &plan,
	})
}

// mapLeaseOf is the lease time-to-live of the Map that spawned in; zero when in
// is not a Map child or its Map has no Lease.
func (c *cycle) mapLeaseOf(in *instance) time.Duration {
	if in.Spawner == "" {
		return 0
	}
	parent := c.st.insts[in.Parent]
	if parent == nil {
		return 0
	}
	pn := parent.def.find(in.Spawner)
	if pn == nil {
		return 0
	}
	if m, ok := pn.kind.(*mapKind); ok {
		return m.lease
	}
	return 0
}

// backoffCount is how many backoff_scheduled records of class the instance
// journaled on this visit of its state and since its last retry.
func (c *cycle) backoffCount(in *instance, class OutcomeClass) int {
	n := 0
	for i := len(c.recs) - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != in.ID {
			continue
		}
		if r.Kind == JournalTransition || r.Kind == JournalRetried {
			break
		}
		if r.Kind == JournalBackoffScheduled && r.Class == class {
			n++
		}
	}
	return n
}

// backoff journals the next delay of the state's backoff schedule for class,
// if one is left: the instance then waits for its due time (nothing sleeps;
// Next shows a wait instruction) and re-executes the state with no attempt
// spent. The schedule is the state's OnClassBackoff one, or for Unavailable on
// a state with no Unavailable route DefaultUnavailableBackoff. scheduled is
// false when the class has no schedule or it is spent; the class edge is then
// taken.
func (c *cycle) backoff(in *instance, n *node, class OutcomeClass, reasons []string, child bool) (scheduled bool, err error) {
	delays := backoffSchedule(retryOf(n), class)
	i := c.backoffCount(in, class)
	if i >= len(delays) {
		return false, nil
	}
	return true, c.emit(in, JournalBackoffScheduled, class, BackoffScheduledData{
		Class: class, Index: i, Delay: delays[i], Due: c.now().Add(delays[i]), Reasons: reasons, ChildFailed: child,
	})
}

// backoffSchedule is the delays a state retries class with before it takes its
// class edge or halts; r is nil for a state with no retry policy.
func backoffSchedule(r *retryData, class OutcomeClass) []time.Duration {
	if r != nil {
		if _, routed := r.byClass[class]; routed || class != Unavailable {
			return r.backoff[class]
		}
	}
	if class == Unavailable {
		return DefaultUnavailableBackoff
	}
	return nil
}

// waitInstruction is a wait instruction for in, retry-after at least one second.
func (c *cycle) waitInstruction(in *instance, prompt string, after time.Duration) *Instruction {
	secs := int((after + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return &Instruction{
		Run: c.run, State: c.st.qualified(in), Kind: InstructionWait, Attempt: in.Attempt,
		Prompt: prompt, Child: in.ID, Workspace: c.workspace(), RetryAfterSeconds: secs,
	}
}

func (c *cycle) backoffInstruction(in *instance) *Instruction {
	b := in.Backoff
	d, ok := c.dueFor(in, deadlineBackoff, c.now())
	if b == nil || !ok {
		return nil
	}
	return c.waitInstruction(in, fmt.Sprintf("Retrying %s after %s failure%s (delay %d, %s); call next again after the retry-after.",
		in.Cur, withArticle(b.Class.String()), failureWhy(in.LastFailure), b.Index+1, b.Delay), d.Due.Sub(c.now()))
}

// withArticle is word after its indefinite article.
func withArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiouAEIOU", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}

// failureWhy is ": <reasons>" for a failure that left reasons, else "".
func failureWhy(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return ": " + strings.Join(reasons, "; ")
}

// ---------------------------------------------------------------- policy

// optionsMatch evaluates a data predicate against the run's journaled options:
// the options are decoded into the declared options struct, the named field is
// read and its fmt.Sprint compared. A zero Pred matches.
func (c *cycle) optionsMatch(p Pred) bool {
	if c.st.def.options == nil {
		return predMatch(p, nil)
	}
	v := reflect.New(c.st.def.options.typ)
	if raw := c.st.started.Options; len(raw) > 0 {
		_ = json.Unmarshal(raw, v.Interface())
	}
	return predMatch(p, v.Elem().Interface())
}

func predMatch(p Pred, opts any) bool {
	switch {
	case p.not && len(p.all) == 1:
		return !predMatch(p.all[0], opts)
	case len(p.all) > 0:
		for _, q := range p.all {
			if !predMatch(q, opts) {
				return false
			}
		}
		return true
	case p.field == "":
		return true
	case opts == nil:
		return false
	}
	f := reflect.ValueOf(opts).FieldByName(p.field)
	return f.IsValid() && fmt.Sprint(f.Interface()) == p.value
}

// ---------------------------------------------------------------- gate

// gateOps is the typed half of a Gate: it asks the policy, then a human.
type gateOps[S any, A comparable] struct{ g *Gate[S, A] }

// execute lets the first matching AutonomyPolicy rule answer a policy-answerable
// gate (auto_answered with the rule's park note). A reserved gate never reaches
// the policy. An unanswered gate appends nothing and waits for Answer.
func (o gateOps[S, A]) execute(c *cycle, in *instance, n *node) error {
	if o.g.k.reservation == Reserved {
		if c.e.recordedReplay {
			return nil
		}
		_, err := o.g.k.liveIntValues(c, in)
		if err != nil {
			return c.fail(in, n, Blocked, []string{err.Error()}, "")
		}
		return c.offerGate(in)
	}
	for _, r := range in.def.policy.rules {
		if r.gate != n || !c.optionsMatch(r.when) {
			continue
		}
		a, ok := o.value(r.answer)
		if !ok {
			return c.fail(in, n, Blocked, []string{fmt.Sprintf("policy answer %q is not in %s", r.answer, o.g.k.answers.name)}, "")
		}
		plan, ok, err := o.planFor(c, in, n, a)
		if err != nil || !ok {
			return err
		}
		return c.emit(in, JournalAutoAnswered, Accepted, AutoAnsweredData{Answer: r.answer, Park: r.park, Plan: &plan})
	}
	return c.offerGate(in)
}

// offerGate journals the person gate's offer once per visit (gate_offered),
// so a fork can be cut at the checkpoint before it. A recorded replay
// journals none: it replays the run's own records.
func (c *cycle) offerGate(in *instance) error {
	if c.e.recordedReplay {
		return nil
	}
	visit := in.Visits[in.Cur]
	for i := len(c.recs) - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Instance != in.ID {
			continue
		}
		if r.Kind == JournalTransition {
			break // an earlier visit's offer
		}
		var d GateOfferedData
		if r.Kind == JournalGateOffered && json.Unmarshal(r.Data, &d) == nil && d.Visit == visit && r.State == in.Cur {
			return nil
		}
	}
	k, ok := in.node().kind.(*gateKind)
	if !ok {
		return nil
	}
	return c.emit(in, JournalGateOffered, 0, GateOfferedData{State: c.st.qualified(in), Visit: visit, Question: k.question})
}

// value finds the enum value whose rendering is s.
func (o gateOps[S, A]) value(s string) (A, bool) {
	vals, _ := o.g.k.answers.vals.([]A)
	for _, v := range vals {
		if fmt.Sprint(v) == s {
			return v, true
		}
	}
	var zero A
	return zero, false
}

// planFor routes the answer through the gate's case table.
func (o gateOps[S, A]) planFor(c *cycle, in *instance, n *node, a A) (EdgePlan, bool, error) {
	ctx, err := c.ctx(in)
	if err != nil {
		return EdgePlan{}, false, c.failErr(in, n, err)
	}
	u, ok, err := c.route(in, n, nil, any(a), ctx)
	if err != nil || !ok {
		return EdgePlan{}, false, err
	}
	plan, err := c.planOf(in, u)
	if err != nil {
		return plan, false, c.halt(in, err.Error())
	}
	return plan, true, nil
}

// answer journals the answer and its process session, then routes on it.
func (o gateOps[S, A]) answer(c *cycle, in *instance, n *node, value string, number *int) error {
	if c.st.breakpointBlocks(in) {
		return fmt.Errorf("%w: gate %s is held at a run breakpoint; use wf resume", ErrRejected, c.st.qualified(in))
	}
	a, ok := o.value(value)
	if !ok {
		return fmt.Errorf("%w: answer %q is outside %s %v", ErrRejected, value, o.g.k.answers.name, o.g.k.answers.values)
	}
	resolved := number
	if !c.e.recordedReplay {
		var err error
		resolved, err = o.g.k.resolveIntValue(c, in, value, number)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRejected, err)
		}
	}
	plan, ok, err := o.planFor(c, in, n, a)
	if err != nil || !ok {
		return err
	}
	return c.emit(in, JournalGateAnswered, Accepted, GateAnsweredData{Answer: value, IntValue: resolved, Plan: &plan, Session: c.e.opts.caller.Session})
}

// present is the human-gate instruction: the question, the answer set and the
// presented artifacts, with no token. Visit binds an answer to this arrival, a
// delegation shows as Delegated, and a timed gate carries its deadline.
func (o gateOps[S, A]) present(c *cycle, in *instance, n *node) *Instruction {
	k := o.g.k
	var b strings.Builder
	b.WriteString(k.question)
	for _, a := range k.presents {
		p, err := c.scope(in).bindString(a.path)
		if err != nil {
			p = a.path
		}
		fmt.Fprintf(&b, "\nPresents %s: %s", a.name, p)
	}
	q := c.st.qualified(in)
	// The answer names this instance and visit: of sibling gates at one
	// state, a bare answer would take the oldest.
	submit := c.commandLine(c.e.opts.AnswerLine(c.run, q))
	if in.ID != "" {
		submit += fmt.Sprintf(" --child %s --visit %d", sink.Quote(in.ID), in.Visits[in.Cur])
	}
	fmt.Fprintf(&b, "\nAnswer with one of %s, as <value>:\n%s", strings.Join(k.answers.values, ", "), submit)
	ins := &Instruction{
		Run: c.run, State: q, Kind: InstructionHumanGate, Attempt: in.Attempt, Prompt: b.String(),
		Submit: submit, Child: in.ID, Workspace: c.workspace(), Question: k.question, Answers: k.answers.values,
		Visit: in.Visits[in.Cur], CoordinatorAnswers: c.coordinatorAnswers(in),
	}
	if !c.e.recordedReplay {
		ins.IntValues, _ = k.liveIntValues(c, in)
		ins.Prompt += gateIntInstructions(ins.IntValues, submit)
	}
	if v, ok := c.st.started.Delegations[q]; ok {
		ins.Delegated, ins.DelegatedAnswer = true, v
	}
	if d, ok := c.dueFor(in, deadlineGateTimeout, c.now()); ok {
		ins.Deadline = d.Due
		ins.RetryAfterSeconds = max(1, int((d.Due.Sub(c.now())+time.Second-1)/time.Second))
	}
	return ins
}

// Answer implements Executor. gate is the qualified state ("finish/Menu"). Of
// several open gates at that state (a Map's children) the oldest instance is
// answered. A value outside the gate's Enum is an error naming the set and
// journals nothing.
func (e *Engine) Answer(run RunID, gate, value string) error {
	return e.AnswerInt(run, gate, value, nil)
}

// AnswerInt answers the oldest matching gate with an optional integer.
func (e *Engine) AnswerInt(run RunID, gate, value string, number *int) error {
	return e.withRun(run, func(c *cycle) error {
		for _, id := range c.st.order {
			in := c.st.insts[id]
			if in.Phase != phaseReady {
				continue
			}
			n := in.node()
			a, ok := gateAnswerer(n)
			if !ok || c.st.qualified(in) != gate {
				continue
			}
			if err := c.checkUntilUnitMutation(in); err != nil {
				return err
			}
			if err := a.answer(c, in, n, value, number); err != nil {
				return err
			}
			return c.drive()
		}
		return fmt.Errorf("%w: run %s has no open gate %q", ErrRejected, run, gate)
	})
}

// AnswerVisit implements Executor. The gate must be open on instance a.Child
// and, when a.Visit is set, on that arrival: a retried answer after the gate
// was reached again, or one meant for another Map child, is ErrRejected.
func (e *Engine) AnswerVisit(run RunID, a GateAnswer) error {
	return e.withRun(run, func(c *cycle) error {
		in := c.st.insts[a.Child]
		if in == nil || in.Phase != phaseReady {
			return fmt.Errorf("%w: no open gate %s on child %q", ErrRejected, a.Gate, a.Child)
		}
		n := in.node()
		ans, ok := gateAnswerer(n)
		if !ok || c.st.qualified(in) != a.Gate {
			return fmt.Errorf("%w: no open gate %s on child %q", ErrRejected, a.Gate, a.Child)
		}
		if a.Visit != 0 && in.Visits[in.Cur] != a.Visit {
			return fmt.Errorf("%w: stale visit %d of gate %s on child %q (now visit %d)", ErrRejected, a.Visit, a.Gate, a.Child, in.Visits[in.Cur])
		}
		if err := c.checkUntilUnitMutation(in); err != nil {
			return err
		}
		if err := ans.answer(c, in, n, a.Value, a.IntValue); err != nil {
			return err
		}
		return c.drive()
	})
}

func gateAnswerer(n *node) (answerer, bool) {
	g, ok := n.kind.(*gateKind)
	if !ok {
		return nil, false
	}
	a, ok := g.ops.(answerer)
	return a, ok
}

// ---------------------------------------------------------------- wait

// waitOps is the typed half of a Wait: the erased ready predicate over T.
type waitOps[S, T any] struct {
	k     *waitKind
	ready func(T) bool
}

// execute re-observes on each Next (once per cycle), takes the ready edge when
// the predicate holds, else the timeout edge once the journaled clock passes
// the arrival plus the timeout. An unavailable read is not met; it waits.
func (o waitOps[S, T]) execute(c *cycle, in *instance, n *node) error {
	if c.polled[in.ID] {
		return nil
	}
	if c.polled == nil {
		c.polled = map[string]bool{}
	}
	c.polled[in.ID] = true
	k := o.k
	d, err := c.scope(in).bindDesc(k.obs)
	if err != nil {
		return c.failErr(in, n, err)
	}
	od, err := c.readObs(in, d, "wait", true)
	if err != nil {
		return err
	}
	if od.Error == "" && !od.Unavailable && o.met(od.Value) {
		if k.readyTo == nil {
			return c.fail(in, n, Blocked, []string{"wait has no ready edge"}, "")
		}
		plan, err := c.planOf(in, *k.readyTo)
		if err != nil {
			return c.halt(in, err.Error())
		}
		return c.emit(in, JournalCompletedByObservation, Accepted, CompletedByObservationData{Output: od.Value, Plan: plan})
	}
	if d, ok := c.dueFor(in, deadlineWaitTimeout, c.now()); ok && !c.now().Before(d.Due) {
		return c.takeTimeout(in, n, k.timeout, "wait")
	}
	return nil
}

func (o waitOps[S, T]) met(raw json.RawMessage) bool {
	var v T
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return o.ready(v)
}

// present is the wait instruction: retry after the poll interval, capped at the
// time left to the timeout.
func (o waitOps[S, T]) present(c *cycle, in *instance, n *node) *Instruction {
	now := c.now()
	var next time.Time
	for _, k := range []deadlineKind{deadlineWaitPoll, deadlineWaitTimeout} {
		if d, ok := c.dueFor(in, k, now); ok && (next.IsZero() || d.Due.Before(next)) {
			next = d.Due
		}
	}
	return c.waitInstruction(in, fmt.Sprintf("Waiting until %s; call next again after the retry-after.", o.k.cond), next.Sub(now))
}

// reservedCoordinatorInstruction ends this coordinator turn at the oldest
// reserved question. The gate stays open; a later answer (including a relayed
// answer from the coordinator) remains valid under the ordinary gate rules.
func (c *cycle) reservedCoordinatorInstruction() (Instruction, bool) {
	if c.e.recordedReplay {
		return Instruction{}, false
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in.Settled || in.Phase != phaseReady || !c.untilUnitAllows(in) || c.st.breakpointBlocks(in) {
			continue
		}
		gate, ok := in.node().kind.(*gateKind)
		if !ok || gate.reservation != Reserved {
			continue
		}
		state := c.st.qualified(in)
		answer := c.commandLine(c.e.opts.AnswerLine(c.run, state))
		answer += fmt.Sprintf(" --child %s --visit %d", sink.Quote(in.ID), in.Visits[in.Cur])
		question := strings.TrimSpace(gate.question)
		if !strings.HasSuffix(question, ".") && !strings.HasSuffix(question, "?") && !strings.HasSuffix(question, "!") {
			question += "."
		}
		prompt := fmt.Sprintf("This run is waiting for a person at %s: %s Choices: %s. Ask the person who started the run and wait for their reply. Answer only with what they tell you; do not choose for them. Run this with their choice as <value>:\n%s", state, question, strings.Join(gate.answers.values, ", "), answer)
		if c.coordinatorAnswers(in) {
			// A fanout fork bound to this gate measures its answer: the
			// coordinator gives it.
			prompt = fmt.Sprintf("This fork measures the answer at %s: %s Choices: %s. Choose the answer yourself, as the person who started the run would. Run this with your choice as <value>:\n%s", state, question, strings.Join(gate.answers.values, ", "), answer)
		}
		// Keep the machine gate fields (including deadline and retry-after) for
		// simulation and JSON consumers; only coordinator delivery changes.
		ins := gate.present(c, in, in.node())
		if ins == nil {
			continue
		}
		prompt += gateIntInstructions(ins.IntValues, answer)
		ins.Kind, ins.Submit, ins.Prompt = InstructionStopped, answer, prompt
		ins.Counts = c.countsLine() // JSON only: next prints the question alone
		return *ins, true
	}
	return Instruction{}, false
}
