package wf

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// UnitState is the closed set of states of one unit: one agent attempt.
type UnitState int

// Unit states.
const (
	UnitPending UnitState = iota + 1
	UnitHanded
	UnitStarted
	UnitRunning
	UnitStalled
	UnitDone
	UnitFailed
	UnitLost
	UnitWithdrawn
	UnitWaiting
	UnitHeld
)

var unitStateNames = [...]string{
	UnitPending:   "pending",
	UnitHanded:    "handed",
	UnitStarted:   "started",
	UnitRunning:   "running",
	UnitStalled:   "stalled",
	UnitDone:      "done",
	UnitFailed:    "failed",
	UnitLost:      "lost",
	UnitWithdrawn: "withdrawn",
	UnitWaiting:   "waiting",
	UnitHeld:      "held",
}

// String returns the lower-case state name.
func (s UnitState) String() string {
	if s >= 1 && int(s) < len(unitStateNames) {
		return unitStateNames[s]
	}
	return fmt.Sprintf("UnitState(%d)", int(s))
}

// live reports whether the unit holds an agent: handed, started, running or
// stalled.
func (s UnitState) live() bool {
	return s == UnitHanded || s == UnitStarted || s == UnitRunning || s == UnitStalled
}

// claimed reports whether the unit has been claimed: started, running or
// stalled.
func (s UnitState) claimed() bool { return s >= UnitStarted && s <= UnitStalled }

// terminal reports whether the unit has settled.
func (s UnitState) terminal() bool {
	return s == UnitDone || s == UnitFailed || s == UnitWithdrawn
}

