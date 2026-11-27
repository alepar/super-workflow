package wf

import (
	"sort"
	"strings"
	"time"
)

// statusUnits fills the unit fields of rs: every materialised subagent unit in
// view order (stalled, running, started, handed; pending in handOutOrder;
// waiting on dependencies; future; settled, newest first), and the estimates
// read from classStats. Status never sorts pending units or forms an ETA on
// its own: handOutOrder and classStats do.
func (c *cycle) statusUnits(rs *RunStatus, now time.Time) {
	rs.PromptDelivery = c.promptDeliveryStatus()
	stats := c.classStats()
	rs.Started, rs.Now = c.recs[0].At, now

	lastAt := map[string]time.Time{}
	for _, r := range c.recs {
		lastAt[r.Instance] = r.At
	}
	type row struct {
		UnitStatus
		view unitView
		in   *instance
		at   time.Time // settled rows: when the unit settled
	}
	groups := map[UnitState][]row{}
	var pending, waiting, settled []row

	pendingSet := map[*instance]bool{}
	pendingOrder := c.handOutOrder(c.pendingUnits())
	for _, in := range pendingOrder {
		pendingSet[in] = true
	}
	for _, in := range pendingOrder {
		class := c.st.qualified(in)
		u := UnitStatus{Name: c.st.unitName(in), State: UnitPending.String(), Class: class, Attempt: in.Attempt}
		if in.Unit != nil && in.Unit.Predecessor != "" {
			u.Note = c.lostNote(in.Unit.Predecessor)
		}
		if d, ok := stats.estimate(class); ok {
			u.Estimate = d
		}
		pending = append(pending, row{UnitStatus: u, in: in,
			view: unitView{Class: class, State: UnitPending, Since: c.arrivedAt(in), Ready: true}})
	}

	growing := false
	humanGates := 0
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if n := in.node(); n != nil && !in.Settled && !c.st.breakpointBlocks(in) {
			if _, ok := n.kind.(*gateKind); ok && in.Phase == phaseReady {
				humanGates++
			}
			if k, ok := n.kind.(*mapKind); ok && in.Phase != phaseDone {
				growing = growing || k.requery
				for i, note := range k.waiting(c, in, n) {
					class := c.firstAgentClass(in, k.child)
					u := UnitStatus{Name: c.st.qualified(in) + "/waiting#" + itoa(i+1), State: UnitWaiting.String(), Class: class, Note: note}
					if d, ok := stats.estimate(class); ok {
						u.Estimate = d
					}
					waiting = append(waiting, row{UnitStatus: u,
						view: unitView{Class: class, State: UnitPending, Since: c.arrivedAt(in), Held: note != "deps"}})
				}
			}
		}
		u := in.Unit
		if u == nil || pendingSet[in] {
			continue
		}
		class := c.st.qualified(in)
		us := UnitStatus{PromptCoverage: promptCoverage(u), Name: u.Name, State: u.State.String(), Class: class, Attempt: in.Attempt, Observed: u.Observed}
		if u.Claim != nil {
			us.Session, us.Context = u.Claim.Session, u.Claim.Context.display()
		}
		if u.ChildHarness != "" {
			us.Harness = u.ChildHarness
			if hs := headlessOf(u); hs != nil {
				us.Pid = hs.Pid
				if u.State == UnitHanded && !c.e.recordedReplay && c.e.headlessProcess().State(*hs) == HeadlessExited {
					us.StartErr = strings.TrimPrefix(errTailText(hs.ErrLog), "; its stderr ends: ")
					if us.StartErr == "" {
						us.StartErr = "exited before fetching its prompt; its stderr (" + hs.ErrLog + ") is empty"
					}
				}
			}
		}
		if u.Predecessor != "" {
			us.Note = c.lostNote(u.Predecessor)
		}
		view := unitView{Name: u.Name, Class: class, State: u.State, Since: u.StartedAt}
		switch u.State {
		case UnitHanded:
			view.Since = u.HandedAt
			us.Elapsed, us.Window = now.Sub(u.HandedAt), u.HandExpires.Sub(u.HandedAt)
		case UnitStarted, UnitRunning, UnitStalled:
			us.Elapsed = now.Sub(u.StartedAt)
			if u.State == UnitStalled {
				us.Quiet = now.Sub(u.lastSign())
			} else if d, ok := stats.Duration(class); ok {
				us.Estimate, us.Remaining = d, d-us.Elapsed
			}
		case UnitWaiting:
			us.Note = "deps"
			if d, ok := stats.estimate(class); ok {
				us.Estimate = d
			}
			waiting = append(waiting, row{UnitStatus: us, view: unitView{Name: u.Name, Class: class, State: UnitPending, Since: c.arrivedAt(in)}})
			continue
		case UnitHeld:
			if u.Held != nil {
				us.Note = "needs a human: " + u.Held.Cause
			}
		default: // done, failed, lost, withdrawn: listed from the journal below
			continue
		}
		groups[u.State] = append(groups[u.State], row{UnitStatus: us, view: view, in: in})
	}
	for _, su := range c.settledUnits(lastAt) {
		settled = append(settled, row{UnitStatus: su.UnitStatus, view: unitView{Name: su.Name, Class: su.Class, State: UnitDone}, at: su.at})
	}
	sort.SliceStable(settled, func(i, j int) bool { return settled[i].at.After(settled[j].at) })

	// Future agent steps: the unconditional prefix of each leaf instance's path
	// becomes `future` rows; every step ahead, past a branch or not, is counted
	// in Future (and in the ETA) but not in the unit total.
	future := c.futureSteps()
	var futureRows []row
	for _, f := range future {
		rs.Future += f.n
		for _, class := range f.listed {
			u := UnitStatus{Name: class, State: "future", Class: class}
			if d, ok := stats.estimate(class); ok {
				u.Estimate = d
			}
			futureRows = append(futureRows, row{UnitStatus: u})
		}
	}

	var ordered []row
	for _, s := range []UnitState{UnitStalled, UnitRunning, UnitStarted, UnitHanded, UnitHeld} {
		ordered = append(ordered, groups[s]...)
	}
	ordered = append(ordered, pending...)
	ordered = append(ordered, waiting...)
	var views []unitView
	var perUnit [][]string
	byInst := map[*instance][]string{}
	for _, f := range future {
		byInst[f.in] = f.classes()
	}
	for _, r := range append(append([]row(nil), ordered...), settled...) {
		views = append(views, r.view)
		perUnit = append(perUnit, byInst[r.in])
		delete(byInst, r.in)
	}
	for _, f := range future {
		if all, ok := byInst[f.in]; ok {
			perUnit = append(perUnit, all) // a leaf with no unit still has steps ahead
			delete(byInst, f.in)
		}
	}
	ordered = append(ordered, futureRows...)
	ordered = append(ordered, settled...)
	for _, r := range ordered {
		rs.Units = append(rs.Units, r.UnitStatus)
	}

	rs.Progress = stats.Progress(views, growing, rs.Future)
	rs.Binding = stats.Binding(views, humanGates)
	rs.UnitCounts = c.unitCounts()
	if rs.Phase != RunEnded {
		rs.ETA, rs.ETAKnown = stats.ETA(views, perUnit, now)
	}
}

