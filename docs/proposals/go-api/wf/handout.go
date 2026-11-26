package wf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Coordinator is who called next: its harness, read from its own environment
// on each call (never the run creator's), and its session.
type Coordinator struct {
	Harness string
	Session string
	// Limit is the harness's own limit on concurrent subagents, read by its
	// adapter from the coordinator's configuration, and LimitSource where;
	// 0 when unknown (E3).
	Limit       int
	LimitSource string
	// Children is how many children the coordinator runs at once, as it
	// declared it (next --children); 0 when it declared none. It overrides
	// Limit. The engine sets no cap of its own: it holds its offers to the
	// coordinator's (childCap).
	Children int
	// Wait is how long a blocking next of this coordinator waits with nothing
	// for it: its harness's tool limit less a margin, 0 when the adapter
	// cannot tell (H2).
	Wait time.Duration
	// Root is where the coordinator's harness keeps its records, from its
	// environment; empty for a library caller.
	Root string
	// ExpectedAdmission is the generation fenced into a Stop continuation.
	ExpectedAdmission int64
	// OutBytes and OutLines are the most one tool call's output may hold in
	// the coordinator's harness (0: no limit): a blocking next's text offers
	// no more units than fit, the rest left for the next call. The JSON
	// hand-out sets neither.
	OutBytes, OutLines int
}

// childCap is how many children co runs at once: its declared count, else
// its harness's own limit, 0 when neither is known (no cap).
func (co Coordinator) childCap() (n int, source string) {
	if co.Children > 0 {
		return co.Children, ChildrenSource
	}
	return co.Limit, co.LimitSource
}

// ChildrenSource is the source an offer names for a child cap the
// coordinator declared itself.
const ChildrenSource = "declared with --children"

// NextAuto is n left out of blocking next (E3): up to the harness's own limit
// less the coordinator's children running or starting, where the adapter
// reads one, otherwise every ready unit.
const NextAuto = -2

// OfferView is how blocking next with n left out counted its offer (E3).
type OfferView struct {
	// Limit is the harness's own limit, 0 when the adapter reads none, and
	// Source where it was read.
	Limit  int
	Source string
	// Busy is the caller's children running or still starting.
	Busy int
}

// HandOutAs hands out up to n ready agent units (n < 0: every one) to the
// coordinator co, with the run's other ready instructions. A coordinator the
// run has not seen is journaled first (coordinator). A next from a new
// session also clears what an earlier coordinator left: each unit claimed
// under another session with no live child is offered again (orphans), and
// each unit handed to another session and never claimed is re-offerable.
func (e *Engine) HandOutAs(run RunID, n int, co Coordinator) ([]Instruction, error) {
	var out []Instruction
	err := e.withCoordinatorNext(run, co, func(c *cycle) error {
		var err error
		out, err = c.handOutCycle(n, co)
		if err != nil {
			return err
		}
		return c.prepareTurnHandoff(out)
	})
	return out, err
}

// handOutCycle is one locked hand-out to co: drive, meet the coordinator,
// then the run's instructions: the ended, stopped or waiting one alone, or
// every non-agent instruction and up to n agent units, handed out.
func (c *cycle) handOutCycle(n int, co Coordinator) ([]Instruction, error) {
	if co.Harness == "" {
		co.Harness, _ = recordedHarness(*c.st.started) // a caller whose environment shows none
	}
	c.caller = co
	if c.st.childSessions[co.Session] || c.st.refusedChildSessions[co.Session] {
		return nil, fmt.Errorf("%w: next: this session was identified as a child of this run; report to its coordinator and end the child task", ErrRejected)
	}
	c.requery = true
	c.handAt = c.now()
	if err := c.bindSegment(co); err != nil {
		return nil, err
	}
	// A session-less caller does not bind; no hand-out dispatches from a map
	// its harness refuses either (callerDispatch relies on this).
	if !c.st.paused && !c.st.ended {
		if _, err := c.currentModels(normalizeHarness(co.Harness)); err != nil {
			return nil, err
		}
	}
	if err := c.drive(); err != nil {
		return nil, err
	}
	if ins, done := c.untilUnitInstruction(); done && c.st.breakpoint == nil {
		return []Instruction{ins}, nil
	}
	if !c.st.ended && !c.st.paused {
		if ins, ok := c.reservedCoordinatorInstruction(); ok {
			return []Instruction{ins}, nil
		}
	}
	if !c.st.ended {
		if err := c.meetCoordinator(co); err != nil {
			return nil, err
		}
	}
	if ins, done := c.untilUnitInstruction(); done && c.st.breakpoint == nil {
		return []Instruction{ins}, nil
	}
	// Replacing a coordinator can recover an orphan and drive it into a gate.
	if !c.st.ended && !c.st.paused {
		if ins, ok := c.reservedCoordinatorInstruction(); ok {
			return []Instruction{ins}, nil
		}
	}
	switch {
	case c.st.ended:
		return []Instruction{c.endedInstruction()}, nil
	case c.st.paused:
		return []Instruction{c.stoppedInstruction()}, nil
	case c.waitingOnHolds():
		return []Instruction{c.waitingInstruction()}, nil
	}
	// Never more offers at once than the coordinator's child cap: up to the
	// cap less its children running or still starting. At its cap it is
	// offered nothing, and a blocking next waits for a child to finish (or
	// anything else for it).
	slots, source := co.childCap()
	if n == NextAuto {
		c.offer = &OfferView{Limit: slots, Source: source}
		n = -1
	}
	if slots > 0 {
		busy := c.busyFor(co)
		if c.offer != nil {
			c.offer.Busy = busy
		}
		if free := max(0, slots-busy); n < 0 || n > free {
			n = free
		}
	}
	var out []Instruction
	for _, id := range c.st.order {
		if ins, ok := c.nonAgentInstruction(c.st.insts[id]); ok && c.untilUnitAllows(c.st.insts[id]) {
			out = append(out, ins)
		}
	}
	order := c.handOutOrder(append(c.expiredHandOuts(), c.pendingUnits()...))
	if n >= 0 && n < len(order) {
		order = order[:n]
	}
	if fit := c.unitsThatFit(order, out, co, false); fit < len(order) {
		c.more, order = len(order)-fit, order[:fit]
	}
	for _, in := range order {
		notStarted := false
		switch u := in.Unit; {
		case u == nil || u.State != UnitHanded || u.HandSession == "":
		case c.displacedBy(co, u.HandSession, u.HandCall, c.admissionOf(co)):
			// Handed to a replaced coordinator and never claimed: it never
			// spawned it, or its child died before the prompt fetch.
			cause := fmt.Sprintf("handed to coordinator session %s, replaced by %s, and never claimed", u.HandSession, co.Session)
			if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: u.Name, Kind: DegradedOrphan, Cause: cause, Action: "handed out again"}); err != nil {
				return nil, err
			}
		case c.passedOver(in, co):
			notStarted = true
			cause := c.unclaimedReofferCause(u)
			if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: u.Name, Kind: DegradedUnstarted, Cause: cause, Action: "handed out again"}); err != nil {
				return nil, err
			}
		}
		prev := lastErrLog(in)
		ins, offered, err := c.handOut(in)
		if err != nil {
			return nil, err
		}
		if !offered {
			continue
		}
		if notStarted {
			markNotStarted(&ins, prev)
		}
		out = append(out, ins)
	}
	c.advise(out)
	return out, nil
}

