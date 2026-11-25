package wf

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// EventKind is the closed set of what Wait reports.
type EventKind string

// Event kinds.
const (
	EventDone         EventKind = "done"
	EventFailed       EventKind = "failed"
	EventStall        EventKind = "stall"          // a running unit went quiet past its class threshold
	EventWindow       EventKind = "window expired" // a spawn or lease loss, or a re-offerable hand-out
	EventDeadline     EventKind = "deadline due"   // a backoff, gate or wait timeout came due
	EventGateAnswered EventKind = "gate answered"
	EventEffect       EventKind = "effect done" // a detached effect's result was journaled
	EventRunEnded     EventKind = "run ended"
	EventBreakpoint   EventKind = "breakpoint"
	EventTimeout      EventKind = "timeout"
	EventBlocked      EventKind = "blocked"
)

// DetailReofferable is the Detail of the window-expired event of a hand-out
// whose window closed before any claim: the unit is not lost, and the next
// HandOut offers it again under the same name.
const DetailReofferable = "re-offerable"

// Event is one thing that needs the coordinator.
//
// Owns: the event kinds (done, failed, blocked, stall, window expired, deadline due,
// gate answered elsewhere, effect done, run ended, breakpoint, timeout) and the one mapping from the
// journal records after a baseline Seq, plus the deadlines that came due, to
// them (cycle.events): a record's unit is the one its instance held at that
// record (the latest handed_out); accepted of an agent unit is done; refused,
// route_off_candidates of an agent unit or attempt_lost with LostCauseCap is
// failed; a refused, route_off_candidates or effect_settled record that halts a
// step with no unit (command, compute, effect) is blocked only when the run is
// held at that instance (cycle.heldAt): nothing moves the run past the halt
// without a human, and a halt a spawner settles is not reported; a heartbeat with
// status stalled is stall; any other
// attempt_lost is window expired; gate_answered is gate answered; run_ended is
// run ended; breakpoint_halted is breakpoint; an effect_result whose intent is detached (it has an effect_started
// or a maximum duration) is effect done, and so is an effect_intent that re-runs
// a dead launch (its detail names the launch and why it died); a due backoff,
// gate-timeout or wait-timeout deadline is deadline due; a handed unit whose
// hand-out window has passed unclaimed is window expired with Detail
// DetailReofferable, reported on every cycle until a HandOut re-offers it or a
// claim arrives (nothing is journaled for it).
// Does not: read anything but the journal and deadline. Wait never polls the
// harness.
// Relationships: Engine.Wait runs one cycle (lock, fold, drive, collect,
// unlock) and, with no events, sleeps without the lock until the earliest
// deadline, the journal's change signal or the timeout.
// Extend by: a new event kind is one EventKind value and one mapping arm in
// cycle.events.
type Event struct {
	Kind    EventKind `json:"kind"`
	Name    string    `json:"name,omitempty"`    // the unit, for unit events
	State   string    `json:"state,omitempty"`   // the qualified state
	Detail  string    `json:"detail,omitempty"`  // the cause, answer, outcome or deadline kind
	Seq     int64     `json:"seq,omitempty"`     // the record it came from
	Session string    `json:"session,omitempty"` // answering process for a gate answer
}

// Line is the event as one line: its kind, the unit name (else the state) and
// the detail.
func (e Event) Line() string {
	subject := e.Name
	if subject == "" {
		subject = e.State
	}
	detail := e.Detail
	if e.Kind == EventGateAnswered {
		detail += " (" + gateAnsweredBy(e.Session) + ")"
	}
	return strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{string(e.Kind), subject, detail}, func(s string) bool { return s == "" }), " "))
}

// String is Line.
func (e Event) String() string { return e.Line() }

// dueEventKind reports whether k maps to EventDeadline. The other kinds either
// end in a record that maps to an event (the windows) or are not the
// coordinator's concern.
func dueEventKind(k deadlineKind) bool {
	return k == deadlineBackoff || k == deadlineGateTimeout || k == deadlineWaitTimeout
}

// dueDeadline is a deadline that had passed when the cycle began, with the
// qualified state its instance was in then: drive moves the instance on.
type dueDeadline struct {
	deadline
	State string
}