// lostNote names the attempt the predecessor ran, read from the name index.
func (c *cycle) lostNote(predecessor string) string {
	return "(attempt " + itoa(c.st.names[predecessor].Attempt) + " lost)"
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for ; n > 0 || i == len(b); n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// firstAgentClass is the class of a Map child Definition's first state when it
// is an agent, the estimate stand-in for an item that has not spawned yet.
func (c *cycle) firstAgentClass(in *instance, child *Def) string {
	if child == nil || len(child.nodes) == 0 {
		return ""
	}
	if sv := ownStates(child)[0]; sv.Kind == "agent" {
		return strings.Join(append(c.st.defPath(in), child.name, sv.Name), "/")
	}
	return ""
}

// futureOf is what lies ahead of one leaf instance: n is the number of agent
// states reachable from its current state (plus those its ancestors can still
// reach after the state that spawned it, once per ancestor), listed the classes
// of the unconditional prefix. The count is a reachability walk, linear in the
// definition, since Status runs in a fresh process on every CLI call.
type futureOf struct {
	in     *instance
	n      int
	listed []string
}

// classes is the future as ETA input: the listed classes, then nothing:
// the steps past a branch are counted in Future but not estimated.
func (f futureOf) classes() []string {
	return append([]string(nil), f.listed...)
}

// futureSteps computes futureOf for every leaf instance still running: one
// that has not settled, has not ended, and has no unsettled child. An ancestor's
// remainder is counted once, however many children it has.
func (c *cycle) futureSteps() []futureOf {
	hasKid := map[string]bool{}
	for _, id := range c.st.order {
		if ch := c.st.insts[id]; id != "" && !ch.Settled {
			hasKid[ch.Parent] = true
		}
	}
	seen, listedSeen := map[string]bool{}, map[string]bool{}
	var out []futureOf
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in.Settled || in.Terminal != "" || in.Phase == phaseDone || hasKid[id] {
			continue
		}
		f := futureOf{in: in}
		var listing bool
		f.listed, listing = c.unconditionalAgents(in)
		cur := in.Cur
		for ch := in; ch != nil; {
			if ch != in {
				if !seen[ch.ID] {
					seen[ch.ID] = true
					f.n += agentsAhead(ch.def, cur, false, map[*Def]bool{ch.def: true})
				}
				if listing && !listedSeen[ch.ID] {
					listedSeen[ch.ID] = true
					var rest []string
					rest, listing = c.walkFrom(ch.def, strings.Join(c.st.defPath(ch), "/"), cur, false, map[*Def]bool{})
					f.listed = append(f.listed, rest...)
				} else {
					listing = false
				}
			} else {
				f.n += agentsAhead(ch.def, cur, false, map[*Def]bool{ch.def: true})
			}
			if ch.ID == "" {
				break
			}
			cur = ch.Spawner
			ch = c.st.insts[ch.Parent]
		}
		out = append(out, f)
	}
	return out
}

