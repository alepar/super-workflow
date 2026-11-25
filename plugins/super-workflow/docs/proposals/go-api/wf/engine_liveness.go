package wf

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// classStats is the estimates folded from this cycle's records, folded again
// only when a record was appended since.
func (c *cycle) classStats() *classStats {
	if c.stats == nil || c.statsAt != len(c.recs) {
		c.stats = foldStats(c.recs, c.st, statsConfig{StallDefault: c.e.opts.StallDefault})
		c.statsAt = len(c.recs)
	}
	return c.stats
}

// lastSign is when the unit last showed life: its start or its last heartbeat.
func (u *unit) lastSign() time.Time {
	if u.BeatAt.After(u.StartedAt) {
		return u.BeatAt
	}
	return u.StartedAt
}

// liveAt is the unit's latest sign of life of any kind: its claim, a
// heartbeat, a prompt fetch or a later started. An implicit lease runs from it.
func (u *unit) liveAt() time.Time {
	t := u.lastSign()
	for _, s := range []time.Time{u.FetchedAt, u.RestartedAt} {
		if s.After(t) {
			t = s
		}
	}
	return t
}

// sampleTranscripts stats the transcript of every started, running or stalled
// unit that has one, and journals a heartbeat (active, by transcript) for each
// written since the unit's last sign of life. That clears a stall and renews a
// leased unit's lease.
//
// The heartbeat is debounced: a write is journaled only when it is at least a
// quarter of the unit's window past the last sign, the window being the stall
// threshold, lowered to the Map lease when that is shorter. A busy transcript
// then costs at most four records per window instead of one per drive. A write
// inside the margin is not lost: when the unit's stall or lease deadline is due
// the margin is zero, so the write is journaled first and postpones the
// deadline. The heartbeat carries the write's own mtime (never later than now),
// so a deferred write moves the deadlines only as far as the write itself. A
// path that cannot be statted is marked unstatable for this cycle, so it has no
// stall deadline; a path that does not exist yet means no write yet. The fold
// never stats anything: this runs in drive, before the timers, so a unit whose
// transcript is being written is neither stalled nor lost to its lease.
func (c *cycle) sampleTranscripts() error {
	c.unstatable = nil
	due := map[string]bool{}
	now := c.now()
	for _, d := range c.deadlines(now) {
		if (d.Kind == deadlineStall || d.Kind == deadlineLease) && d.Unit != "" && !now.Before(d.Due) {
			due[d.Unit] = true
		}
	}
	for _, id := range append([]string(nil), c.st.order...) {
		in := c.st.insts[id]
		u := in.Unit
		if !c.untilUnitAllows(in) || u == nil || in.Settled || !u.State.claimed() || u.Claim == nil || u.Claim.Transcript == "" {
			continue
		}
		mt, err := c.e.opts.TranscriptMtime(u.Claim.Transcript)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			if c.unstatable == nil {
				c.unstatable = map[string]bool{}
			}
			c.unstatable[u.Name] = true
			continue
		}
		window := c.stallThreshold(in)
		if lease := c.mapLeaseOf(in); lease > 0 && lease < window {
			window = lease
		}
		margin := window / 4
		if due[u.Name] {
			margin = 0
		}
		if !mt.After(u.lastSign()) || mt.Sub(u.lastSign()) < margin {
			continue
		}
		seen := mt
		if seen.After(now) {
			seen = now
		}
		d := HeartbeatData{Name: u.Name, Status: "active", By: "transcript", Written: seen}
		if lease := c.mapLeaseOf(in); lease > 0 {
			d.Expires = seen.Add(lease)
		}
		if err := c.emit(in, JournalHeartbeat, 0, d); err != nil {
			return err
		}
	}
	return nil
}

// stallThreshold is how long a unit of in's class may go without a transcript
// write before it is reported stalled: the state's StallAfter hint, else the
// class default. A zero class default (samples that finished at once) is no
// evidence about a child: StallDefault stands in for it.
func (c *cycle) stallThreshold(in *instance) time.Duration {
	if n := in.node(); n != nil {
		if k, ok := n.kind.(*agentKind); ok {
			if d := hintsOf(k.dispatch).StallAfter; d != 0 {
				return d
			}
		}
	}
	if d := c.classStats().StallThreshold(c.st.qualified(in)); d > 0 {
		return d
	}
	return c.e.opts.StallDefault
}

// watched reports whether the engine sees u's transcript this cycle: one is
// recorded and sampleTranscripts could stat it (a path with no write yet
// counts). Only a watched unit has a stall deadline, and only a watched
// running unit outlives its lease.
func (c *cycle) watched(u *unit) bool {
	return u.Claim != nil && u.Claim.Transcript != "" && !c.unstatable[u.Name]
}