// dueDeadlines are the deadlines of the event kinds that have passed at now.
func (c *cycle) dueDeadlines(now time.Time) []dueDeadline {
	var out []dueDeadline
	for _, d := range c.deadlines(now) {
		if !dueEventKind(d.Kind) || now.Before(d.Due) {
			continue
		}
		dd := dueDeadline{deadline: d}
		if in := c.st.insts[d.Instance]; in != nil {
			dd.State = c.st.qualified(in)
		}
		out = append(out, dd)
	}
	return out
}

// events maps the records after baseline, and the deadlines in due that came
// due, to Events: record events in Seq order, then the due deadlines.
// cursor says the caller carried baseline from its previous wait: an expired
// hand-out it was already shown (handed out at or before baseline) is skipped.
func (c *cycle) events(baseline int64, cursor bool, due []dueDeadline) []Event {
	detached := map[int64]bool{} // intents run by the detached runner
	for _, r := range c.recs {
		switch r.Kind {
		case JournalEffectStarted:
			if d, err := decodeData(r); err == nil {
				detached[d.(*EffectStartedData).Intent] = true
			}
		case JournalEffectIntent:
			if d, err := decodeData(r); err == nil && d.(*EffectIntentData).Max > 0 {
				detached[r.Seq] = true
			}
		}
	}
	// held is the unit each instance held at the record being read: a
	// handed_out record sets it, so a settlement record takes the unit of its
	// own visit, never a lookup that a revisit could answer with another's.
	type heldUnit struct{ name, state string }
	held := map[string]heldUnit{}
	var out []Event
	for _, r := range c.recs {
		if r.Kind == JournalHandedOut {
			if d, err := decodeData(r); err == nil {
				held[r.Instance] = heldUnit{d.(*HandedOutData).Name, r.State}
			}
		}
		if r.Seq <= baseline {
			continue
		}
		ev := Event{Seq: r.Seq, State: c.qualifiedState(r)}
		if h := held[r.Instance]; h.state == r.State {
			ev.Name = h.name
		}
		switch r.Kind {
		case JournalAccepted:
			if ev.Name == "" {
				continue // a computed or non-agent state
			}
			ev.Kind = EventDone
		case JournalRefused, JournalRouteOffCandidates:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			var ref RefusedData
			switch d := d.(type) {
			case *RefusedData:
				ref = *d
			case *RouteOffCandidatesData:
				ref = d.RefusedData
			}
			switch {
			case ev.Name != "":
				ev.Kind = EventFailed
			case ref.Halt && c.heldAtInstance(c.st.insts[r.Instance]):
				ev.Kind, ev.Detail = EventBlocked, firstReason(ref)
			default:
				continue // a re-issue or fallback edge of a non-agent state
			}
		case JournalEffectSettled:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			ref := d.(*EffectSettledData).Refused
			if ref == nil || !ref.Halt || !c.heldAtInstance(c.st.insts[r.Instance]) {
				continue
			}
			ev.Kind, ev.Detail = EventBlocked, firstReason(*ref)
		case JournalAttemptLost:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			lost := d.(*AttemptLostData)
			ev.Name, ev.Detail = lost.Name, lost.Cause
			ev.Kind = EventWindow
			if lost.Cause == LostCauseCap {
				ev.Kind = EventFailed
			}
		case JournalEffectIntent:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			rr := d.(*EffectIntentData).Rerun
			if rr == nil {
				continue
			}
			ev.Kind, ev.Detail = EventEffect, fmt.Sprintf("re-run after dead launch %d: %s", rr.Of, rr.Reason)
		case JournalEffectResult:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			res := d.(*EffectResultData)
			if !detached[res.Intent] {
				continue
			}
			ev.Kind, ev.Detail = EventEffect, res.Outcome
		case JournalHeartbeat:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			if hb := d.(*HeartbeatData); hb.Status == "stalled" {
				ev.Kind, ev.Name = EventStall, hb.Name
			} else {
				continue
			}
		case JournalGateAnswered:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			answer := d.(*GateAnsweredData)
			ev.Kind, ev.Detail, ev.Session = EventGateAnswered, answer.Answer, answer.Session
		case JournalBreakpointHalted:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			halt := d.(*BreakpointHaltedData)
			ev.Kind, ev.Name, ev.State, ev.Detail = EventBreakpoint, "", halt.State, halt.Reason
			if b := c.st.breakpoint; b != nil && b.Seq == r.Seq {
				ev.State = b.State
			}
		case JournalRunEnded:
			d, err := decodeData(r)
			if err != nil {
				continue
			}
			ev.Kind, ev.Name, ev.State = EventRunEnded, "", d.(*RunEndedData).Terminal
			ev.Detail = string(d.(*RunEndedData).Outcome)
		default:
			continue
		}
		if ev.Kind == EventBlocked {
			// A revisited state can halt twice: one blocked event per held
			// instance, from its latest halting record.
			if i := slices.IndexFunc(out, func(o Event) bool { return o.Kind == EventBlocked && o.State == ev.State }); i >= 0 {
				out = slices.Delete(out, i, i+1)
			}
		}
		out = append(out, ev)
	}
	for _, d := range due {
		out = append(out, Event{Kind: EventDeadline, State: d.State, Detail: d.Kind.String()})
	}
	for _, in := range c.expiredHandOuts() {
		// A follower carrying its cursor saw this expiry on the wait whose
		// cursor passed the hand-out; it is not shown again until a re-offer.
		if cursor && c.handedOutSeq(in.Unit.Name) <= baseline {
			continue
		}
		out = append(out, Event{Kind: EventWindow, Name: in.Unit.Name, State: c.st.qualified(in), Detail: DetailReofferable})
	}
	return out
}