// routeEdges is a state's way-on edges: the ones that are not a fallback. Retry
// exhausted/stalled edges, OnClass edges and Timeout edges are failure paths,
// not choices of the route.
func routeEdges(sv StateView) []EdgeView {
	var out []EdgeView
	for _, e := range sv.Edges {
		if e.Fallback == "" && e.Class == "" && e.Timeout == 0 {
			out = append(out, e)
		}
	}
	return out
}

// unconditionalAgents lists the agent states reached from in's current state
// by following states that have exactly one route edge; a branch (or a Map)
// ends the list, and what lies beyond is only counted. A state with one route
// edge plus fallback edges (Retry exhausted/stalled, OnClass, Timeout) is
// unconditional: the fallbacks are failure paths, counted but not listed. A
// Sub state on the path contributes its child's own unconditional prefix.
// end reports that the walk reached a terminal state without meeting a branch,
// so the spawner's remainder (walkFrom of the parent) is certain to follow.
func (c *cycle) unconditionalAgents(in *instance) (out []string, end bool) {
	return c.walkFrom(in.def, strings.Join(c.st.defPath(in), "/"), in.Cur, false, map[*Def]bool{})
}

// walkFrom is the unconditional agent prefix of d from state start (the start
// itself only when includeStart), classes qualified by prefix. onPath guards a
// definition already being walked, as computeDepths does; visited keeps a loop
// inside one definition linear.
func (c *cycle) walkFrom(d *Def, prefix, start string, includeStart bool, onPath map[*Def]bool) (out []string, end bool) {
	states := ownStates(d)
	byName := make(map[string]StateView, len(states))
	for _, sv := range states {
		byName[sv.Name] = sv
	}
	cur, ok := byName[start]
	if !ok {
		return nil, false
	}
	onPath[d] = true
	defer delete(onPath, d)
	qualify := func(p, name string) string {
		if p == "" {
			return name
		}
		return p + "/" + name
	}
	// visit adds what a state contributes and reports whether the walk goes on.
	visit := func(sv StateView) bool {
		switch {
		case sv.Kind == "agent":
			out = append(out, qualify(prefix, sv.Name))
		case sv.Kind == "map":
			return false // the item count is not known in advance
		case sv.child != nil:
			if len(sv.child.nodes) == 0 || onPath[sv.child] {
				return false
			}
			sub, reached := c.walkFrom(sv.child, qualify(prefix, sv.child.name), sv.child.nodes[0].name, true, onPath)
			out = append(out, sub...)
			return reached
		}
		return true
	}
	visited := map[string]bool{start: true}
	if includeStart && !visit(cur) {
		return out, false
	}
	for {
		if cur.Kind == "terminal" {
			return out, true
		}
		es := routeEdges(cur)
		if len(es) != 1 {
			return out, false
		}
		next, ok := byName[es[0].To]
		if !ok || visited[next.Name] {
			return out, false
		}
		visited[next.Name] = true
		if !visit(next) {
			return out, false
		}
		cur = next
	}
}