// loseOrphan loses in's unit as an orphan of a replaced coordinator, with a
// degraded record (loseNoted).
func (c *cycle) loseOrphan(in *instance, cause string) error {
	return c.loseNoted(in, DegradedOrphan, cause, LostCauseOrphan)
}

// liveCoordinator is the earlier coordinator session co would sweep that is
// live: one with a claimed unit of a harness whose children die with
// their coordinator, whose blocking next holds its lock now (coordinator_lock.go),
// and that no wf stop stopped since it last took units. A mistaken parallel
// session is then refused, loudly, instead of re-offering every running child
// of a live coordinator. pid is what the live call wrote.
func (c *cycle) liveCoordinator(co Coordinator, admission int64) (session, pid string) {
	checked := map[string]bool{}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		u := in.Unit
		if u == nil || in.Settled || !u.State.claimed() || u.Claim == nil {
			continue
		}
		old := u.childOwner()
		if checked[old] || !c.displacedBy(co, old, u.childOfferSeq(), admission) || !childDiesWithCoordinator(u) {
			continue
		}
		checked[old] = true
		if last := c.st.coordLastAt[old]; !c.st.pausedAt.IsZero() && !c.st.pausedAt.Before(last) {
			continue // stopped since it last took units
		}
		if live, pid := c.e.coordinatorLive(c.run, old); live {
			return old, pid
		}
	}
	return "", ""
}

// bindSegment checks co against the run's current harness segment and,
// while the segment is unbound, binds it to co with a coordinator record
// . It runs under the hand-out's lock before anything drives or
// returns, so of two first callers of a new segment the second is checked
// against the first's binding; and it binds even for a session known from
// an earlier segment. A session-less caller (a library's) is neither checked
// nor binds, and a stopped run's segment is not bound (its resume opens the
// next).
//
// Another harness is refused: the spawn call and the model map are a
// harness's. A coordinator session the run already knows, calling from
// another records root than the segment's current one (a Claude session moved
// to another profile, so another CLAUDE_CONFIG_DIR, keeps its id), goes on,
// with one degraded note per change; the note moves the segment's root, so
// the root a session vacated is another root from then on.
// A session the run has not seen under another root is refused as another
// harness is: admitted, it would replace the segment's coordinator and
// retire its live children at once (meetCoordinator), on no more evidence
// than its own environment. No new segment is opened for a moved session: a segment's root
// is read by this check alone, and each hand-out records its own caller's
// root (HandedOutData.Root), which is where its child's records are looked
// up.
func (c *cycle) bindSegment(co Coordinator) error {
	cur := c.st.segment()
	if co.Session == "" || cur == nil || c.st.ended {
		return nil
	}
	bound, root := c.segmentDriver()
	if bound != "" && normalizeHarness(co.Harness) != bound {
		return fmt.Errorf("%w: next: %s", ErrRejected, mismatchOtherHarness(bound, normalizeHarness(co.Harness)))
	}
	if bound != "" && root != "" && co.Root != "" && co.Root != root {
		if !c.knownCoordinator(co.Session) {
			return fmt.Errorf("%w: next: %s", ErrRejected, mismatchUnknownSession(bound))
		}
		// The record moves the segment's root (its fold), so the next call
		// under co.Root matches and writes nothing: one record per change.
		d := DegradedData{Kind: DegradedRootChanged, Root: co.Root, Cause: fmt.Sprintf("the coordinator's %s records root changed from %s to %s", bound, root, co.Root),
			Action: "the run goes on: units handed out from here record the new root; a child claimed under the old one is still read there"}
		if err := c.emitAs("", "", 0, JournalDegraded, 0, d); err != nil {
			return err
		}
	}
	if cur.Harness != "" || c.st.paused {
		if cur.Harness != "" && !c.st.paused && c.st.checkedSegment != cur.Start {
			return c.checkChildHarnesses(normalizeHarness(cur.Harness))
		}
		return nil
	}
	h := normalizeHarness(co.Harness)
	sm, err := c.currentModels(h)
	if err != nil {
		// Nothing is journaled and the segment stays unbound: the map its
		// boundary verb gave holds a value this harness does not take. The
		// boundary verb re-issued with a corrected map opens a fresh segment.
		return fmt.Errorf("%w (the segment is bound by %s): re-issue the boundary verb (wf resume, rewind or fork) with a corrected --model-map; never retry next", err, h)
	}
	if err := c.emitAs("", "", 0, JournalCoordinator, 0, CoordinatorData{Harness: co.Harness, Session: co.Session, Root: co.Root, SegmentStart: cur.Start, Models: &sm}); err != nil {
		return err
	}
	if err := c.checkChildHarnesses(h); err != nil {
		return err
	}
	return c.segmentModelsNote(h, sm)
}

// segmentModelsNote journals, at a segment's binding, what a person must know
// about its models: the run moved to another harness with no
// --model-map, so that harness's defaults apply; or the harness has no model
// for some tiers the definition uses.
// missingTiers is names as tiers.
func missingTiers(names []string) []Tier {
	out := make([]Tier, len(names))
	for i, n := range names {
		out[i] = Tier(n)
	}
	return out
}

func (c *cycle) segmentModelsNote(h string, sm SegmentModels) error {
	var missing []string
	for _, t := range collectTiers(c.st.def) {
		if _, ok := sm.Models[string(t)]; !ok {
			missing = append(missing, string(t))
		}
	}
	var dg *DegradedData
	_, noModel := noModelHarness(h)
	switch {
	case noModel:
		if sm.From != ModelsFromStart {
			dg = unresolvedTiersNote(h, missingTiers(missing), sm.From == ModelsFromMap && len(sm.ModelMap) > 0)
		}
	case sm.From == ModelsFromDefaults:
		i := len(c.st.segments) - 1
		dg = &DegradedData{Kind: DegradedTiers,
			Cause:  fmt.Sprintf("the run moved from %s to %s with no --model-map: the earlier map does not carry over to another harness", c.segmentHarness(i-1), h),
			Action: fmt.Sprintf("%s defaults apply: %s; stop and resume with --model-map to choose", h, tiersOf(sm.Models))}
		if len(missing) > 0 {
			dg.Action += fmt.Sprintf("; no model for tier(s) %s: those children run on the session's model", strings.Join(missing, ", "))
		}
	case len(missing) > 0 && sm.From != ModelsFromStart:
		dg = unresolvedTiersNote(h, missingTiers(missing), false)
	}
	if dg == nil {
		return nil
	}
	return c.emitAs("", "", 0, JournalDegraded, 0, *dg)
}

// checkSegment refuses a coordinator verb (recover, heartbeat, an agent's
// started) whose caller is not of the current segment's harness, inside the
// verb's own mutation lock: a segment bound between the CLI's
// preflight and the mutation still refuses it. It binds nothing. A library
// caller, a session-less one and a person's operator form are not checked.
// A coordinator session the run knows, under another records root, is
// served: its next journals the change (bindSegment). A session it has not
// seen under another root is refused, as by next.
func (c *cycle) checkSegment(verb string, who harness.Caller) error {
	if !who.FromEnv || who.Session == "" || c.e.opts.operatorChild {
		return nil
	}
	h, root := c.segmentDriver()
	switch {
	case h == "":
	case normalizeHarness(who.Harness) != h:
		return fmt.Errorf("%w: %s: %s", ErrRejected, verb, mismatchOtherHarness(h, normalizeHarness(who.Harness)))
	case root != "" && who.Root != "" && who.Root != root && !c.knownCoordinator(who.Session):
		return fmt.Errorf("%w: %s: %s", ErrRejected, verb, mismatchUnknownSession(h))
	}
	return nil
}