// unit is the dispatch record of one agent attempt inside the fold.
//
// Owns: the stable name (instance path, item key, seat or state, then
// "#attempt", produced by one naming function and journaled in handed_out); the
// state (pending, handed, started, running, stalled, done, failed, lost,
// withdrawn or waiting); the claim (harness, session, agent, transcript); the
// hand-out, start, fetch and last-heartbeat stamps; the per-state lost counts
// and the predecessor line; and, on runState, the run-wide name index: name to
// instance and attempt, the retired names, and the name-to-token map that keeps
// Submit working.
// Hides: how a name maps to the internal token, and which transitions are
// legal. A claim is refused unless the unit is handed, or its metadata is
// compatible with the existing claim. Below the lost cap, lost returns the unit to pending at attempt+1 with
// no retry spent. At the cap, attempt_lost with cause "lost cap" settles the
// attempt failed.
// Does not: read a clock or a transcript, or decide an expiry. Transitions
// happen only in runState.apply, from journaled records. Expiries come from
// deadline, and the Engine journals them.
// Relationships: composed into instance beside Token, Issued and Lease (which
// the existing verbs still read until they move over). The Engine verbs and the
// status rows read it. Dependencies point from the Engine to runState to the
// unit.
// Lifecycle: rebuilt by every fold. It lives for one locked cycle and is never
// shared across goroutines.
// Errors: a name that was never handed out, a retired name, or a unit claimed by
// another live agent becomes ErrRejected (exit 4). Callers never compare tokens
// or names themselves.
// Extend by: a new state is one UnitState value plus its apply cases; callers
// never compare tokens or names, and only apply changes a unit's state.
type unit struct {
	Packet        *PromptPacket   `json:",omitempty"`
	PagesFetched  map[string]bool `json:",omitempty"`
	PageFetchedAt time.Time       `json:",omitzero"`
	Name          string          `json:",omitempty"`
	State         UnitState       `json:",omitempty"`
	Attempt       int             `json:",omitempty"` // the instance attempt this unit runs
	Token         string          `json:",omitempty"`
	Claim         *unitClaim      `json:",omitempty"`
	HandedAt      time.Time       `json:",omitzero"` // stamps are the records' At, never a clock
	HandExpires   time.Time       `json:",omitzero"`
	StartedAt     time.Time       `json:",omitzero"`
	FetchedAt     time.Time       `json:",omitzero"`
	Bodies        []string        `json:",omitempty"` // the instruction bodies its prompt fetches delivered (E1), each a path the child is told to read
	Whole         bool            `json:",omitempty"` // a fetch handed it the whole prompt (prompt --json)
	BeatAt        time.Time       `json:",omitzero"`
	StalledAt     time.Time       `json:",omitzero"` // UnitStalled: when the engine reported it
	AskedAt       time.Time       `json:",omitzero"` // UnitStalled: when next last asked the stall question (zero: not yet)
	LeaseExpires  time.Time       `json:",omitzero"`
	Unprogressed  int             `json:",omitempty"` // the coordinator's active answers, one per stall episode, with no engine event from the child between them (HeartbeatData.Unprogressed)
	Answered      bool            `json:",omitempty"` // the coordinator answered active since the last stall report: the episode is counted
	Asks          int             `json:",omitempty"` // UnitStalled: how many times next asked this episode's question
	Observed      string          `json:",omitempty"` // the coordinator's latest heartbeat active --note on this attempt (HeartbeatData.Note)
	RestartedAt   time.Time       `json:",omitzero"`  // the latest attempt_started on the already claimed unit
	Predecessor   string          `json:",omitempty"` // the retired name this attempt replaced
	BoundCaller   *Claim          `json:",omitempty"`
	Bound         string          `json:",omitempty"` // the claim token bound by the first tokened prompt fetch
	Offered       []string        `json:",omitempty"` // the claim tokens this unit's handed_out records minted
	Output        string          `json:",omitempty"` // the allocated result path its hand-out recorded, relative to out/
	Held          *UnitHeldData   `json:",omitempty"` // UnitHeld: why a blocked failure holds it
	HandSession   string          `json:",omitempty"` // the coordinator session the latest hand-out went to
	HandSegment   int64           `json:",omitempty"` // the harness segment (its start seq) the latest hand-out was made in
	HandCall      int64           `json:",omitempty"` // the next call of the latest hand-out (HandedOutData.Call)
	Unstarted     string          `json:",omitempty"` // the Cause of the latest never-started note since the latest hand-out: a re-list with the same evidence journals no other
	Reoffers      int             `json:",omitempty"` // hand-outs again to the same coordinator session while unclaimed; history only, nothing is capped by it
	SessionSince  time.Time       `json:",omitzero"`  // the first hand-out to the latest coordinator session
	// Origins maps each claim token in Offered to the coordinator session and
	// next call (its recorded Call, 0 before hand-outs had one) of the
	// hand-out that minted it: a fetch credits the offer whose token it
	// presented, not the latest one.
	Origins map[string]offerOrigin `json:",omitempty"`
	// HandHarness and HandRoot are the harness and the records root of the
	// coordinator the latest hand-out went to, from its environment at next:
	// where the child's records are read for usage and liveness, and what a
	// caller of another harness is noted against. FirstHandedAt is the unit's
	// first hand-out: a child of any of its offers started after it.
	HandHarness   string    `json:",omitempty"`
	HandRoot      string    `json:",omitempty"`
	FirstHandedAt time.Time `json:",omitzero"`
	// Recovery is the open recovery offer of a claimed child that ended
	// without done (F1): the attempt stays open until the coordinator answers.
	Recovery *RecoveryOfferedData `json:",omitempty"`
	// RecoveryAnswer is the coordinator's answer to that offer, journaled and
	// not yet applied (its degraded record and its loss or failure follow);
	// ResumedAt is the transcript's mtime when it answered resume, so the same
	// end is not offered again before the child writes.
	RecoveryAnswer string `json:",omitempty"`
	RecoveryCause  string `json:",omitempty"` // the answered offer's cause
	// RecoveryRecorded: the answer's degraded record is journaled; the
	// answer stays pending until its loss or failure is.
	RecoveryRecorded bool   `json:",omitempty"`
	ResumedAt        string `json:",omitempty"`
}

type unitClaim struct {
	Harness, Session, Agent, Transcript string
	Context                             ContextMode
	Root                                string `json:",omitempty"`
	Env                                 bool   `json:",omitempty"` // a prompt fetch read the child's environment
}