// settledUnit is a unit that is no longer live: its attempt ended.
type settledUnit struct {
	UnitStatus
	at time.Time
}

// settledUnits lists every handed-out unit that is no longer live, newest
// first by the caller. The fold keeps only an instance's current unit, so the
// settled ones are read back from the journal: a unit ends with the record that
// settles its instance's open attempt (accepted, refused, the transition that
// follows, a loss, a withdrawal or a late vote). A unit still held by its
// instance in a terminal state takes that state instead.
func (c *cycle) settledUnits(lastAt map[string]time.Time) []settledUnit {
	delivery := map[string]string{}
	for _, pd := range c.promptDeliveryStatus() {
		delivery[pd.Unit] = pd.Coverage
	}
	type info struct {
		inst          string
		attempt       int
		started, end  time.Time
		state         UnitState
		session, note string
		harness       string // the child's harness when not the coordinator's
		context       ContextMode
		settled       bool
		order         int
	}
	byName := map[string]*info{}
	var names []string
	cur := map[string]string{} // instance -> the name of its open unit
	end := func(inst string, r JournalRecord, state UnitState) {
		n := cur[inst]
		if i := byName[n]; i != nil && !i.settled {
			i.settled, i.end, i.state = true, r.At, state
		}
		delete(cur, inst)
	}
	for _, r := range c.recs {
		switch r.Kind {
		case JournalHandedOut, JournalAttemptStarted, JournalMetadataAttached, JournalAttemptLost, JournalWithdrawn, JournalLateVote:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			switch d := d.(type) {
			case *HandedOutData:
				if i := byName[d.Name]; i != nil { // a re-offer of the same unit
					i.harness = d.childHarness()
					continue
				}
				ref := c.st.names[d.Name]
				byName[d.Name] = &info{inst: r.Instance, attempt: ref.Attempt, order: len(names), harness: d.childHarness()}
				names = append(names, d.Name)
				cur[r.Instance] = d.Name
			case *AttemptStartedData:
				if i := byName[d.Name]; i != nil {
					if i.started.IsZero() {
						i.started = r.At
					}
					if d.Session != "" {
						i.session = d.Session
					}
					if d.Context != "" {
						i.context = d.Context
					}
				}
			case *MetadataAttachedData:
				if i := byName[d.Name]; i != nil {
					if d.Session != "" {
						i.session = d.Session
					}
					if d.Context != "" {
						i.context = d.Context
					}
				}
			case *AttemptLostData:
				if i := byName[d.Name]; i != nil && !i.settled {
					i.settled, i.end, i.state, i.note = true, r.At, UnitLost, c.lostNote(d.Name)
					if d.Cause == LostCauseCap {
						i.state = UnitFailed
					}
				}
				delete(cur, r.Instance)
			case *WithdrawnData:
				if byName[d.Name] == nil { // withdrawn before it was ever handed out
					byName[d.Name] = &info{inst: r.Instance, attempt: c.st.names[d.Name].Attempt, order: len(names)}
					names = append(names, d.Name)
				}
				if i := byName[d.Name]; i != nil && !i.settled {
					i.settled, i.end, i.state, i.note = true, r.At, UnitWithdrawn, d.Reason
				}
				delete(cur, r.Instance)
			case *LateVoteData:
				if i := byName[d.Name]; i != nil && !i.settled {
					i.settled, i.end, i.state = true, r.At, UnitDone
				}
			}
		case JournalAccepted, JournalCompletedByObservation, JournalTransition:
			end(r.Instance, r, UnitDone)
		case JournalRefused:
			end(r.Instance, r, UnitFailed)
		}
	}
	var out []settledUnit
	for _, name := range names {
		i := byName[name]
		in := c.st.insts[i.inst]
		if in != nil && in.Unit != nil && in.Unit.Name == name {
			if !in.Unit.State.terminal() {
				continue // still live
			}
			i.state = in.Unit.State
			if !i.settled {
				i.end = lastAt[i.inst]
			}
		} else if !i.settled {
			continue // a unit with no known end stays out of the settled list
		}
		if in != nil && i.note == "" && in.Settled && in.halted() {
			i.note = in.Reason // the halt its spawner absorbed
		}
		u := UnitStatus{Name: name, State: i.state.String(), Class: c.st.classOf(c.st.names[name]), Attempt: i.attempt, Session: i.session, Note: i.note, Harness: i.harness}
		u.PromptCoverage = delivery[name]
		if !i.started.IsZero() {
			u.Context = i.context.display()
		}
		if !i.started.IsZero() && !i.end.Before(i.started) {
			u.Elapsed = i.end.Sub(i.started)
		}
		out = append(out, settledUnit{UnitStatus: u, at: i.end})
	}
	return out
}

