package wf

import (
	"fmt"
	"time"
)

// deadlineKind is the closed set of due times the clock decides.
type deadlineKind int

// Deadline kinds.
const (
	deadlineGateTimeout  deadlineKind = iota + 1 // a ready gate's Timeout edge
	deadlineWaitTimeout                          // a ready wait's Timeout edge
	deadlineWaitPoll                             // a ready wait's next re-observation
	deadlineBackoff                              // a backoff's due time
	deadlineLease                                // a claimed attempt's lease (attempt_started, heartbeat, or an old lease_claimed), or a fetched unit's implicit lease
	deadlineHandOut                              // an unclaimed hand-out's re-offer: idle (no session, no unit claimed) or UnstartedGrace (its session's)
	deadlineSpawn                                // a claimed, never-fetched unit's spawn window
	deadlineStall                                // a running unit's last sign of life plus the class threshold
	deadlineEffectPoll                           // a running detached effect's next result-file poll
	deadlineEffectMax                            // a running detached effect's maximum duration
	deadlineEndSettle                            // a claimed unit's end settle: its transcript read EndIfSettled and loseEndedChildren looks again then
	deadlineStallRecheck                         // a stalled unit's next stall question: its report plus a whole number of lease lengths
)

var deadlineKindNames = [...]string{
	deadlineGateTimeout:  "gate timeout",
	deadlineWaitTimeout:  "wait timeout",
	deadlineWaitPoll:     "wait poll",
	deadlineBackoff:      "backoff",
	deadlineLease:        "lease",
	deadlineHandOut:      "hand-out",
	deadlineSpawn:        "spawn",
	deadlineStall:        "stall",
	deadlineEffectPoll:   "effect poll",
	deadlineEffectMax:    "effect max",
	deadlineEndSettle:    "end settle",
	deadlineStallRecheck: "stall recheck",
}

// String returns the lower-case kind name.
func (k deadlineKind) String() string {
	if k >= deadlineGateTimeout && int(k) < len(deadlineKindNames) {
		return deadlineKindNames[k]
	}
	return fmt.Sprintf("deadline(%d)", int(k))
}

// deadline is one due time of a folded run.
type deadline struct {
	Kind     deadlineKind
	Instance string    // the instance it belongs to
	Unit     string    // the unit name, for the unit kinds
	Intent   int64     // the effect intent, for the effect kinds
	Due      time.Time // when it falls due
}

// deadlines lists every due time the clock decides in the folded run, in
// instance order, then kind order.
//
// Owns: the deadline kinds and when each falls due, computed from the
// journaled stamps (JournalRecord.At through arrivedAt, the unit's stamps,
// Backoff.Due, effect_started), the EngineOptions windows and the declared
// timeouts. A stall is the last sign of life (start, or the latest heartbeat,
// which a transcript write journals) plus the class threshold; it has no
// deadline without a transcript, or with one that cannot be statted.
// Does not: append records or sleep. drive journals the deadlines that have
// passed (expireTimers), Wait sleeps until the earliest one and Status lists
// them, each through one switch over deadlineKind.
// Extend by: a new clock-driven expiry is one deadlineKind value, one case
// here and one arm in each switch over the kind; callers never compute a
// timeout or a window themselves. A stalled unit has no lease deadline; a
// fetched unit's lease expiry is a stall report (leaseExpired), never a loss.
// No claimed unit is without a liveness bound: a never-fetched one has its
// spawn window, a watched one its stall deadline, a leased one its lease, and
// any other fetched one an implicit lease (implicitLease).
func (c *cycle) deadlines(now time.Time) []deadline {
	var out []deadline
	for _, id := range c.st.order {
		out = append(out, c.instanceDeadlines(c.st.insts[id], now)...)
	}
	return out
}

// dueFor is in's deadline of kind k, if it has one.
func (c *cycle) dueFor(in *instance, k deadlineKind, now time.Time) (deadline, bool) {
	for _, d := range c.instanceDeadlines(in, now) {
		if d.Kind == k {
			return d, true
		}
	}
	return deadline{}, false
}

// detachedClock is where an open detached intent's clocks run from: the
// intent's At and its decided Max, else (a record from before the intent
// carried them) the legacy effect_started's wall Start and Max. The zero time
// means the intent is not detached.
func (cmd *cmdIntent) detachedClock() (start time.Time, max time.Duration) {
	switch s := cmd.Started; {
	case cmd.Max > 0:
		return cmd.At, cmd.Max
	case s != nil && !s.Start.IsZero():
		return s.Start, s.Max
	}
	return time.Time{}, 0
}

// nextTick is the first start+k*every (k >= 1) that is after now.
func nextTick(start time.Time, every time.Duration, now time.Time) time.Time {
	k := time.Duration(1)
	if el := now.Sub(start); el >= 0 {
		k = el/every + 1
	}
	return start.Add(k * every)
}