// merge adds missing ownership metadata; an established value cannot change.
func (cl unitClaim) merge(add unitClaim) (unitClaim, bool) {
	for _, field := range []struct {
		old  *string
		next string
	}{
		{&cl.Harness, add.Harness}, {&cl.Session, add.Session},
		{&cl.Agent, add.Agent}, {&cl.Transcript, add.Transcript},
		{(*string)(&cl.Context), string(add.Context)}, {&cl.Root, add.Root},
	} {
		if field.next == "" {
			continue
		}
		if *field.old != "" && *field.old != field.next {
			return unitClaim{}, false
		}
		*field.old = field.next
	}
	cl.Env = cl.Env || add.Env
	return cl, true
}

// offer records a claim token a hand-out of u minted; empty (an old record) is
// none.
func (u *unit) offer(claim, session string, call, segment int64, errLog string) {
	if claim != "" {
		u.Offered = append(u.Offered, claim)
		if u.Origins == nil {
			u.Origins = map[string]offerOrigin{}
		}
		u.Origins[claim] = offerOrigin{Session: session, Call: call, Segment: segment, ErrLog: errLog}
	}
}

// offerOrigin is the coordinator session, next call and harness segment of
// one hand-out.
type offerOrigin struct {
	Session string `json:",omitempty"`
	Call    int64  `json:",omitempty"`
	Segment int64  `json:",omitempty"`
	ErrLog  string `json:",omitempty"` // the stderr file the hand-out chose, "" when its adapter has none
}

// errLogOf is the stderr file the hand-out that minted token recorded.
func (u *unit) errLogOf(token string) string { return u.Origins[token].ErrLog }

// spawnErrLog is the stderr file a handed_out record carries.
func (d *HandedOutData) spawnErrLog() string {
	if d.Spawn == nil {
		return ""
	}
	return d.Spawn.ErrLog
}

// conflict names the first field of add that contradicts cl.
func (cl unitClaim) conflict(unit string, add unitClaim) string {
	for _, f := range []struct{ name, old, next string }{
		{"harness", cl.Harness, add.Harness}, {"session", cl.Session, add.Session},
		{"agent", cl.Agent, add.Agent}, {"transcript", cl.Transcript, add.Transcript},
		{"context", string(cl.Context), string(add.Context)},
	} {
		if f.next != "" && f.old != "" && f.old != f.next {
			return fmt.Sprintf("conflicting metadata for unit %s: %s %q is already attached, not %q", unit, f.name, f.old, f.next)
		}
	}
	return "conflicting metadata for unit " + unit
}

// wave claims deliver prompts inline and deliberately have no transcript.
func (u *unit) waveClaim() bool {
	return u.Claim != nil && *u.Claim == (unitClaim{Harness: waveSession, Session: waveSession, Agent: waveSession})
}

// unitRef is one entry of the run-wide name index.
type unitRef struct {
	Instance, State string
	Attempt         int
}

// initIndex allocates the name index; the fold calls it before writing.
func (st *runState) initIndex() {
	if st.names == nil {
		st.names, st.retired, st.nameToken = map[string]unitRef{}, map[string]bool{}, map[string]string{}
		st.endedUnits = map[string]endedUnit{}
		st.acceptedClaims = map[string]acceptedClaim{}
		st.failedClaims = map[string]failedClaim{}
	}
}

// acceptedClaim is a unit whose done was accepted, keyed by its bound claim.
type acceptedClaim struct {
	Name string
}

// failedClaim is a unit that ended failed, keyed by its bound claim, and the
// class and reasons of the record that ended it.
type failedClaim struct {
	Name           string
	Class          OutcomeClass
	Reasons        []string
	ApprovalAction string
}

// noteFailedClaim records u's bound claim as ended by its child's own failed
// (a ChildFailed record; unmarked and older records never qualify); the
// caller ends the unit right after.
func (st *runState) noteFailedClaim(u *unit, class OutcomeClass, reasons []string) {
	if u.Bound == "" {
		return
	}
	st.initIndex()
	st.failedClaims[u.Bound] = failedClaim{Name: u.Name, Class: class, Reasons: reasons}
}

// addUnitTokens records tokens spent by the named unit's attempt.
func (st *runState) addUnitTokens(name string, n int) {
	if st.unitTokens == nil {
		st.unitTokens = map[string]int{}
	}
	st.unitTokens[name] += n
}

