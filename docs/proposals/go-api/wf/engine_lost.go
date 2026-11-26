package wf

import (
	"fmt"
	"slices"
)

// lose journals the loss of in's current unit with cause. Below the lost cap
// the fold returns the unit to pending at attempt+1 with no retry spent; at the
// cap (EngineOptions.LostCap losses from one unit state on this visit) it
// journals attempt_lost with LostCauseCap instead, and the state takes its
// failure route as an exhausted retry does, with the reason naming the unit,
// the count and the cause. An attempt with an open refusal (a submission
// refused and not yet fixed) is not lost: it fails as Refused with the
// refusal's reasons and fingerprint and spends a retry. The heartbeat verbs
// and expireTimers call it; nothing else journals attempt_lost.
func (c *cycle) lose(in *instance, cause string) error { return c.loseUnit(in, cause, false) }

// loseUnit is lose; noted is true when the caller journaled the degraded
// record already (loseNoted, a recovery's restart), else lose journals one of
// kind lost (B4) naming what follows.
func (c *cycle) loseUnit(in *instance, cause string, noted bool) error {
	u := in.Unit
	if u == nil || !u.State.live() {
		return nil
	}
	c.killHeadless(u) // a lost attempt's headless child must not work on
	if !noted {
		if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: u.Name, Kind: DegradedLost, Cause: cause, Action: c.lostAction(in, cause)}); err != nil {
			return err
		}
	}
	if in.Refusals > 0 {
		// The child was refused and never fixed its output: its attempt fails
		// as the refusal it left open, spending a retry, not lost for free.
		return c.failSubmitted(in, in.node(), in.LastRefusal, in.RefusalFP)
	}
	name, state := u.Name, u.State.String()
	count := in.Lost[state] + 1
	// An orphan was lost to its coordinator, not by its own fault: it never
	// reaches the cap.
	if count < c.e.opts.LostCap || cause == LostCauseOrphan {
		return c.emit(in, JournalAttemptLost, 0, AttemptLostData{Name: name, Cause: cause})
	}
	reason := fmt.Sprintf("lost cap: %s lost %d times while %s (last: %s)", name, count, state, cause)
	if err := c.emit(in, JournalAttemptLost, 0, AttemptLostData{Name: name, Cause: LostCauseCap, Reason: reason}); err != nil {
		return err
	}
	return c.exhaust(in, in.node(), reason)
}

// finishLostCaps journals the exhaustion of each instance whose lost cap was
// journaled (attempt_lost with LostCauseCap) with no refused record after it:
// a crash between the two records left the decision pending.
func (c *cycle) finishLostCaps() error {
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if in.Settled || in.CapPending == "" {
			continue
		}
		n := in.node()
		if n == nil {
			continue
		}
		if err := c.exhaust(in, n, in.CapPending); err != nil {
			return err
		}
	}
	return nil
}

// loseExpired loses the unit a passed deadline d was listed for, unless an
// earlier arm of this pass already settled or replaced it.
func (c *cycle) loseExpired(in *instance, d deadline, cause string) error {
	if in.Unit == nil || in.Unit.Name != d.Unit {
		return nil
	}
	return c.lose(in, cause)
}

// loseNoted loses in's unit with lostCause after a degraded record of kind
// saying why and what follows: offered again; failed as its open refusal (an
// attempt left refused, see lose); or, at the lost cap, failed.
func (c *cycle) loseNoted(in *instance, kind DegradedKind, cause, lostCause string) error {
	if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: in.Unit.Name, Kind: kind, Cause: cause, Action: c.lostAction(in, lostCause)}); err != nil {
		return err
	}
	return c.loseUnit(in, lostCause, true)
}

// lostAction is what follows the loss of in's unit with lostCause.
func (c *cycle) lostAction(in *instance, lostCause string) string {
	switch {
	case in.Refusals > 0:
		return "failed as its open refusal"
	case lostCause != LostCauseOrphan && in.Lost[in.Unit.State.String()]+1 >= c.e.opts.LostCap:
		if n := in.node(); n != nil && c.recoveryDue(in, n) {
			return "lost cap: offered for recovery"
		}
		return "failed: lost cap"
	}
	return "offered again"
}

// loseForkOrphans is a fork's crash-restart: each unit live at the
// fork point (runState.forkOrphans) still live has its child in the source
// run, never in this one. It is lost with LostCauseFork and offered again at
// its next attempt: never failed as an open refusal (the child that left it
// is not this run's), spending no retry (no turn, if never started), and not
// counted toward the lost cap. A late voter of a settled instance is
// withdrawn, since nothing reissues it. Every cycle of the run makes it, so
// a crash part-way through is finished by the next.
func (c *cycle) loseForkOrphans() error {
	if len(c.st.forkOrphans) == 0 {
		return nil
	}
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if c.st.ended || in == nil || in.Unit == nil || !in.Unit.State.live() || !c.st.forkOrphans[in.Unit.Name] {
			continue
		}
		name := in.Unit.Name
		if in.Settled {
			if err := c.emit(in, JournalWithdrawn, 0, WithdrawnData{Name: name, Reason: "fork: its child ran in the source run"}); err != nil {
				return err
			}
			continue
		}
		cause := fmt.Sprintf("fork: the child of %s ran in the source run, not in this fork", name)
		if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: name, Kind: DegradedOrphan, Cause: cause, Action: "offered again"}); err != nil {
			return err
		}
		if err := c.emit(in, JournalAttemptLost, 0, AttemptLostData{Name: name, Cause: LostCauseFork}); err != nil {
			return err
		}
	}
	return nil
}