// knownCoordinator reports whether the journal already names session as a
// coordinator of this run.
func (c *cycle) knownCoordinator(session string) bool {
	_, ok := c.st.coordOrder[session]
	return ok
}

// recordCoordinator journals a named session without driving the run.
func (c *cycle) recordCoordinator(co Coordinator) error {
	if co.Session == "" || c.st.lastCoordinator == co.Session {
		return nil
	}
	return c.emitAs("", "", 0, JournalCoordinator, 0, CoordinatorData{Harness: co.Harness, Session: co.Session, Root: co.Root})
}

// meetCoordinator clears what earlier coordinators left, on every next from
// a named session (B4). Where the claim's harness runs children inside their
// coordinator's process (Claude Code subagents, Codex threads), each unit
// claimed under a session this one replaced (see supersedes) in this segment
// died with it: it is lost as an orphan, exempt from the lost cap, with a
// degraded record, and offered again at once. Elsewhere the child may outlive
// its coordinator and nothing is retired on elapsed time: it finishes, or its
// lease runs out and the new coordinator is asked. A session-less caller
// clears nothing: the engine cannot tell it from the coordinator before it.
// An older session coming back clears nothing of a newer one's.
func (c *cycle) meetCoordinator(co Coordinator) error {
	if co.Session == "" {
		return nil
	}
	if live, _ := c.liveCoordinator(co, c.admissionOf(co)); live != "" {
		return fmt.Errorf("%w: next: another coordinator session of this run is live, and this session is not it. If that session is gone, its next ends within its harness's tool limit: run next again then", ErrRejected)
	}
	if err := c.recordCoordinator(co); err != nil {
		return err
	}
	lost := false
	for _, id := range slices.Clone(c.st.order) {
		in := c.st.insts[id]
		if in == nil || c.st.ended || !c.untilUnitAllows(in) {
			continue
		}
		u := in.Unit
		if u == nil || in.Settled || !u.State.claimed() || u.Claim == nil || !c.displacedBy(co, u.childOwner(), u.childOfferSeq(), c.admissionOf(co)) {
			continue
		}
		name, old := u.Name, u.childOwner()
		// A child dies with its coordinator only within a segment (a crash
		// restart), and only on a harness whose children run inside it.
		// Across a boundary (a stop and resume, a rewind) nothing says the old
		// coordinator or its children exited. A child that outlives
		// its coordinator is never retired on elapsed time: silence is no
		// evidence of death (rule 5). It finishes and submits, or its lease
		// runs out and the new coordinator is asked.
		if !childDiesWithCoordinator(u) || u.HandSegment != c.st.segment().Start {
			continue
		}
		cause := fmt.Sprintf("coordinator session %s was replaced by %s; its child of %s ran inside it (%s) and died with it", old, co.Session, name, u.Claim.Harness)
		if err := c.loseOrphan(in, cause); err != nil {
			return err
		}
		lost = true
	}
	if lost {
		return c.drive()
	}
	return nil
}

// handOutOrder ranks the pending units of a run for wf next <n>.
//
// Owns: the hand-out order: lost reissues first (a unit with a Predecessor), then
// the critical path (the longest remaining graph depth of the unit's state,
// computed once per Def from the Inspect walk), then definition order (the
// instance order of runState.order).
// Hides: the tie-break rules.
// Does not: hand anything out. Engine.HandOut takes the first n and journals
// handed_out; Status and the ETA's critical-path bound read the same order.
// Extend by: a new tie-break goes only here; callers never sort pending units.
func (c *cycle) handOutOrder(pending []*instance) []*instance {
	pos := make(map[string]int, len(c.st.order))
	for i, id := range c.st.order {
		pos[id] = i
	}
	depth := make(map[*instance]int, len(pending))
	for _, in := range pending {
		depth[in] = c.depth(in)
	}
	out := append([]*instance(nil), pending...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if la, lb := lostReissue(a), lostReissue(b); la != lb {
			return la
		}
		if depth[a] != depth[b] {
			return depth[a] > depth[b]
		}
		return pos[a.ID] < pos[b.ID]
	})
	return out
}

// lostReissue reports whether the instance's pending unit replaces a lost one.
func lostReissue(in *instance) bool { return in.Unit != nil && in.Unit.Predecessor != "" }

// pendingUnits are the instances whose open agent instruction has no unit
// handed out yet: a fresh one, a lost one back at pending, or a reissue after
// the previous attempt's unit settled.
func (c *cycle) pendingUnits() []*instance {
	var out []*instance
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if !c.untilUnitAllows(in) || in.Phase != phaseIssued || in.Issued == nil || in.Settled {
			continue
		}
		if u := in.Unit; u == nil || u.State == UnitPending || (u.State.terminal() && u.Token != in.Token) {
			out = append(out, in)
		}
	}
	return out
}

// expiredHandOuts are the instances whose handed unit, offered and never
// claimed, the next hand-out offers again under the same name (reofferable).
func (c *cycle) expiredHandOuts() []*instance {
	var out []*instance
	for _, id := range c.st.order {
		if in := c.st.insts[id]; c.untilUnitAllows(in) && c.reofferable(in) {
			out = append(out, in)
		}
	}
	return out
}

// reofferable reports whether in's handed unit is offered again. Handed is not
// claimed, and nothing expires before a claim: a unit is claimed when its
// child fetches the prompt, however long the spawn takes (B1). It is offered
// again after the same coordinator passes it over or reaches its grace, a
// newer session replaces it, or the current driver finds the previous
// driver's window expired. A sessionless hand-out is offered again after
// the window while no unit is claimed. A child from an earlier offer can
// still claim it with its own token; a second child is told it is taken (B3).
func (c *cycle) reofferable(in *instance) bool {
	u := in.Unit
	if u == nil || in.Settled || u.State != UnitHanded {
		return false
	}
	if cur := c.st.segment(); cur != nil && u.HandSegment != 0 && u.HandSegment != cur.Start {
		// An earlier segment's unclaimed offer expired at the boundary: the
		// current segment's coordinator is offered it, whatever the order of
		// sessions.
		return true
	}
	if u.HandSession != "" {
		return c.displacedBy(c.caller, u.HandSession, u.HandCall, c.admissionOf(c.caller)) || c.unclaimedReofferCause(u) != ""
	}
	due, ok := c.idleReoffer(u)
	return ok && !c.now().Before(due)
}

// idleReoffer is when u, handed and unclaimed, is offered again to the same
// coordinator: its hand-out's Expires (HandOutWindow after it), while no unit
// of the run is claimed. Expires is no deadline: nothing is lost at it. While any child runs, the coordinator is alive and nothing is
// re-offered to it.
func (c *cycle) idleReoffer(u *unit) (time.Time, bool) {
	for _, id := range c.st.order {
		if in := c.st.insts[id]; in.Unit != nil && !in.Settled && in.Unit.State.claimed() {
			return time.Time{}, false
		}
	}
	return u.HandExpires, true
}