// unitPath is the instance's name segments below the root: a Map child's
// "<state>[<key>]" becomes "<state>/<key>", a Sub child's "<state>#<n>" becomes
// "<state>" (n == 1) or "<state>.<n>".
func unitPath(id string) string {
	if id == "" {
		return ""
	}
	var parts []string
	for _, seg := range strings.Split(id, "/") {
		switch {
		case strings.HasSuffix(seg, "]") && strings.Contains(seg, "["):
			i := strings.Index(seg, "[")
			parts = append(parts, seg[:i], seg[i+1:len(seg)-1])
		case strings.Contains(seg, "#"):
			i := strings.LastIndex(seg, "#")
			if n, err := strconv.Atoi(seg[i+1:]); err == nil && n != 1 {
				parts = append(parts, seg[:i]+"."+seg[i+1:])
			} else {
				parts = append(parts, seg[:i])
			}
		default:
			parts = append(parts, seg)
		}
	}
	return strings.Join(parts, "/")
}

// unitName is the stable name of the next unit of in at its current state:
// the instance path, "/", the state, "#", and the smallest ordinal k >= 1 whose
// name is not yet in the name index (so revisits and reissues never reuse a
// name). The instance path drops the root, turns a Map child's "<state>[<key>]"
// segment into "<state>/<key>" and a Sub child's "<state>#<n>" into "<state>"
// (n == 1) or "<state>.<n>".
func (st *runState) unitName(in *instance) string {
	base := in.Cur
	if p := unitPath(in.ID); p != "" {
		base = p + "/" + in.Cur
	}
	for k := 1; ; k++ {
		name := base + "#" + strconv.Itoa(k)
		if _, used := st.names[name]; !used && !st.retired[name] {
			return name
		}
	}
}

// unitByName finds the live unit called name. A retired or unknown name is
// not found.
func (st *runState) unitByName(name string) (*instance, *unit, bool) {
	ref, ok := st.names[name]
	if !ok || st.retired[name] {
		return nil, nil, false
	}
	in := st.insts[ref.Instance]
	if in == nil || in.Unit == nil || in.Unit.Name != name {
		return nil, nil, false
	}
	return in, in.Unit, true
}

// nameUsed reports whether name is in the index or retired.
func (st *runState) nameUsed(name string) bool {
	_, used := st.names[name]
	return used || st.retired[name]
}

// awaitsHandOut reports whether the instance has an open agent instruction
// whose unit is not handed out yet: it has none, it is pending (fresh or a
// lost reissue), or the previous attempt's unit settled and the instruction
// was reissued under a new token.
func (in *instance) awaitsHandOut() bool {
	if in.Phase != phaseIssued || in.Issued == nil || in.Settled {
		return false
	}
	u := in.Unit
	return u == nil || u.State == UnitPending || (u.State.terminal() && u.Token != in.Token)
}

// tokenForName is the internal token a live unit name maps to.
func (st *runState) tokenForName(name string) (string, bool) {
	if _, _, ok := st.unitByName(name); !ok {
		return "", false
	}
	tok, ok := st.nameToken[name]
	return tok, ok
}

// endedUnit is what a retired name keeps: its final state and claim, so a late
// wf started can still attach metadata to it.
type endedUnit struct {
	State   string
	Claim   unitClaim
	Backoff bool // old engines could journal liveness after this retirement
}

// retire takes a unit's name out of use for good. A unit that was claimed and
// ended (state non-empty) keeps its claim for a late started.
func (st *runState) retire(u *unit, state string) {
	if u.Name == "" {
		return
	}
	st.initIndex()
	st.retired[u.Name] = true
	if state != "" && u.Claim != nil {
		st.endedUnits[u.Name] = endedUnit{State: state, Claim: *u.Claim}
	}
}

// endUnit settles the instance's unit as to (UnitDone, UnitFailed or
// UnitWithdrawn) and
// retires its name. It does nothing when there is no unit or it already
// settled.
func (in *instance) endUnit(st *runState, to UnitState) {
	if in.Unit == nil || in.Unit.State.terminal() {
		return
	}
	in.Unit.Recovery, in.Unit.RecoveryAnswer, in.Unit.RecoveryRecorded = nil, "", false // an open offer ends with its unit
	in.Unit.State = to
	st.retire(in.Unit, to.String())
}