// agentsAhead counts the agent states reachable from state start of d (start
// itself only when includeStart), descending into Sub and Map child
// definitions; inDefs guards a definition already being walked.
func agentsAhead(d *Def, start string, includeStart bool, inDefs map[*Def]bool) int {
	states := ownStates(d)
	byName := make(map[string]StateView, len(states))
	for _, sv := range states {
		byName[sv.Name] = sv
	}
	first, ok := byName[start]
	if !ok {
		return 0
	}
	n := 0
	visited := map[string]bool{start: true}
	visit := func(sv StateView) {
		if sv.Kind == "agent" {
			n++
		}
		if sv.child != nil && len(sv.child.nodes) > 0 && !inDefs[sv.child] {
			inDefs[sv.child] = true
			n += agentsAhead(sv.child, sv.child.nodes[0].name, true, inDefs)
			delete(inDefs, sv.child)
		}
	}
	if includeStart {
		visit(first)
	}
	queue := []StateView{first}
	for len(queue) > 0 {
		sv := queue[0]
		queue = queue[1:]
		for _, e := range sv.Edges {
			if next, ok := byName[e.To]; ok && !visited[e.To] {
				visited[e.To] = true
				visit(next)
				queue = append(queue, next)
			}
		}
	}
	return n
}

// statusReasons fills the reason fields of rs: the deadlines, the open effect
// intents, the previous lock holder and, for a run with no agent work to hand
// out, why. It journals nothing and never drives.
func (c *cycle) statusReasons(rs *RunStatus, now time.Time) {
	for _, d := range c.deadlines(now) {
		rs.Due = append(rs.Due, DueTime{Kind: d.Kind.String(), Instance: d.Instance, Unit: d.Unit, Due: d.Due})
	}
	sort.SliceStable(rs.Due, func(i, j int) bool { return rs.Due[i].Due.Before(rs.Due[j].Due) })

	blocked := ""
	if h, ok := c.heldAt(); ok {
		blocked = "blocked: " + c.st.qualified(h) + " " + h.Reason
	}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in.Phase == phaseHalted {
			continue
		}
		if seq, e, ok := openIntent(in); ok {
			oi := OpenIntent{Seq: seq, Instance: id,
				Effect: e.Adapter + "." + e.Op, Detached: in.Cmd != nil && in.Cmd.Started != nil}
			if cmd := in.Cmd; cmd != nil && in.Pending == nil {
				if start, max := cmd.detachedClock(); max > 0 {
					oi.Due = start.Add(max)
				}
				if r := cmd.Rerun; r != nil {
					oi.Reason, oi.RerunOf = r.Reason, r.Of
					if cmd.Started == nil {
						oi.WaitGroup = r.Pgid
					}
				}
			}
			rs.OpenIntents = append(rs.OpenIntents, oi)
		}
	}
	// Status reads the run lock-free (withReadOnly): the record in the lock
	// file is its latest holder. Only a journal with no such record falls
	// back to the holder it found when it last took the lock itself.
	if h, ok := c.e.j.(interface {
		recordedHolder(RunID) (LockHolder, bool)
	}); ok {
		if lh, ok := h.recordedHolder(c.run); ok {
			rs.LockHolder = &lh
		}
	}
	if h, ok := c.e.j.(interface {
		lastHolder(RunID) (LockHolder, bool)
	}); ok && rs.LockHolder == nil {
		if lh, ok := h.lastHolder(c.run); ok {
			rs.LockHolder = &lh
		}
	}

	if rs.Breakpoint != nil {
		rs.NothingReadyReason = "waiting for the run's operator to resume"
		return
	}
	if rs.BudgetStop != "" {
		rs.NothingReadyReason = "stopped on its run budget (" + rs.BudgetStop + "): the run's operator, not its coordinator, raises it with: " + rs.BudgetResume
		return
	}
	if rs.Phase == RunEnded || len(c.pendingUnits()) > 0 {
		return
	}
	if blocked != "" {
		rs.NothingReadyReason = blocked
		return
	}
	if held := c.heldUnits(); len(held) > 0 {
		rs.NothingReadyReason = "waiting on human: unit " + held[0].Name + " held: " + held[0].Cause
		return
	}
	for _, id := range c.st.order {
		if ins, ok := c.nonAgentInstruction(c.st.insts[id]); ok {
			switch ins.Kind {
			case InstructionHumanGate:
				rs.NothingReadyReason = "waiting on human: gate " + ins.State
				return
			case InstructionManualVerification:
				if why := c.effectInFlight(c.st.insts[id], ins.Effect); why != "" {
					rs.NothingReadyReason = why
				} else if c.recovered(c.st.insts[id]) {
					verdict := "asks for your verdict"
					if ins.Person {
						verdict = "asks for a person's verdict"
					}
					rs.NothingReadyReason = "effect " + itoa(int(ins.Effect)) + " is recovered by the next call, which checks it, runs it again, or " + verdict +
						": run next once and follow what it returns; a later status does not show what it decided"
				} else if ins.Person {
					rs.NothingReadyReason = "waiting on human: verify effect " + itoa(int(ins.Effect))
				} else {
					rs.NothingReadyReason = "waiting on the coordinator: settle effect " + itoa(int(ins.Effect))
				}
				return
			}
		}
	}
	running, stalled := 0, ""
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if u := in.Unit; u != nil && in.Phase == phaseIssued && !in.Settled { // a unit in backoff is waiting on the clock
			switch u.State {
			case UnitStalled:
				if stalled == "" {
					stalled = u.Name
				}
				running++
			case UnitHanded, UnitStarted, UnitRunning:
				running++
			}
		}
	}
	switch {
	case stalled != "":
		// Only next asks whether its child still runs, and nothing moves
		// until that is answered: not a unit to wait on.
		rs.NothingReadyReason = "unit " + stalled + " is stalled: run next, which asks whether its child is still running"
	case running > 0:
		rs.NothingReadyReason = "waiting on " + itoa(running) + " running units"
	case len(rs.Due) > 0:
		rs.NothingReadyReason = "waiting until " + rs.Due[0].Due.UTC().Format(time.RFC3339) + " (" + rs.Due[0].Kind + ")"
	case c.stepReady():
		rs.NothingReadyReason = "a step is ready to advance on the next call"
	default:
		rs.NothingReadyReason = "nothing to do"
	}
}