// handedOutSeq is the seq of the latest handed_out record of the named unit.
func (c *cycle) handedOutSeq(name string) int64 {
	for i := len(c.recs) - 1; i >= 0; i-- {
		if r := c.recs[i]; r.Kind == JournalHandedOut {
			if d, err := decodeData(r); err == nil && d.(*HandedOutData).Name == name {
				return r.Seq
			}
		}
	}
	return 0
}

// heldAt reports the instance the run is held at: halted and unsettled with
// nothing above it to settle it, the first in c.st.order, and false when the
// run has ended or nothing is held.
//
// Owns: when the run is held, the one meaning of "blocked" for Wait's blocked
// event, Status's RunBlocked phase and its blocked: reason, and the instance
// Retry accepts.
// Does not: read the definition tree, a spawner's sibling policy, the clock or
// the journal; only the folded state. Callers evaluate it after the cycle's
// drive (Wait) or on the fold a driving verb left (Status, Retry).
// Relationships: a spawner (Map, Sub) settles a halted unsettled child on its
// next execute, whatever its policy, so after a drive in which the parent ran
// only the root, or a child whose spawner has not decided it, is held.
// Extend by: a new way for a halt to be absorbed is a change to
// heldAtInstance, not to its callers.
func (c *cycle) heldAt() (*instance, bool) {
	for _, id := range c.st.order {
		if in := c.st.insts[id]; c.heldAtInstance(in) {
			return in, true
		}
	}
	return nil, false
}

// heldAtInstance is heldAt's rule for one instance: it is halted (by phase, not
// halted(), which also counts a child its spawner settled) and unsettled, and
// is the root or its spawner has not decided it.
func (c *cycle) heldAtInstance(in *instance) bool {
	if in == nil || c.st.ended || in.Phase != phaseHalted || in.Settled || in.Recovery {
		return false // a recovery offer is the coordinator's to answer, not a human's
	}
	return in.ID == "" || c.mapUndecided(in)
}

// mapUndecided reports whether in's parent is still at the state that spawned
// it and has not decided on this entry, so a halted in can still be retried.
func (c *cycle) mapUndecided(in *instance) bool {
	p := c.st.insts[in.Parent]
	return p != nil && p.Cur == in.Spawner && p.Pending == nil && in.Entry == p.Visits[in.Spawner]
}

func firstReason(r RefusedData) string {
	if len(r.Reasons) == 0 {
		return ""
	}
	return r.Reasons[0]
}

// qualifiedState is the qualified state the record is about.
func (c *cycle) qualifiedState(r JournalRecord) string {
	in := c.st.insts[r.Instance]
	if in == nil {
		return r.State
	}
	return strings.Join(append(c.st.defPath(in), r.State), "/")
}

// gateAnsweredBy names only the recorded answering process.
func gateAnsweredBy(session string) string {
	if session == "" {
		session = "unknown session"
	}
	return "answered by " + session
}