// historicalBackoffLiveness recognizes records older engines could append after
// backoff left a claimed unit live. They must not revive that retired unit or
// affect its replacement. Public verbs still require a live claim.
func (st *runState) historicalBackoffLiveness(data any) bool {
	var name string
	switch d := data.(type) {
	case *HeartbeatData:
		name = d.Name
	case *AttemptStartedData:
		name = d.Name
	case *PromptFetchedData:
		name = d.Name
	case *AttemptLostData:
		name = d.Name
	case *RecoveryOfferedData:
		name = d.Unit
	case *RecoveryAnsweredData:
		name = d.Unit
	default:
		return false
	}
	return st.endedUnits[name].Backoff
}

// applyUnit folds the dispatch records. It returns false when d is not one.
func (st *runState) applyUnit(r JournalRecord, in *instance, data any) (bool, error) {
	switch d := data.(type) {
	case *HandedOutData:
		if in.Phase != phaseIssued || d.Token != in.Token {
			return true, corrupt(r, "handed_out %q does not match the open instruction", d.Name)
		}
		st.initIndex()
		st.noteChildSession(harness.SpawnSession(d.Harness, d.Claim))
		if d.Session != "" || d.Harness != "" {
			if st.coordinators == nil {
				st.coordinators = map[string]string{}
			}
			st.coordinators[d.Session] = d.Harness
			st.noteCoordinator(d.Session)
			if d.Session != "" {
				st.lastCoordinator = d.Session
				if st.coordLastAt == nil {
					st.coordLastAt = map[string]time.Time{}
				}
				st.coordLastAt[d.Session] = r.At
			}
		}
		if u := in.Unit; u != nil && u.State == UnitHanded && u.Name == d.Name {
			// A re-offer: the hand-out window closed before any claim, so the
			// same unit is handed out again under the same name.
			if d.Session != "" && d.Session == u.HandSession {
				u.Reoffers++
			} else {
				u.Reoffers, u.SessionSince = 0, r.At // a new coordinator's first offer
			}
			u.HandedAt, u.HandExpires, u.HandSession, u.HandCall = r.At, d.Expires, d.Session, d.call(r)
			u.Unstarted = "" // a new offer: its evidence starts over
			u.HandSegment = st.countHandOut(d)
			u.HandHarness, u.HandRoot = d.Harness, d.Root
			if d.Output != "" {
				u.Output = d.Output
			}
			u.offer(d.Claim, d.Session, d.Call, u.HandSegment, d.spawnErrLog())
			return true, nil
		}
		if _, used := st.names[d.Name]; used || st.retired[d.Name] {
			return true, corrupt(r, "unit name %q already used", d.Name)
		}
		pred := ""
		if in.Unit != nil {
			switch {
			case in.Unit.State == UnitPending:
				pred = in.Unit.Predecessor
			case !in.Unit.State.terminal():
				return true, corrupt(r, "handed_out %q over a %s unit", d.Name, in.Unit.State)
			}
		}
		in.Unit = &unit{Name: d.Name, State: UnitHanded, Attempt: in.Attempt, Token: d.Token,
			HandedAt: r.At, HandExpires: d.Expires, Predecessor: pred, Output: d.Output, HandSession: d.Session,
			HandHarness: d.Harness, HandRoot: d.Root, FirstHandedAt: r.At, SessionSince: r.At, HandCall: d.call(r), HandSegment: st.countHandOut(d)}
		in.Unit.offer(d.Claim, d.Session, d.Call, in.Unit.HandSegment, d.spawnErrLog())
		st.names[d.Name] = unitRef{Instance: in.ID, State: in.Cur, Attempt: in.Attempt}
		st.nameToken[d.Name] = d.Token
	case *AttemptStartedData:
		_, u, ok := st.unitByName(d.Name)
		if !ok {
			return true, corrupt(r, "attempt_started: no live unit %q", d.Name)
		}
		switch {
		case u.State == UnitHanded:
			u.State = UnitStarted
			u.Claim = &unitClaim{Harness: d.Harness, Session: d.Session, Agent: d.Agent, Transcript: d.Transcript, Context: d.Context, Root: d.Root, Env: d.Env}
			u.StartedAt, u.LeaseExpires = r.At, d.Expires
		case u.State.claimed() && u.Claim != nil:
			merged, ok := u.Claim.merge(unitClaim{Harness: d.Harness, Session: d.Session, Agent: d.Agent, Transcript: d.Transcript, Context: d.Context, Root: d.Root, Env: d.Env})
			if !ok {
				return true, corrupt(r, "attempt_started: conflicting ownership for unit %q", d.Name)
			}
			u.Claim = &merged
			u.RestartedAt = r.At
			u.Unprogressed = 0 // the child's started: an engine event from it
		default:
			return true, corrupt(r, "attempt_started: unit %q is %s", d.Name, u.State)
		}
	case *PromptFetchedData:
		_, u, ok := st.unitByName(d.Name)
		if !ok || !u.State.claimed() {
			return true, corrupt(r, "prompt_fetched: no started unit %q", d.Name)
		}
		if d.Packet != nil {
			if d.Packet.Run != r.Run || d.Packet.Unit != d.Name || d.Packet.Attempt != in.Attempt {
				return true, corrupt(r, "prompt packet belongs to another run, unit or attempt")
			}
			if len(d.Packet.Pages) == 0 || len(d.Packet.Pages) != len(d.Packet.Codes) {
				return true, corrupt(r, "invalid prompt packet pages")
			}
			for _, ref := range d.Packet.References {
				if len(ref.Pages) == 0 || len(ref.Pages) != len(ref.Codes) {
					return true, corrupt(r, "invalid reference pages")
				}
			}
			if u.Packet != nil && u.Packet.Digest != d.Packet.Digest {
				return true, corrupt(r, "prompt packet changed")
			}
			u.Packet = d.Packet
		}
		if d.Page > 0 {
			if u.Packet == nil || d.PacketDigest != u.Packet.Digest {
				return true, corrupt(r, "prompt page digest mismatch")
			}
			pages := u.Packet.Pages
			if d.Reference != "" {
				ref, ok := u.Packet.References[d.Reference]
				if !ok {
					return true, corrupt(r, "unknown prompt reference")
				}
				pages = ref.Pages
			}
			if d.Page > len(pages) {
				return true, corrupt(r, "prompt page out of range")
			}
			if u.PagesFetched == nil {
				u.PagesFetched = map[string]bool{}
			}
			u.PagesFetched[pageKey(d.Reference, d.Page)] = true
			u.PageFetchedAt = r.At
		}
		// The fetch that binds a token credits the offer that minted it. A
		// tokenless fetch (a person's, a library's, an old record), a token no
		// hand-out recorded, or one minted by a hand-out from before Call
		// (whose call is not known) credits nothing.
		if o, ok := u.Origins[d.Claim]; ok && u.Bound == "" && d.Claim != "" && o.Session != "" && o.Call != 0 {
			st.noteFetchedCall(o.Session, o.Call)
		}
		u.FetchedAt = r.At
		u.Unprogressed = 0 // a fetch is the child's own sign of life
		u.Whole = u.Whole || d.Whole
		if d.PromptDir != "" {
			if body := filepath.Join(d.PromptDir, promptBodyFile); !slices.Contains(u.Bodies, body) {
				u.Bodies = append(u.Bodies, body)
			}
		}
		if u.Bound == "" && d.Claim != "" {
			u.Bound = d.Claim
			u.BoundCaller = d.Caller
		}
		// A fetch is a sign of life: it starts a started unit, and a re-fetch
		// of a bound unit (held to its bound token, so it is the child's own;
		// its record names no claim) answers a stalled unit's question.
		if u.State == UnitStarted || u.State == UnitStalled && u.Bound != "" {
			u.State = UnitRunning
		}
	case *HeartbeatData:
		_, u, ok := st.unitByName(d.Name)
		if !ok || !u.State.claimed() {
			return true, corrupt(r, "heartbeat: no started unit %q", d.Name)
		}
		switch d.Status {
		case "active":
			if d.By == "transcript" {
				// A transcript write: the unit is alive, but writing says
				// nothing of whether it fetched its prompt, so a started unit
				// keeps its spawn window.
				if u.State == UnitStalled {
					u.State = UnitRunning
				}
			} else {
				u.State = UnitRunning
			}
			switch d.By {
			case "coordinator":
				u.Unprogressed, u.Answered = d.Unprogressed, true
				if d.Note != "" {
					u.Observed = d.Note
				}
			case "transcript":
				// A harness's file moved: liveness, not an engine event from
				// the child (rule 5), so the count runs on.
			default:
				u.Unprogressed = 0 // the child's own verb: progress
			}
			u.BeatAt = r.At
			if !d.Written.IsZero() {
				u.BeatAt = d.Written
			}
			if !d.Expires.IsZero() {
				u.LeaseExpires = d.Expires
			}
		case "stalled":
			if u.State != UnitStarted && u.State != UnitRunning {
				return true, corrupt(r, "heartbeat: stalled unit %q is %s", d.Name, u.State)
			}
			u.State, u.StalledAt, u.AskedAt = UnitStalled, r.At, time.Time{}
			u.Answered, u.Asks = false, 0 // a new stall episode
		case "asked":
			if u.State != UnitStalled {
				return true, corrupt(r, "heartbeat: asked about unit %q, which is %s", d.Name, u.State)
			}
			u.AskedAt = r.At
			u.Asks++
		case "died":
			u.BeatAt = r.At
		default:
			return true, corrupt(r, "heartbeat: status %q", d.Status)
		}
	case *AttemptLostData:
		_, u, ok := st.unitByName(d.Name)
		if !ok || !u.State.live() {
			return true, corrupt(r, "attempt_lost: no live unit %q", d.Name)
		}
		if in.Lost == nil {
			in.Lost = map[string]int{}
		}
		if d.Cause != LostCauseFork { // a fork's orphan is not this run's loss
			in.Lost[u.State.String()]++
		}
		u.Recovery, u.RecoveryAnswer, u.RecoveryRecorded = nil, "", false
		if d.Cause == LostCauseCap {
			in.CapPending = d.Reason
			if in.CapPending == "" {
				in.CapPending = "lost cap: " + d.Name
			}
		}
		if u.State == UnitHanded {
			st.turns-- // a unit lost before its start costs no turn
		}
		st.retire(u, "")
		if d.Cause == LostCauseCap {
			u.State = UnitFailed
			return true, nil
		}
		in.Unit = &unit{State: UnitPending, Attempt: in.Attempt + 1, Predecessor: u.Name}
		in.reissue()
	case *WithdrawnData:
		_, u, ok := st.unitByName(d.Name)
		if !ok && in.awaitsHandOut() && !st.nameUsed(d.Name) {
			// Withdrawn before it was ever handed out: the unit exists only
			// now, already settled, under the name it would have been given.
			st.initIndex()
			pred := ""
			if in.Unit != nil {
				pred = in.Unit.Predecessor
			}
			in.Unit = &unit{Name: d.Name, State: UnitWithdrawn, Attempt: in.Attempt, Token: in.Token, Predecessor: pred}
			st.names[d.Name] = unitRef{Instance: in.ID, State: in.Cur, Attempt: in.Attempt}
			st.turns-- // the instruction's turn was never started
			st.retire(in.Unit, "")
			return true, nil
		}
		// A started unit can only be withdrawn once its instance settled for a
		// join while it ran (a late failure).
		if !ok || (u.State != UnitPending && u.State != UnitHanded && !(in.Settled && u.State.claimed())) {
			return true, corrupt(r, "withdrawn: no pending or handed unit %q", d.Name)
		}
		if !u.State.claimed() {
			st.turns-- // a unit withdrawn before its start costs no turn
		}
		u.State = UnitWithdrawn
		st.retire(u, "")
	case *UnitHeldData:
		u := in.Unit
		if u == nil {
			u = &unit{Name: d.Name, Attempt: in.Attempt, Token: in.Token}
			in.Unit = u
		}
		if u.State.terminal() {
			return true, corrupt(r, "unit_held: unit %q is %s", u.Name, u.State)
		}
		if d.ApprovalAction != "" {
			d.approvalHold = r.Seq
			if u.Bound != "" {
				st.initIndex()
				st.failedClaims[u.Bound] = failedClaim{Name: u.Name, ApprovalAction: d.ApprovalAction}
			}
		}
		u.State, u.Held = UnitHeld, d
	case *LateVoteData:
		_, u, ok := st.unitByName(d.Name)
		if !ok || !u.State.claimed() {
			return true, corrupt(r, "late_vote: no started unit %q", d.Name)
		}
		u.State = UnitDone
		st.retire(u, UnitDone.String())
		if d.Tokens > 0 {
			st.tokens += d.Tokens
			st.addUnitTokens(d.Name, d.Tokens)
		}
	case *MetadataAttachedData:
		add := unitClaim{Harness: d.Harness, Session: d.Session, Agent: d.Agent, Transcript: d.Transcript, Context: d.Context, Root: d.Root, Env: d.Env}
		if _, u, ok := st.unitByName(d.Name); ok {
			if d.Tokens > 0 {
				return true, corrupt(r, "metadata_attached: usage for unit %q before it ended", d.Name)
			}
			if !u.State.claimed() || u.Claim == nil {
				return true, corrupt(r, "metadata_attached: unit %q is %s", d.Name, u.State)
			}
			merged, ok := u.Claim.merge(add)
			if !ok {
				return true, corrupt(r, "metadata_attached: conflicting ownership for unit %q", d.Name)
			}
			u.Claim = &merged
			if !d.Resolved {
				u.RestartedAt = r.At // a late started is a sign of life; the engine's own find is not
				u.Unprogressed = 0
			}
			return true, nil
		}
		e, ok := st.endedUnits[d.Name]
		if !ok {
			return true, corrupt(r, "metadata_attached: no unit %q", d.Name)
		}
		merged, ok := e.Claim.merge(add)
		if !ok {
			return true, corrupt(r, "metadata_attached: conflicting ownership for unit %q", d.Name)
		}
		e.Claim = merged
		st.endedUnits[d.Name] = e
		if d.Tokens > 0 {
			st.tokens += d.Tokens
			st.addUnitTokens(d.Name, d.Tokens)
		}
	case *EffectStartedData:
		if in.Cmd == nil || in.Cmd.Seq != d.Intent {
			return true, corrupt(r, "effect_started: no open command intent %d", d.Intent)
		}
		in.Cmd.Started = d
	default:
		return false, nil
	}
	return true, nil
}