// stepReady reports whether some live instance is at a state the next drive
// will run (an agent, compute, Map or Sub that no non-agent instruction covers).
func (c *cycle) stepReady() bool {
	for _, id := range c.st.order {
		if in := c.st.insts[id]; in.Phase == phaseReady && !in.Settled {
			return true
		}
	}
	return false
}

// recovered reports whether the next call may recover in's open intent
// before it asks for a settlement: a keyed edge effect is checked
// (recoverEdgeIntent); a command with a DoneWhen, an Idempotent mark or a key
// is observed, run again or checked (recover); a detached command with no
// launch past its start grace is adopted, launched again or recovered
// (resume). A journaled launch is the runner's (effectInFlight). Status runs
// none of these itself, and an unknown answer journals nothing, so it cannot
// tell whether next already asked: it names the outcomes and sends the caller
// to one next, whose instruction is the answer.
func (c *cycle) recovered(in *instance) bool {
	switch {
	case in.Pending != nil && in.Pending.Open != 0:
		return !in.Pending.OpenEffect.NonIdempotent
	case in.Cmd != nil && in.Cmd.Result == nil:
		if c.e.opts.EffectRunner != nil && c.resultDir() != "" && (in.Cmd.Started != nil || in.Cmd.Max > 0) {
			// effectInFlight found no launch inside the grace: resume recovers
			// one with none (a journaled launch whose runner cannot be read is
			// the poll's, which settles nothing).
			return in.Cmd.Started == nil
		}
		n := in.node()
		if n == nil {
			return false
		}
		k, ok := n.kind.(*commandKind)
		return n.doneWhen != nil || ok && (k.idempotent || len(k.key) > 0)
	}
	return false
}