// unitCounts counts the subagent units a coordinator can act on: ready (pending,
// or handed out and expired unclaimed, which the next hand-out offers again),
// handed (offered and not yet claimed) and running (claimed).
func (c *cycle) unitCounts() UnitCounts {
	n := UnitCounts{Ready: len(c.pendingUnits())}
	for _, id := range c.st.order {
		in := c.st.insts[id]
		u := in.Unit
		switch {
		case u == nil:
		case u.State.claimed():
			n.Running++
		case u.State == UnitHanded && !in.Settled:
			if c.reofferable(in) {
				n.Ready++
			} else {
				n.Handed++
			}
		}
	}
	return n
}

// busyFor is how many of co's children are running or still starting: its
// claimed units and its unclaimed hand-outs (E3). A session-less
// caller counts every claimed unit of the run and every unclaimed
// session-less hand-out.
func (c *cycle) busyFor(co Coordinator) int {
	n := 0
	for _, id := range c.st.order {
		in := c.st.insts[id]
		u := in.Unit
		if u == nil || in.Settled {
			continue
		}
		switch {
		case u.State.claimed():
			if co.Session == "" || u.childOwner() == co.Session {
				n++
			}
		case u.State == UnitHanded:
			// A session-less caller's offers carry no session: each one
			// not yet offered again is starting.
			if u.HandSession == co.Session && !c.reofferable(in) {
				n++
			}
		}
	}
	return n
}

// newClaimToken mints the claim token of one hand-out.
func newClaimToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// handOut journals handed_out for in and returns its paged instruction. A unit
// whose hand-out window expired unclaimed keeps its name; each hand-out mints a
// fresh claim token. The brief is computed and checked first (briefProblem): a
// brief the coordinator's harness cannot carry halts the run naming the unit
// and the reason, nothing is journaled as handed out, and the bool is false.
//
// Order before the hand-out is journaled: compute the brief, the stderr-path
// check (adapters with StartLog), the brief check (adapters with BriefChecker),
// the child-log directory, then the journal. Keep it when adding a step.
func (c *cycle) handOut(in *instance) (Instruction, bool, error) {
	name := c.offerName(in)
	// A re-offer keeps the spelling its first hand-out recorded; a new unit
	// gets the literal slug path.
	rel := slugOutput(name)
	if u := in.Unit; u != nil && u.State == UnitHanded && u.Name == name {
		rel = u.Output
	}
	output, err := c.outputAt(name, rel)
	if err != nil {
		return Instruction{}, false, err
	}
	claim := newClaimToken()
	brief := c.brief(claim)
	dispatch := c.callerDispatch(in)
	if dispatch.Family != "" {
		// A unit of another harness never falls back to the coordinator's:
		// its harness unavailable now (another coordinator, a failed
		// preflight) fails it with class Unavailable.
		why := c.familyUnavailable(string(dispatch.Family))
		if harness.HeadlessFor(string(dispatch.Family)) == nil && why == "" {
			why = fmt.Sprintf("family %s has no harness with a headless launch for coordinator harness %s", dispatch.Family, normalizeHarness(c.caller.Harness))
		}
		if why != "" {
			return Instruction{}, false, c.fail(in, in.node(), Unavailable, []string{why}, "")
		}
	}
	errLog := ""
	if startLogOf(c.coordinatorAdapter()) != nil || dispatch.Family != "" {
		errLog = c.childErrLog(claim) // spawn writes a headless child's stderr there
	}
	if errLog != "" && !errLogPathOK(errLog) {
		return Instruction{}, false, c.halt(in, fmt.Sprintf("hand-out of %s refused: the stderr file path %s holds a character the pi launch line cannot carry (only letters, digits and / . _ - are allowed): use a state dir and run id without them", name, errLog))
	}
	if why := c.briefProblem(brief, claim); why != "" {
		return Instruction{}, false, c.halt(in, fmt.Sprintf("hand-out of %s refused: %s", name, why))
	}
	if errLog != "" {
		if err := makeChildLogDir(c.e.opts.StateRoot, filepath.Dir(errLog)); errors.Is(err, errNotRealDir) {
			return Instruction{}, false, c.halt(in, fmt.Sprintf("hand-out of %s refused: %v, where the child log directory goes under the state dir (a symlink or another file is in its place): remove it", name, err))
		} else if err != nil {
			return Instruction{}, false, fmt.Errorf("hand-out of %s: create the child log directory: %w", name, err)
		}
	}
	if c.handCall == 0 {
		c.handCall = c.nextSeq() // this hand-out's own Seq
	}
	if err := c.emit(in, JournalHandedOut, 0, HandedOutData{
		Name: name, Token: in.Token, Session: c.caller.Session, Harness: c.caller.Harness, Root: c.caller.Root, Expires: c.now().Add(c.e.opts.HandOutWindow), Claim: claim, Output: rel, Segment: c.st.segment().Start,
		Call: c.handCall, Spawn: &HandOutSpawn{Model: dispatch.Model, Effort: dispatch.Effort, ErrLog: errLog, Harness: string(dispatch.Family)},
	}); err != nil {
		return Instruction{}, false, err
	}
	return Instruction{
		Run: c.run, State: c.st.qualified(in), Kind: InstructionAgent, Attempt: in.Attempt,
		Name: name, Output: output, Class: c.st.qualified(in), Brief: brief, Child: in.ID,
		Dispatch: dispatch, Spawn: c.spawnCommand(claim, dispatch), ErrLog: errLog, GitAuthority: in.Issued.GitAuthority, Workspace: in.Issued.Workspace, Context: ContextFresh,
		PreviousRefusal: previousRefusal(in),
	}, true, nil
}

// spawnCommand is the spawn verb's line for a unit of another harness, ""
// for one of the coordinator's own.
func (c *cycle) spawnCommand(claim string, d DispatchHints) string {
	if d.Family == "" {
		return ""
	}
	return c.verbLine("spawn", "--claim", claim)
}

// childDiesWithCoordinator: u's child runs inside its coordinator's process
// (its harness's subagents). A headless child is its own process session
// and outlives a replaced coordinator.
func childDiesWithCoordinator(u *unit) bool {
	return u.ChildHarness == "" && harness.For(u.Claim.Harness).ChildrenDieWithCoordinator()
}

// coordinatorAdapter is the adapter of the coordinator being handed to.
func (c *cycle) coordinatorAdapter() harness.Adapter {
	return harness.For(normalizeHarness(c.caller.Harness))
}

// markNotStarted turns ins into the re-offer of a unit never started, with
// the stderr file its earlier hand-out recorded (prev) and that file's last
// lines. The pointer is read only for an adapter with StartLog; prev is empty
// for any other, which recorded none.
func markNotStarted(ins *Instruction, prev string) {
	ins.NotStarted = true
	ins.PrevErrLog = prev
	if prev != "" {
		ins.PrevErrTail = errLogExcerpt(prev)
	}
}

// lastErrLog is the stderr file the latest hand-out of in's unit recorded
// (read before a new hand-out folds), "" for none.
func lastErrLog(in *instance) string {
	if u := in.Unit; u != nil && len(u.Offered) > 0 {
		return u.errLogOf(u.Offered[len(u.Offered)-1])
	}
	return ""
}

// briefChecker is the coordinator's adapter as a BriefChecker, or nil when
// its harness has none. It is the one place the assertion is made.
func (c *cycle) briefChecker() harness.BriefChecker {
	bc, _ := harness.For(normalizeHarness(c.caller.Harness)).(harness.BriefChecker)
	return bc
}