// leaseExpired handles the passed lease deadline d of in's unit. The engine
// cannot see a child's work between its calls (rule 5: it reads no harness's
// private records), and silence is not evidence of death: one tool call can
// be silent for any length of time. So a fetched unit whose lease runs out is
// reported stalled, the coordinator's question "is it still running?", and
// becomes lost only through heartbeat died; heartbeat active extends it. The
// engine never reissues a fetched unit on a guess. A unit never fetched (a
// Map lease on a started claim) and a wave claim (the coordinator's own) are
// lost to their lease.
func (c *cycle) leaseExpired(in *instance, d deadline) error {
	if u := in.Unit; u != nil && u.Name == d.Unit && !u.FetchedAt.IsZero() && !u.waveClaim() && (u.State == UnitStarted || u.State == UnitRunning) {
		return c.stall(in, d)
	}
	return c.loseExpired(in, d, LostCauseLease)
}

// stallRecheck is when next puts in's stall question to the coordinator
// (again): at once when it never asked, else one recheck interval after it
// last did. A coordinator that cannot tell answers neither way; a blocking
// next that already asked waits for this (or a sign of life) and asks again,
// so a child that died unseen is asked about on a bounded schedule. ok is
// false for a unit that is not stalled.
func (c *cycle) stallRecheck(in *instance) (time.Time, bool) {
	u := in.Unit
	every := c.recheckEvery(in)
	if u == nil || in.Settled || u.State != UnitStalled || every <= 0 {
		return time.Time{}, false
	}
	if u.AskedAt.IsZero() {
		return u.StalledAt, true
	}
	return u.AskedAt.Add(every), true
}

// askStalled is the stall question of in's unit when it is due at now, nil
// otherwise. Asking journals heartbeat asked, which starts the wait for the
// next recheck.
func (c *cycle) askStalled(in *instance, now time.Time) (*StalledView, error) {
	due, ok := c.stallRecheck(in)
	if !ok || now.Before(due) {
		return nil, nil
	}
	u := in.Unit
	if err := c.emit(in, JournalHeartbeat, 0, HeartbeatData{Name: u.Name, Status: "asked", By: "next"}); err != nil {
		return nil, err
	}
	v := &StalledView{Name: u.Name, Since: u.liveAt(), Claim: u.Bound, Unprogressed: u.Unprogressed, Asks: u.Asks}
	v.Recheck, _ = c.stallRecheck(in)
	return v, nil
}

// recheckEvery is the interval of stallRecheck: the unit's lease length,
// else its stall threshold.
func (c *cycle) recheckEvery(in *instance) time.Duration {
	if every := c.leaseLength(in); every > 0 {
		return every
	}
	return c.stallThreshold(in)
}

// leaseLength is how long a fetched unit of in's class lives past its last
// sign of life before the coordinator is asked about it: the state's
// StallAfter hint, the workflow's per-step length, else FetchedLease.
func (c *cycle) leaseLength(in *instance) time.Duration {
	if n := in.node(); n != nil {
		if k, ok := n.kind.(*agentKind); ok {
			if d := hintsOf(k.dispatch).StallAfter; d > 0 {
				return d
			}
		}
	}
	return c.e.opts.FetchedLease
}

// renewDue reports whether a call from in's child (validate, which writes
// nothing) should renew its lease: it fetched its prompt, it is not settled
// or a wave claim, and it is stalled (the call answers the coordinator's
// question) or a quarter of its lease has passed since its last sign of life.
// The debounce keeps a busy child to about four records per lease.
func (c *cycle) renewDue(in *instance) bool {
	u := in.Unit
	if u == nil || in.Settled || !u.State.claimed() || u.FetchedAt.IsZero() || u.waveClaim() {
		return false
	}
	// A unit its coordinator vouched for without progress journals any child
	// call: the call is the progress that starts the count over.
	return u.State == UnitStalled || u.Unprogressed > 0 || c.now().Sub(u.liveAt()) >= c.effectiveLease(in)/4
}

// effectiveLease is the lease in's unit lives by: the Map's lease when the
// workflow declared one, else leaseLength.
func (c *cycle) effectiveLease(in *instance) time.Duration {
	if lease := c.mapLeaseOf(in); lease > 0 {
		return lease
	}
	return c.leaseLength(in)
}

// renewByCall renews the lease of unit name after its child called a verb
// that writes nothing (validate), when renewDue says so: the call is a sign
// of life the engine sees without reading any harness's records. It journals
// a heartbeat active by "child". Like any writing verb, its cycle may journal
// a pending migration first. A run held by another call skips the renewal:
// the child's next call renews it.
func (e *Engine) renewByCall(run RunID, name string) error {
	err := e.withRun(run, func(c *cycle) error {
		in, _, err := c.claimedUnit("renew", name)
		if err != nil || !c.renewDue(in) {
			return nil
		}
		d := HeartbeatData{Name: name, Status: "active", By: "child"}
		if lease := c.mapLeaseOf(in); lease > 0 {
			d.Expires = c.now().Add(lease)
		}
		return c.emit(in, JournalHeartbeat, 0, d)
	})
	if errors.Is(err, ErrRunLocked) {
		return nil
	}
	return err
}

