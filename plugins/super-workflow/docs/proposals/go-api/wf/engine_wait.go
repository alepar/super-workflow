package wf

import (
	"errors"
	"slices"
	"time"
)

// defaultWaitTimeout is how long Wait blocks when its caller passes none.
const defaultWaitTimeout = 540 * time.Second

// waitLockRetry is how long Wait pauses before it tries a locked run again.
const waitLockRetry = 100 * time.Millisecond

// Wait implements Executor: it cycles lock, fold, drive and collect until an
// event appears, sleeping without the lock between cycles until the earliest
// deadline, the journal's change signal or the timeout.
func (e *Engine) Wait(run RunID, since int64, timeout time.Duration) (WaitResult, error) {
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	// The change signal is subscribed inside each locked cycle, after its own
	// appends: the baseline of a sleep is the journal as the cycle left it, so
	// the cycle's own records never wake it and an append by another writer
	// after the unlock does. stop is the current subscription's.
	var stop chan struct{}
	defer func() {
		if stop != nil {
			close(stop)
		}
	}()
	limit := time.NewTimer(timeout)
	defer limit.Stop()

	baseline := since
	// cursor: the caller carries the seq of its previous wait. An expired
	// hand-out it was already shown then stays behind it. A held run still
	// reports blocked on the first cycle: the loop stops there for a human.
	cursor := since >= 0
	first := true
	// Until a cycle runs, a timeout reports the caller's own cursor: -1 (the
	// default baseline) when it passed none, never 0, which would replay the
	// whole journal to a follower that carries it.
	last := WaitResult{Seq: since}
	for {
		// The timeout is checked before each cycle but the first: an
		// always-ready change signal must not keep the loop driving past it,
		// and the first cycle always runs, so even an immediate timeout
		// reports the journal's seq (unless the run stays locked: then the
		// caller's own cursor). A cycle that has started runs to its end.
		if !first {
			select {
			case <-limit.C:
				last.Events = []Event{{Kind: EventTimeout}}
				return last, nil
			default:
			}
		}
		var res WaitResult
		var next, now time.Time
		var changed <-chan struct{}
		err := e.withRun(run, func(c *cycle) error {
			now = c.now()
			if baseline < 0 {
				baseline = lastHandOut(c.recs)
			}
			due := c.dueDeadlines(now)
			if err := c.drive(); err != nil {
				return err
			}
			now = c.now()
			evs := c.events(baseline, cursor, due)
			if c.st.ended && !slices.ContainsFunc(evs, func(ev Event) bool { return ev.Kind == EventRunEnded }) {
				evs = append(evs, Event{Kind: EventRunEnded, State: c.st.term, Detail: string(c.st.outcome)})
			}
			if b := c.st.breakpoint; b != nil && first && !slices.ContainsFunc(evs, func(ev Event) bool { return ev.Kind == EventBreakpoint && ev.Seq == b.Seq }) {
				evs = append(evs, Event{Kind: EventBreakpoint, State: b.State, Detail: b.Reason, Seq: b.Seq})
			}
			if h, ok := c.heldAt(); ok && first && !slices.ContainsFunc(evs, func(ev Event) bool { return ev.Kind == EventBlocked }) {
				// The halt was journaled at or before the baseline (an earlier
				// next or a coordinator): a held run returns at once.
				evs = append(evs, Event{Kind: EventBlocked, State: c.st.qualified(h), Detail: h.Reason})
			} else if c.waitingOnHolds() && !slices.ContainsFunc(evs, func(ev Event) bool { return ev.Kind == EventBlocked }) {
				// Only holds remain: the run waits on a human, so wait returns
				// with the first held unit (next lists every one). A hold
				// journals no halt, so this is the only blocked event it gets.
				h := c.heldUnits()[0]
				evs = append(evs, Event{Kind: EventBlocked, Name: h.Name, State: c.st.qualified(c.st.insts[h.Child]), Detail: "held: " + h.Cause})
			}
			first = false
			res = c.waitCounts()
			res.Events = evs
			res.Seq = int64(len(c.recs))
			for _, d := range c.deadlines(now) {
				if d.Due.After(now) && (next.IsZero() || d.Due.Before(next)) {
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
		if errors.Is(err, ErrRunLocked) {
			// A writer (a child's done, another verb) holds the run past the
			// journal's own bounded retry. Wait only reads and drives, so it
			// waits its turn instead of failing, until its own timeout.
			select {
			case <-limit.C:
				last.Events = []Event{{Kind: EventTimeout}}
				return last, nil
			case <-time.After(waitLockRetry):
			}
			continue
		}
		if err != nil {
			return WaitResult{}, err
		}
		if len(res.Events) > 0 {
			return res, nil
		}
		last = res
		// Sleep without the lock. A deadline is on the engine clock, so wake
		// just after it; the loop re-reads the clock.
		var wake <-chan time.Time
		if !next.IsZero() {
			t := time.NewTimer(next.Sub(now) + time.Millisecond)
			defer t.Stop()
			wake = t.C
		}
		select {
		case <-changed:
		case <-wake:
		case <-limit.C:
			res.Events = []Event{{Kind: EventTimeout}}
			return res, nil
		}
	}
}

// lastHandOut is the Seq of the run's last handed_out or attempt_started
// record, 0 when there is none: the baseline of a Wait that passed no since.
func lastHandOut(recs []JournalRecord) int64 {
	for i := len(recs) - 1; i >= 0; i-- {
		if k := recs[i].Kind; k == JournalHandedOut || k == JournalAttemptStarted {
			return recs[i].Seq
		}
	}
	return 0
}

// waitCounts fills the counts of a WaitResult: the units ready, handed and
// running, and what binds the run.
func (c *cycle) waitCounts() WaitResult {
	res := WaitResult{UnitCounts: c.unitCounts()}
	var units []unitView
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if u := in.Unit; u != nil && u.State.claimed() {
			units = append(units, unitView{Name: u.Name, Class: c.st.qualified(in), State: u.State, Since: u.StartedAt})
		}
	}
	// An expired hand-out waits for a re-offer, as a pending unit waits for
	// its first offer.
	for _, in := range append(c.expiredHandOuts(), c.pendingUnits()...) {
		units = append(units, unitView{Class: c.st.qualified(in), State: UnitPending, Since: c.arrivedAt(in), Ready: true})
	}
	humanGates := 0
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if in.Phase != phaseReady || in.Settled {
			continue
		}
		if n := in.node(); n != nil {
			if _, ok := n.kind.(*gateKind); ok {
				humanGates++
			}
		}
	}
	res.Binding = c.classStats().Binding(units, humanGates)
	return res
}