// briefProblem is why the coordinator's harness cannot launch brief, or "".
// A refusal for a single quote names where it comes from, found by checking
// each source of the rendered brief in turn.
func (c *cycle) briefProblem(brief, claim string) string {
	bc := c.briefChecker()
	if bc == nil {
		return ""
	}
	err := bc.CheckBrief(brief)
	if err == nil {
		return ""
	}
	why := err.Error()
	if strings.Contains(brief, "'") {
		switch ws := c.workspace(); {
		case strings.Contains(ws, "'"):
			why += "; it comes from the workspace path " + ws
		case strings.Contains(c.proseWork(), "'"):
			why += "; it comes from the definition's work text"
		default:
			why += "; it comes from the command line " + c.verbLine("prompt", "--claim", claim)
		}
	}
	return why
}

// proseWork is the definition's work text as the brief carries it: for a
// harness that checks briefs, each prose apostrophe becomes U+2019.
func (c *cycle) proseWork() string {
	if c.briefChecker() != nil {
		return proseApostrophes(c.st.work())
	}
	return c.st.work()
}

// proseApostrophes replaces each ASCII ' with a letter on both sides (don't,
// user's) by U+2019. Any other ' is a quoted shell argument and stays.
func proseApostrophes(s string) string {
	rs := []rune(s)
	for i, r := range rs {
		if r == '\'' && i > 0 && i+1 < len(rs) && unicode.IsLetter(rs[i-1]) && unicode.IsLetter(rs[i+1]) {
			rs[i] = '\u2019'
		}
	}
	return string(rs)
}

// callerDispatch is the dispatch of in's unit for the coordinator that asked
// (A3a, an earlier revision): the state's hints resolved for the caller's harness under
// the current segment's models, so a hand-out after a boundary spawns with the
// new segment's map. The issued dispatch stands for a non-agent unit.
func (c *cycle) callerDispatch(in *instance) DispatchHints {
	k, ok := in.node().kind.(*agentKind)
	if !ok {
		return in.Issued.Dispatch
	}
	if rm := c.e.replayModels; rm != nil {
		// A replay hands out as the recorded hand-out of this issue that a
		// child claimed did; the issued record keeps the issue's dispatch.
		d := in.Issued.Dispatch
		if m, e, ok := rm.handOut(in.ID, in.Cur, c.st.issuedAt[replayKey(in.ID, in.Cur)]-1); ok {
			d.Model, d.Effort = m, e
		}
		return d
	}
	h := normalizeHarness(c.caller.Harness)
	if h == "" {
		h = c.segmentHarness(len(c.st.segments) - 1)
	}
	// handOutCycle refused a map this harness does not take before any
	// hand-out, so the error is unreachable here; none is shown models-less.
	// A family with no harness for this coordinator keeps its unresolved
	// name, which handOut refuses (familyUnavailable).
	hints := hintsOf(k.dispatch)
	d, err := c.resolveDispatch(hints, h, shown(c.currentModels(h)))
	if err != nil {
		return hints
	}
	return d
}

// brief is the spawn brief of a unit (C2b, I1): the prompt command first,
// then the tool-directory instruction and the short rules for the run
// (RulesForSpawnBrief). The child's work and rules come from that command.
func (c *cycle) brief(claim string) string {
	unit := whatThisUnitRuns(c.proseWork()) + "\n"
	if c.briefBare {
		unit = ""
	}
	return briefText(c.verbLine("prompt", "--claim", claim)) + "\n" + unit + toolWorkdirAdvice(c.workspace()) + spawnRules(c.st.def.usesTracker()) + "\n"
}

// childRules closes every fetched prompt: how the child finishes, and the
// refused-versus-failed and escalation rules it is the one to meet (C2).
func (c *cycle) childRules(name, output, claim string) string {
	l := func(verb string, args ...string) string {
		if claim != "" {
			args = append([]string{"--claim", claim}, args[1:]...)
		}
		return c.verbLine(verb, args...)
	}
	done, validate := l("done", name, "--output", "<file>"), l("validate", name, "--output", "<file>")
	if output != "" {
		done, validate = l("done", name), l("validate", name)
	}
	return fmt.Sprintf(`## Finishing

You are subagent %[1]s. Each command below is one line; by default run it as printed.
- Check your result first (it checks exactly as done does and records nothing); fix what it names and run it again until it says ok:
  %[5]s
- Then finish. A refused done spends your attempt; never re-run a command that was accepted:
  %[2]s
  Pass --usage tokens=N to done when your harness shows the tokens this task spent; otherwise omit it.
- If you cannot do the work:
  %[3]s
- When a command exits 5 saying your attempt is still open, do what it says and run it again. When one exits 9 (the run was busy, nothing was changed), run the same command again. When done says this attempt is spent, the run has recorded it: do not run failed; tell your coordinator the output and end your task. On any other non-zero exit of done, run the failed command above with --reason saying what blocked you and what you need, so the run can route it; if failed itself exits non-zero, tell your coordinator its output.
- If the coordinator can present an approval prompt that you cannot, use failed with --class approval and quote the exact command or tool call in --reason. Never work around the refusal.
- Any other declined tool call or work that needs a missing permission (network in a no-network sandbox) is blocked: never work around it; finish with:
  %[4]s
- A command that ran and failed (EROFS on a default cache path, a failing test) is ordinary work: fix it inside your own workspace, for example with a workspace-local cache, and go on.
- When the sandbox blocks a command the task needs, request escalation, and pass --escalated '<command>: <reason>: <outcome>' to done or failed for each one, the outcome a short phrase without ": " (ran, refused by reviewer). If escalation is refused, that is still blocked unless your coordinator can present the approval prompt you could not access.
- Run any command expected to take more than a few minutes with your harness's own background-task tool and check on it with that tool, so your transcript keeps moving, rather than a polling or wait loop.`,
		name, done, l("failed", name, "--reason", "<text>"), l("failed", name, "--class", "blocked", "--reason", "<cause>"), validate)
}

// finishLine is the last line of every prompt body (E1): the finishing
// commands again, where a child that read the whole file ends.
func (c *cycle) finishLine(name, output, claim string) string {
	l := func(verb string, args ...string) string {
		if claim != "" {
			args = append([]string{"--claim", claim}, args[1:]...)
		}
		return c.verbLine(verb, args...)
	}
	done, validate := l("done", name), l("validate", name)
	if output == "" {
		done, validate = l("done", name, "--output", "<file>"), l("validate", name, "--output", "<file>")
	}
	return fmt.Sprintf("Finish: when your result is written, check it until it says ok:\n  %s\nthen run:\n  %s\nIf you cannot do the work, run:\n  %s", validate, done, l("failed", name, "--reason", "<text>"))
}