// stall journals that the unit a passed stall deadline d was listed for has
// gone quiet, or whose lease expired after its prompt fetch (leaseExpired). It is a report, never a loss: the coordinator answers with
// Heartbeat, or a transcript write clears it.
func (c *cycle) stall(in *instance, d deadline) error {
	if in.Unit == nil || in.Unit.Name != d.Unit || (in.Unit.State != UnitRunning && in.Unit.State != UnitStarted) {
		return nil
	}
	return c.emit(in, JournalHeartbeat, 0, HeartbeatData{Name: d.Unit, Status: "stalled", By: "engine"})
}

// Heartbeat implements Executor.
func (e *Engine) Heartbeat(run RunID, t HeartbeatTarget, alive bool) error {
	return e.HeartbeatAs(run, t, alive, Coordinator{})
}

// HeartbeatAs is Heartbeat from coordinator co: a died report on a child of a
// session co replaced (or that a later session replaced) loses it as an
// orphan, exempt from the lost cap.
func (e *Engine) HeartbeatAs(run RunID, t HeartbeatTarget, alive bool, co Coordinator) error {
	return e.withRun(run, func(c *cycle) error {
		if err := c.checkSegment("heartbeat", harness.Caller{Harness: co.Harness, Session: co.Session, Root: co.Root, FromEnv: co.Session != ""}); err != nil {
			return err
		}
		if t.Name == "" {
			if t.Session == "" {
				return fmt.Errorf("%w: heartbeat: a unit name or a session is required", ErrRejected)
			}
			if alive {
				return fmt.Errorf("%w: heartbeat: alive is per unit, not per session", ErrRejected)
			}
			if b := c.st.untilUnit; b != nil {
				in := c.st.insts[b.Target.Instance]
				if err := c.checkUntilUnitMutation(in); err != nil {
					return err
				}
				if in.Unit == nil || in.Unit.Claim == nil || in.Unit.Claim.Session != t.Session {
					return fmt.Errorf("%w: session is outside immutable until-unit boundary", ErrRejected)
				}
			}
			if err := c.loseSession(t.Session); err != nil {
				return err
			}
			return c.drive()
		}
		in, _, err := c.claimedUnit("heartbeat", t.Name)
		if err != nil {
			return err
		}
		if alive {
			d := HeartbeatData{Name: t.Name, Status: "active", By: "coordinator", Note: t.Note}
			switch u := in.Unit; {
			case t.Note != "":
				// Observed work is evidence: the count starts over.
			case u == nil:
				d.Unprogressed = 1
			case u.Answered:
				d.Unprogressed = u.Unprogressed // one count per stall episode
			default:
				d.Unprogressed = u.Unprogressed + 1
			}
			if lease := c.mapLeaseOf(in); lease > 0 {
				d.Expires = c.now().Add(lease)
			}
			if err := c.emit(in, JournalHeartbeat, 0, d); err != nil {
				return err
			}
			return c.drive()
		}
		if in.Settled {
			// A late voter: its child already settled for an early join. Nothing
			// reissues it (deadline.go), so a died report changes nothing, as in
			// loseSession.
			return nil
		}
		if err := c.emit(in, JournalHeartbeat, 0, HeartbeatData{Name: t.Name, Status: "died", By: "coordinator"}); err != nil {
			return err
		}
		if u := in.Unit; u != nil && c.childCoordinatorReplaced(co, u) {
			// Its coordinator was replaced: the child was lost with it, not
			// by its own fault, so it never reaches the lost cap.
			if err := c.loseOrphan(in, fmt.Sprintf("reported died; its coordinator session %s was replaced", u.childOwner())); err != nil {
				return err
			}
			return c.drive()
		}
		if err := c.lose(in, LostCauseDied); err != nil {
			return err
		}
		return c.drive()
	})
}

// loseSession loses every claimed unit whose claim names session, the orphans
// of a coordinator that is gone. No such unit journals nothing.
func (c *cycle) loseSession(session string) error {
	for _, id := range append([]string(nil), c.st.order...) {
		if c.st.ended {
			return nil
		}
		in := c.st.insts[id]
		u := in.Unit
		if !c.untilUnitAllows(in) || u == nil || in.Settled || !u.State.claimed() || u.Claim == nil || u.Claim.Session != session {
			continue
		}
		if err := c.lose(in, LostCauseOrphan); err != nil {
			return err
		}
	}
	return nil
}