// instanceDeadlines lists in's deadlines in kind order.
func (c *cycle) instanceDeadlines(in *instance, now time.Time) []deadline {
	var out []deadline
	add := func(k deadlineKind, due time.Time) {
		out = append(out, deadline{Kind: k, Instance: in.ID, Due: due})
	}
	switch in.Phase {
	case phaseReady:
		n := in.node()
		if n == nil || c.st.breakpointBlocks(in) {
			break
		}
		switch k := n.kind.(type) {
		case *gateKind:
			if k.timeout != nil {
				add(deadlineGateTimeout, c.arrivedAt(in).Add(k.timeout.e.timeout))
			}
		case *waitKind:
			arrived := c.arrivedAt(in)
			if k.timeout != nil {
				add(deadlineWaitTimeout, arrived.Add(k.timeout.e.timeout))
			}
			poll := k.poll
			if poll <= 0 {
				poll = defaultPoll
			}
			add(deadlineWaitPoll, nextTick(arrived, poll, now))
		}
	case phaseBackoff:
		if in.Backoff != nil {
			add(deadlineBackoff, in.Backoff.Due)
		}
	}
	if u := in.Unit; u != nil && !in.Settled { // a settled child's late voter has no expiry: nothing reissues it
		unitDue := func(k deadlineKind, due time.Time) {
			out = append(out, deadline{Kind: k, Instance: in.ID, Unit: u.Name, Due: due})
		}
		// A stalled unit has no lease deadline: its lease expiry was a stall
		// report, the coordinator's question (leaseExpired), and listing it
		// again would be due on every drive. A heartbeat active (or a write
		// to a watched transcript) answers it and renews the lease.
		if u.State.claimed() && u.State != UnitStalled {
			switch {
			case !u.LeaseExpires.IsZero():
				unitDue(deadlineLease, u.LeaseExpires)
			case c.implicitLease(u):
				unitDue(deadlineLease, u.liveAt().Add(c.leaseLength(in)))
			}
		}
		if u.State == UnitRunning && !u.FetchedAt.IsZero() && c.watched(u) {
			unitDue(deadlineStall, u.lastSign().Add(c.stallThreshold(in)))
		}
		if due, ok := c.stallRecheck(in); ok {
			unitDue(deadlineStallRecheck, due)
		}
		if c.endWatched(in) {
			c.e.mu.Lock()
			p, ok := c.e.endPending[u.Claim.Transcript]
			c.e.mu.Unlock()
			if ok {
				unitDue(deadlineEndSettle, p.due)
			}
		}
		switch u.State {
		case UnitHanded:
			if due, ok := c.idleReoffer(u); ok && u.HandSession == "" {
				unitDue(deadlineHandOut, due)
			} else if due, ok := c.staleHandOutDue(u); ok {
				unitDue(deadlineHandOut, due)
			} else if u.HandSession != "" {
				// Its coordinator's next offers it again then (unstartedCause):
				// a blocking next wakes for it.
				unitDue(deadlineHandOut, u.HandedAt.Add(c.e.opts.UnstartedGrace))
			}
		case UnitStarted, UnitRunning, UnitStalled:
			// A prompt fetch is the spawn signal: a fetched unit lives by its
			// lease (the Map's, else the implicit one), or by its watched
			// transcript. Only a unit claimed by started and never fetched
			// keeps the spawn window.
			if !u.waveClaim() && u.FetchedAt.IsZero() {
				unitDue(deadlineSpawn, u.StartedAt.Add(c.e.opts.SpawnWindow))
			}
		}
	} else if in.Phase == phaseIssued && in.Lease != nil {
		add(deadlineLease, in.Lease.Expires)
	}
	if cmd := in.Cmd; cmd != nil && cmd.Result == nil {
		start, max := cmd.detachedClock()
		if every := c.e.opts.EffectPoll; every > 0 && !start.IsZero() {
			out = append(out, deadline{Kind: deadlineEffectPoll, Instance: in.ID, Intent: cmd.Seq, Due: nextTick(start, every, now)})
		}
		if max > 0 {
			out = append(out, deadline{Kind: deadlineEffectMax, Instance: in.ID, Intent: cmd.Seq, Due: start.Add(max)})
		}
	}
	return out
}

// implicitLease reports whether u, a claimed unit, lives by the implicit lease:
// it fetched its prompt (so it has no spawn window), it has no Map lease and
// the engine cannot watch a transcript for it. Its lease deadline is
// leaseLength past its last sign of life (liveAt): then the coordinator is
// asked whether its child still runs (leaseExpired), and a started, a
// heartbeat, a re-fetch or a validate renews it. FetchedLease is deliberately much longer than
// SpawnWindow: a slow serial spawner's started routinely trails the child's
// prompt fetch by minutes, and a drive in between must not lose the unit.
// Wave claims are the coordinator's own and have none.
func (c *cycle) implicitLease(u *unit) bool {
	return !u.FetchedAt.IsZero() && u.LeaseExpires.IsZero() && !u.waveClaim() && !c.watched(u)
}