// childPage is what wf prompt prints (E1), always short: who the child is,
// the finishing rules, which hold whatever the body says, and the body's path
// to read with the harness's own file reader, which pages a large file
// itself. No rule a child must obey is only in the body.
func (c *cycle) childPage(name, output, workspace, body, claim string) string {
	l := func(verb string, args ...string) string {
		if claim != "" {
			args = append([]string{"--claim", claim}, args[1:]...)
		}
		return c.verbLine(verb, args...)
	}
	var b strings.Builder
	b.WriteString(toolWorkdirAdvice(c.workspace()))
	fmt.Fprintf(&b, "You are subagent %s of workflow run %s. Instructions:\n  %s\n", name, c.run, body)
	b.WriteString("Read it all with your file reader first, even if task rules forbid it.\n")
	b.WriteString("Whatever the file says, and where your task text disagrees, this page wins:\n")
	if workspace != "" {
		fmt.Fprintf(&b, "- Work in %s.\n", workspace)
	}
	const check = "  First run it with validate in place of done; fix your result until it says ok (a refused done spends your attempt).\n"
	if output != "" {
		fmt.Fprintf(&b, "- Write your result as JSON to %s (schema in the file), then run:\n  %s\n%s", output, l("done", name), check)
	} else {
		fmt.Fprintf(&b, "- Write your result as JSON to a file outside any git worktree (schema in the file), then run:\n  %s\n%s", l("done", name, "--output", "<file>"), check)
	}
	b.WriteString("- Cannot do it: use failed in place of done with --reason <text>. If only your coordinator can prompt for approval, add --class approval and quote the exact command or tool call. Use --class blocked for other refusals. Never work around a refusal (escalate a sandbox block and pass it to --escalated).\n")
	b.WriteString("- Exit 9 (busy): rerun. Attempt spent: tell your coordinator. Else use failed, saying what you need.\n")
	b.WriteString("- Run long commands with your background-task tool, not a wait loop.\n")
	return b.String()
}

// outputPath is the allocated result path of the live unit name: the spelling
// its hand-out recorded, or, for a unit handed out before hand-outs recorded
// one, the legacy percent-encoded spelling. No result is created.
func (c *cycle) outputPath(name string) (string, error) {
	rel := ""
	if _, u, ok := c.st.unitByName(name); ok {
		rel = u.Output
	}
	return c.outputAt(name, rel)
}

// outputAt is the absolute path of rel under the run's out directory, its
// parent created; an empty rel is the legacy spelling of name.
func (c *cycle) outputAt(name, rel string) (string, error) {
	if rel == "" {
		return c.legacyOutputPath(name)
	}
	if c.e.opts.RunDir == nil || c.e.opts.RunDir(c.run) == "" {
		return "", nil
	}
	p, err := filepath.Abs(filepath.Join(c.e.opts.RunDir(c.run), "out", rel))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// slugMax bounds the readable part of an output file name, well inside the
// 255-byte component limit with the hash and suffix after it.
const slugMax = 200

// slugOutput is the output file name a hand-out allocates for a unit: a flat,
// literal name a shell or an agent writes as printed. Every character outside
// [A-Za-z0-9._-] becomes '-', and a short hash of the full name follows, so
// names that slug alike stay distinct. It holds no '/', so it cannot leave the
// out directory.
func slugOutput(name string) string {
	b := []byte(name)
	for i, ch := range b {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
			b[i] = '-'
		}
	}
	if len(b) > slugMax {
		b = b[:slugMax]
	}
	sum := sha256.Sum256([]byte(name))
	return string(b) + "-" + hex.EncodeToString(sum[:6]) + ".md"
}