// effectInFlight is the status reason for an open effect that may still be
// carried out, "" when nothing says so and it is the coordinator's (or a
// person's) to settle. Status reads the run lock-free, so an open
// intent alone cannot tell in-flight from uncertain. A detached command asks
// its runner, by its effect_started or, when that append was lost, by the
// proc and result files as next does (adopt): running within its maximum is
// a wait; past it, or ended, only a
// driving call stops or records it, so the caller runs next. Otherwise a
// hold on the run lock, read from the kernel's lock table, says another call
// may be applying it: that is not proof (a reader holds it too), so the
// reason says "may", and next, which waits its turn, then shows what the
// effect needs.
func (c *cycle) effectInFlight(in *instance, seq int64) string {
	n := "effect " + itoa(int(seq))
	if cmd := in.Cmd; cmd != nil && cmd.Result == nil && c.e.opts.EffectRunner != nil && c.resultDir() != "" && (cmd.Started != nil || cmd.Max > 0) {
		// Read-only, as next reads it: the journaled launch, else the proc
		// and result files (a launch whose effect_started was lost).
		runner := c.e.opts.EffectRunner
		var p DetachedProcess
		var req DetachedRequest
		launched := cmd.Started != nil
		var err error
		if launched {
			p, req = processOf(cmd.Started), c.effectPaths(cmd.Started.Intent)
		} else {
			req = c.effectPaths(cmd.Seq)
			p, launched, err = runner.Launched(req)
		}
		var rep DetachedReport
		if err == nil {
			rep, err = runner.Status(p, req)
		}
		switch {
		case err != nil:
			// next reads it again and settles nothing on a read error.
			return n + " cannot be read now: run next, which reads it again"
		case rep.Status == DetachedPresent:
			return n + " has ended: run next to record it"
		case launched && rep.Status == DetachedRunning:
			if start, max := cmd.detachedClock(); max > 0 && !c.now().Before(start.Add(max)) {
				return n + " is past its maximum duration: run next to stop it"
			}
			return n + " is running, wait"
		case launched:
			return n + " has ended: run next to record it"
		case c.now().Before(cmd.At.Add(startGrace)):
			return n + " is starting, wait"
		}
	}
	if !c.lockFree {
		return "" // this cycle holds the lock itself: no other call does
	}
	if h, ok := c.e.j.(interface{ heldNow(RunID) (bool, bool) }); ok {
		switch held, known := h.heldNow(c.run); {
		case held:
			return n + " may be running: another call holds the run; run next, which waits for it"
		case !known:
			return n + " may be running: the run lock's holder cannot be read; run next, which waits for it"
		}
	}
	return ""
}