// call is the next call d was made in: its Call, else (a record from before
// hand-outs carried one) its own Seq.
func (d *HandedOutData) call(r JournalRecord) int64 {
	if d.Call != 0 {
		return d.Call
	}
	return r.Seq
}

// noteFetchedCall records that a child fetched a unit handed to session in
// the next call call.
func (st *runState) noteFetchedCall(session string, call int64) {
	if st.fetchedCall == nil {
		st.fetchedCall = map[string]int64{}
	}
	if call > st.fetchedCall[session] {
		st.fetchedCall[session] = call
	}
}

// unstarted reports whether u, handed and unclaimed, was passed over by the
// coordinator it went to: a child of a unit handed to that session by a
// later next call fetched its prompt. A coordinator calls next again
// only after starting what it was offered, so a later offer's child running
// while this one never fetched means its spawn was refused or forgotten.
// Siblings of one call are no evidence: a serial spawner may still be
// starting this one. Calls are ordered by journal Seq; nothing here reads a
// clock.
func (st *runState) unstarted(u *unit) bool {
	return u.State == UnitHanded && u.HandSession != "" && st.fetchedCall[u.HandSession] > u.HandCall
}

// countHandOut counts d's unit in the harness segment it was handed out in
// (its Segment, else, in a journal from before segments, the current one)
// and returns that segment's start.
func (st *runState) countHandOut(d *HandedOutData) int64 {
	start := d.Segment
	for i := len(st.segments) - 1; i >= 0; i-- {
		s := &st.segments[i]
		if start == 0 || s.Start == start {
			if s.units == nil {
				s.units = map[string]bool{}
			}
			s.units[d.Name] = true
			if s.Harness == "" && d.Session != "" && d.Harness != "" {
				// A segment with no coordinator record binds from its first
				// session-bearing hand-out: a journal from before segments,
				// where a known session resumed with no new record, and a
				// snapshot fork's history, whose coordinator records are
				// notes. A live run writes the record before any hand-out.
				s.Harness, s.Root, s.Session = normalizeHarness(d.Harness), d.Root, d.Session
			}
			return s.Start
		}
	}
	return start
}