// legacyOutputPath is the spelling hand-outs used before they recorded one:
// every component escaped, including dots and backslashes, so names cannot
// traverse the output root or alias percent-encoded names. Oversized
// components use a disjoint digest spelling. Journals written then keep it on
// resume.
func (c *cycle) legacyOutputPath(name string) (string, error) {
	if c.e.opts.RunDir == nil || c.e.opts.RunDir(c.run) == "" {
		return "", nil
	}
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.PathEscape(part), ".", "%2E")
		if part == "" {
			parts[i] = "%EMPTY"
		}
		limit := 255
		if i == len(parts)-1 {
			limit -= len(".md")
		}
		if len(parts[i]) > limit {
			// PathEscape never emits % followed by S, so a compacted component
			// cannot alias an ordinary escaped one (or the empty marker).
			parts[i] = fmt.Sprintf("%%SHA256-%x", sha256.Sum256([]byte(part)))
		}
	}
	p, err := filepath.Abs(filepath.Join(c.e.opts.RunDir(c.run), "out", filepath.Join(parts...)+".md"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// defDepths are a definition's critical depths: at is the depth of each state
// and after the depth that follows it, the depth minus the state's own weight.
// steps counts the states and edges examined, child Defs included, for the
// cost guard.
type defDepths struct {
	at, after map[string]int
	steps     int
}

// depth is the longest remaining depth of in: its own state's depth, plus, for
// a child instance, what the parent still has after the state that spawned it,
// up to the root.
func (c *cycle) depth(in *instance) int {
	n := c.e.defDepths(in.def).at[in.Cur]
	for ch := in; ch.ID != ""; {
		p := c.st.insts[ch.Parent]
		if p == nil {
			break
		}
		n += c.e.defDepths(p.def).after[ch.Spawner]
		ch = p
	}
	return n
}

// depths is the longest remaining depth of each state of d, as computeDepths
// defines it. It is computed once per Def and cached on the Engine (e.mu guards
// the map).
func (e *Engine) depths(d *Def) map[string]int { return e.defDepths(d).at }

func (e *Engine) defDepths(d *Def) *defDepths {
	e.mu.Lock()
	defer e.mu.Unlock()
	if dd, ok := e.depthCache[d]; ok {
		return dd
	}
	if e.depthCache == nil {
		e.depthCache = map[*Def]*defDepths{}
	}
	dd := computeDepths(d, map[*Def]bool{})
	e.depthCache[d] = dd
	return dd
}

// computeDepths walks inspectStates, the one walk over a definition's states.
// A state's own weight is 1 for an agent, its child Def's depth from the
// child's first state for a Sub or Map (0 for a child Def already being
// walked), and 0 otherwise. On an acyclic definition a state's depth is the
// heaviest path from it to a terminal. A loop is one node of the condensation:
// a strongly connected component weighs the sum of its states' own weights,
// and every state in it has the component's weight plus the deepest component
// reachable past it, so a loop's agents count once and its back edges count 0.
// after is the depth minus the state's own weight. Linear in states and edges:
// a longest simple path through overlapping loops is exponential, and Status
// and HandOut compute depths in a fresh process on every CLI call.
func computeDepths(d *Def, defPath map[*Def]bool) *defDepths {
	defPath[d] = true
	defer delete(defPath, d)
	states := ownStates(d)
	index := make(map[string]int, len(states))
	for i, sv := range states {
		index[sv.Name] = i
	}
	own := make([]int, len(states))
	dd := &defDepths{at: map[string]int{}, after: map[string]int{}}
	for i, sv := range states {
		switch {
		case sv.Kind == "agent":
			own[i] = 1
		case sv.child != nil && !defPath[sv.child] && len(sv.child.nodes) > 0:
			cd := computeDepths(sv.child, defPath)
			own[i] = cd.at[sv.child.nodes[0].name]
			dd.steps += cd.steps
		}
	}
	at, steps := condensedDepths(states, index, own)
	dd.steps += steps
	for i, sv := range states {
		dd.at[sv.Name] = at[i]
		dd.after[sv.Name] = at[i] - own[i]
	}
	return dd
}

// condensedDepths is the depth of each state on the condensation of the state
// graph (Tarjan, iterative so a deep definition cannot overflow the stack).
// Tarjan completes a component only after every component it reaches, so its
// depth is final when it pops. steps counts the states and edges examined.
func condensedDepths(states []StateView, index map[string]int, own []int) (at []int, steps int) {
	n := len(states)
	succ := make([][]int, n)
	for i, sv := range states {
		for _, e := range sv.Edges {
			if j, ok := index[e.To]; ok {
				succ[i] = append(succ[i], j)
			}
		}
	}
	idx, low, comp := make([]int, n), make([]int, n), make([]int, n)
	for i := range idx {
		idx[i], comp[i] = -1, -1
	}
	var compDepth, stack []int
	type frame struct{ v, next int }
	var call []frame
	counter := 0
	push := func(v int) {
		idx[v], low[v] = counter, counter
		counter++
		stack = append(stack, v)
		call = append(call, frame{v: v})
		steps++
	}
	for root := range states {
		if idx[root] >= 0 {
			continue
		}
		push(root)
		for len(call) > 0 {
			f := &call[len(call)-1]
			v := f.v
			if f.next < len(succ[v]) {
				w := succ[v][f.next]
				f.next++
				steps++
				switch {
				case idx[w] < 0:
					push(w)
				case comp[w] < 0: // on the stack: in v's component
					low[v] = min(low[v], idx[w])
				}
				continue
			}
			call = call[:len(call)-1]
			if len(call) > 0 {
				p := call[len(call)-1].v
				low[p] = min(low[p], low[v])
			}
			if low[v] != idx[v] {
				continue
			}
			c, k := len(compDepth), len(stack)
			for stack[k-1] != v {
				k--
			}
			k--
			weight, down := 0, 0
			for _, w := range stack[k:] {
				comp[w] = c
			}
			for _, w := range stack[k:] {
				weight += own[w]
				for _, x := range succ[w] {
					steps++
					if comp[x] != c {
						down = max(down, compDepth[comp[x]])
					}
				}
			}
			stack = stack[:k]
			compDepth = append(compDepth, weight+down)
		}
	}
	at = make([]int, n)
	for i := range at {
		at[i] = compDepth[comp[i]]
	}
	return at, steps
}

// briefText is a unit's brief: the child's whole task, addressed to the
// child (I1). The coordinator never runs it: next prints it between lines,
// under an offer addressed to the coordinator.
func briefText(promptLine string) string { return promptLine + briefCommandTail }

const briefCommandTail = "\nYou are a child agent for one unit of work. Run the command above, then do what it prints."

// Legacy brief framing remains readable for already-issued units.
const (
	briefHead = "You are a child agent for one unit of work. Run: "
	briefTail = "\nThen do what that prints."
)

// BriefCommand is the command a unit's brief tells its child to run; ok is
// false for text that is not a brief.
func BriefCommand(brief string) (cmd string, ok bool) {
	if first, rest, found := strings.Cut(brief, "\n"); found && first != "" && strings.HasPrefix("\n"+rest, briefCommandTail) {
		return first, true
	}
	if strings.HasPrefix(brief, toolWorkdirAdviceHead) {
		_, rest, found := strings.Cut(brief, "\n"+briefHead)
		if !found {
			return "", false
		}
		brief = briefHead + rest
	}
	rest, ok := strings.CutPrefix(brief, briefHead)
	cmd, ok2 := strings.CutSuffix(rest, briefTail)
	return cmd, ok && ok2
}

// childCommandFix is what a first prompt fetch from the coordinator's own
// session is told (I2).
const childCommandFix = "this is a child's command: start a child agent with the text between the lines next printed for it"

// relist shows co its own offered and unclaimed units again (J4): the end
// of a whole wait with nothing else is the evidence their spawns did not
// start. It repeats the offer the unit already has, under the same claim
// token, and journals no hand-out: a spawn that never starts would
// otherwise write a hand-out and mint a token on every wait, however short.
// What it journals is one never-started note per change of evidence
// (relistEvidence), so a wait that learned nothing new writes nothing. A
// fresh offer under a new token comes from the hand-out pass, not from
// here: once UnstartedGrace passed on the engine clock, a later offer's
// child fetched, a new coordinator session took over, or a stop and resume
// expired the offer. Every token the unit was offered under stays good, so
// a slow first child still binds it.
func (c *cycle) relist(co Coordinator) ([]Instruction, error) {
	if co.Session == "" || c.st.ended || c.st.paused || c.childrenFull(co) {
		return nil, nil
	}
	var mine []*instance
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if u := in.Unit; u != nil && !in.Settled && u.State == UnitHanded && u.HandSession == co.Session && !c.headlessLive(u) {
			mine = append(mine, in)
		}
	}
	// The re-list is an offer like any other: no more than co's child cap
	// less its children running (the re-listed units are every one it was
	// starting), and within the harness's tool-output limit, which only
	// shortens it further.
	if k, _ := co.childCap(); k > 0 {
		if free := max(0, k-c.claimedBy(co)); free < len(mine) {
			mine = mine[:free]
		}
	}
	if fit := c.unitsThatFit(mine, nil, co, true); fit < len(mine) {
		c.more, mine = len(mine)-fit, mine[:fit]
	}
	var out []Instruction
	for _, in := range mine {
		prev := lastErrLog(in)
		ins, offered, err := c.sameOffer(in)
		if err != nil {
			return nil, err
		}
		ins.SameClaim = offered // the text says so: two children of it would share the claim
		if !offered {
			// No offer of this session and segment to repeat: a fresh one.
			if ins, offered, err = c.handOut(in); err != nil {
				return nil, err
			}
		}
		if offered {
			markNotStarted(&ins, prev)
			out = append(out, ins)
		}
	}
	return out, nil
}

// sameOffer is the latest offer of in's unit as an instruction again: the
// same claim token, result file and stderr file, with no hand-out journaled.
// It journals a never-started note when the evidence differs from the one
// the unit's latest note recorded (relistEvidence), and nothing otherwise.
// The bool is false when the unit has no offer made to the calling session
// in the current segment.
func (c *cycle) sameOffer(in *instance) (Instruction, bool, error) {
	u := in.Unit
	cur := c.st.segment()
	if u == nil || len(u.Offered) == 0 || u.HandSession != c.caller.Session || cur == nil || (u.HandSegment != 0 && u.HandSegment != cur.Start) || in.Issued == nil {
		return Instruction{}, false, nil
	}
	claim := u.Offered[len(u.Offered)-1]
	output, err := c.outputAt(u.Name, u.Output)
	if err != nil {
		return Instruction{}, false, err
	}
	if cause := relistEvidence(u.errLogOf(claim)); cause != u.Unstarted {
		if err := c.emit(in, JournalDegraded, 0, DegradedData{Unit: u.Name, Kind: DegradedUnstarted, Cause: cause, Action: "offered again under the same claim"}); err != nil {
			return Instruction{}, false, err
		}
	}
	return Instruction{
		Run: c.run, State: c.st.qualified(in), Kind: InstructionAgent, Attempt: in.Attempt,
		Name: u.Name, Output: output, Class: c.st.qualified(in), Brief: c.brief(claim), Child: in.ID,
		Dispatch: c.callerDispatch(in), Spawn: c.spawnCommand(claim, c.callerDispatch(in)), ErrLog: u.errLogOf(claim), GitAuthority: in.Issued.GitAuthority, Workspace: in.Issued.Workspace, Context: ContextFresh,
		PreviousRefusal: previousRefusal(in),
	}, true, nil
}

// relistEvidence is what a whole wait with no fetch showed about an offer,
// as its never-started note records it: that no child fetched, and the last
// lines its spawn printed to errLog, when the harness records them and the
// file holds any. Two waits with the same evidence journal one note.
func relistEvidence(errLog string) string {
	cause := "never fetched by the end of a whole wait with nothing else for its coordinator"
	if errLog != "" {
		if tail := errLogExcerpt(errLog); len(tail) > 0 {
			cause += "; its start printed: " + strings.Join(tail, " | ")
		}
	}
	return cause
}

// outputReserve is what a blocking next's text holds besides its offers: the
// offer header, the more line, advice and the coordinator's reminder.
const (
	outputReserveBytes = 2048
	outputReserveLines = 12
)

// unitsThatFit is how many of order's units a blocking next to co can offer
// with its output under co's harness limit: each offer is measured as it
// prints (renderUnit), with the run's other instructions and a reserve
// taken first, and at least one is offered. The text of an offer is never
// shortened to fit more. A pi offer is sized with the worst-case stderr path
// and, when it is offered again as never started (relist: every unit; else
// passedOver), the sentence naming the earlier file and a 3-line excerpt of
// 200 bytes a line.
func (c *cycle) unitsThatFit(order []*instance, others []Instruction, co Coordinator, relist bool) int {
	if co.OutBytes <= 0 && co.OutLines <= 0 {
		return len(order)
	}
	bytes, lines := outputReserveBytes, outputReserveLines
	for _, in := range others {
		// Each line of a prompt prints indented, under the instruction's own
		// rule lines (a recovery's alone is about 250 bytes).
		bytes += len(in.Prompt) + 2*(strings.Count(in.Prompt, "\n")+1) + 512
		lines += strings.Count(in.Prompt, "\n") + 6
	}
	a := harness.For(normalizeHarness(co.Harness))
	over := func(bytes, lines int) bool {
		return co.OutBytes > 0 && bytes > co.OutBytes || co.OutLines > 0 && lines > co.OutLines
	}
	size := func(in *instance) (int, int) {
		ins := Instruction{Kind: InstructionAgent, Name: c.offerName(in), Brief: c.brief(strings.Repeat("0", 16)), Dispatch: c.callerDispatch(in), NotStarted: relist || c.passedOver(in, co), SameClaim: relist}
		if startLogOf(a) != nil {
			worst := c.childErrLog(strings.Repeat("0", 16))
			ins.ErrLog = worst
			if ins.NotStarted && worst != "" {
				ins.PrevErrLog = worst
				ins.PrevErrTail = []string{worst, worst, worst}
				for i := range ins.PrevErrTail {
					ins.PrevErrTail[i] = strings.Repeat("x", errLogExcerptBytes-len("…")) + "…"
				}
			}
		}
		text := renderUnit(a, ins)
		return len(text), strings.Count(text, "\n")
	}
	for i, in := range order {
		b, l := size(in)
		if i == 0 && over(bytes+b, lines+l) && !c.briefBare {
			// One offer is always made; rather than cross the limit with
			// it, this call's briefs leave out their "This unit runs" line.
			c.briefBare = true
			b, l = size(in)
		}
		bytes += b
		lines += l
		if i > 0 && over(bytes, lines) {
			return i
		}
	}
	return len(order)
}

// passedOver reports whether in's unit is offered again as never started.
// The offer's sizing and its rendering both read it.
func (c *cycle) passedOver(in *instance, co Coordinator) bool {
	u := in.Unit
	return u != nil && u.State == UnitHanded && !in.Settled && !c.displacedBy(co, u.HandSession, u.HandCall, c.admissionOf(co)) && c.unclaimedReofferCause(u) != ""
}

// staleHandOutDue is the re-offer time for an unclaimed hand-out whose owner
// has not driven since another named coordinator took over. Claimed units
// use the existing session and lease rules.
func (c *cycle) staleHandOutDue(u *unit) (time.Time, bool) {
	session := c.caller.Session
	if u.HandSession == "" || session == "" || session == u.HandSession || c.st.lastCoordinator != session {
		return time.Time{}, false
	}
	due := u.HandExpires
	if due.IsZero() {
		due = u.HandedAt.Add(c.e.opts.HandOutWindow)
	}
	return due, true
}

func (c *cycle) unclaimedReofferCause(u *unit) string {
	if u.HandSession == c.caller.Session {
		return c.unstartedCause(u)
	}
	if due, ok := c.staleHandOutDue(u); ok && !c.passNow().Before(due) {
		if live, _ := c.e.coordinatorLive(c.run, u.HandSession); !live {
			return fmt.Sprintf("handed to coordinator session %s and never fetched before its window ended while %s drove the run", u.HandSession, c.caller.Session)
		}
	}
	return ""
}

// passNow is the time this hand-out pass judges by (handAt), else the clock.
func (c *cycle) passNow() time.Time {
	if !c.handAt.IsZero() {
		return c.handAt
	}
	return c.now()
}

// unstartedCause is why u, handed to a coordinator session and never
// fetched, is offered to that coordinator again, or "" while it is not: a
// later offer's child fetched (runState.unstarted), or UnstartedGrace passed
// since its latest hand-out with no fetch. The grace is the fallback for a
// coordinator whose stale units are all it has left; the evidence is sooner.
// Nothing caps the offers: a unit nobody fetched is offered again for as
// long as the run goes on, each time with what its last spawn printed, and
// no count of them ends the session. Each offer waits for its evidence (a
// later offer's fetch, the grace, or J4's whole wait), so next never spins.
func (c *cycle) unstartedCause(u *unit) string {
	switch {
	case u.State != UnitHanded || u.HandSession == "":
		return ""
	case c.headlessLive(u):
		// Its headless child is still starting: not passed over. A child
		// that never fetches is stopped at headlessFetchBound, then
		// re-offered.
		return ""
	case u.HandSession == c.caller.Session && c.childrenFull(c.caller):
		// Waiting for a child to finish, not passed over: no strike until one does.
		return ""
	case c.st.unstarted(u):
		return "never fetched, while a child of a unit handed to the same coordinator by a later next fetched its prompt"
	case !c.passNow().Before(u.HandedAt.Add(c.e.opts.UnstartedGrace)):
		return fmt.Sprintf("never fetched in the %s since its hand-out", c.e.opts.UnstartedGrace)
	}
	return ""
}

// childrenFull reports whether co's claimed children reach its child cap
// (childCap): an offer of co's that waits unclaimed meanwhile waits for one
// of them to finish, which is no sign its harness is not starting it.
func (c *cycle) childrenFull(co Coordinator) bool {
	k, _ := co.childCap()
	return k > 0 && co.Session != "" && c.claimedBy(co) >= k
}

// childOwner is the coordinator session whose child holds u: the session of
// the offer whose claim token the child fetched with, else the latest
// hand-out's (owner). A child spawned from an earlier session's offer is
// that session's, though a newer session was offered the unit since.
func (u *unit) childOwner() string {
	if s := u.Origins[u.Bound].Session; s != "" {
		return s
	}
	return u.owner()
}

func (u *unit) childOfferSeq() int64 {
	if origin, ok := u.Origins[u.Bound]; ok {
		return origin.Call
	}
	return u.HandCall
}

// claimedBy counts co's claimed children not yet settled.
func (c *cycle) claimedBy(co Coordinator) int {
	n := 0
	for _, id := range c.st.order {
		in := c.st.insts[id]
		if u := in.Unit; u != nil && !in.Settled && u.State.claimed() && u.childOwner() == co.Session {
			n++
		}
	}
	return n
}

// offerName is the name in's unit is handed out under (handOut).
func (c *cycle) offerName(in *instance) string {
	if u := in.Unit; u != nil && u.State == UnitHanded {
		return u.Name
	}
	return c.st.unitName(in)
}
