package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

const childClaimFix = "use the command in your task text (with its --claim token)"

// Caller identity is cooperative and admits nothing: the claim token binds a
// call to its unit. A caller that is not the one the unit was handed out to
// or first fetched by is journaled (noteChildCaller) and proceeds.
func callerClaim(who harness.Caller) *Claim {
	if !who.FromEnv {
		return nil
	}
	return &Claim{Harness: who.Harness, Session: who.Session, Agent: who.Agent, Root: who.Root, Env: true}
}

func sameChild(owner *Claim, who harness.Caller) bool {
	if owner == nil {
		return true
	}
	// Never across harnesses or roots. Claude currently exposes a shared
	// parent session, and no child ID in its shell: the default compares an
	// agent ID only when the harness supplied one. Codex and Hermes match by
	// their own rules.
	return harness.SameChild(harness.Caller{Harness: owner.Harness, Session: owner.Session, Agent: owner.Agent, Root: owner.Root}, who)
}

var errForeignLiveClaim = fmt.Errorf("%w: the claim belongs to another live unit", ErrRejected)

// childClaimCheck is the form check of a child verb's claim. A call that
// carries a token, whoever makes it, is held to it: the token was offered
// for this unit, and the unit is not bound to a different one. A call from a
// harness environment must carry one. It runs again under the mutation lock
// of every verb that writes, so a binding made between the CLI's dry check
// and the journal append still refuses the call. The routes that go by name
// with no token (a library call, a person's operator form) are not checked
// here; the CLI asks a plain shell for the token of a harness hand-out
// (prepareChild).
func (c *cycle) childClaimCheck(name, token string, who harness.Caller) error {
	if token == "" {
		if c.e.opts.operatorChild || !who.FromEnv || (TrustSessionlessCallers && who.Session == "") {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrRejected, childClaimFix)
	}
	if name == "" {
		name = c.unitByClaim(token)
	}
	in, u, ok := c.st.unitByName(name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrRejected, childClaimFix)
	}
	if !slices.Contains(u.Offered, token) {
		other, otherUnit, found := c.st.unitByName(c.unitByClaim(token))
		if !in.Settled && u.State.live() && found && !other.Settled && otherUnit.State.live() && !c.expiredOffer(otherUnit, token) {
			return errForeignLiveClaim
		}
		return fmt.Errorf("%w: %s", ErrRejected, childClaimFix)
	}
	if u.Bound != "" && u.Bound != token {
		return fmt.Errorf("%w: another child already owns this unit; exit without working", ErrRejected)
	}
	if c.e.opts.operatorChild || !who.FromEnv {
		return nil // no caller identity to note
	}
	return c.noteChildCaller(in, u, token, who)
}

// handedToHarness reports whether u's latest hand-out went to a coordinator
// of a harness with an adapter, with a records root: a CLI hand-out, whose
// brief carries the unit's claim token. A library hand-out records no root.
func handedToHarness(u *unit) bool {
	return u.HandRoot != "" && !(u.HandSession == "" && TrustSessionlessCallers) && normalizeHarness(u.HandHarness) != "generic"
}

// noteChildCaller journals, once per unit and cause, that the caller of a
// child verb holding the unit's token is not the agent the engine expected:
// another harness than the unit's hand-out, a child of another coordinator
// session (a resumed or replaced coordinator's), or not the first fetcher.
// The call proceeds: the token is what binds it. On a dry cycle (a preflight,
// validate) nothing is written.
func (c *cycle) noteChildCaller(in *instance, u *unit, token string, who harness.Caller) error {
	cause := ""
	// A unit dispatched to another harness (a headless child) is expected
	// to be called from that harness, under the coordinator's session.
	want := u.HandHarness
	if u.ChildHarness != "" {
		want = u.ChildHarness
	}
	switch owner := boundChildCaller(u); {
	case u.HandSession != "" && identifyingHarness(want) && normalizeHarness(who.Harness) != normalizeHarness(want):
		if u.ChildHarness != "" {
			cause = fmt.Sprintf("the caller's harness %s is not the one its unit was dispatched to (%s)", who.Harness, want)
		} else {
			cause = fmt.Sprintf("the caller's harness %s is not the one its unit was handed out in (%s)", who.Harness, want)
		}
	case u.ChildHarness != "" && u.HandSession != "" && who.Is == harness.IsChild && who.Session != handParent(u, token):
		cause = fmt.Sprintf("the caller is a headless child of coordinator session %s, the unit was handed out by %s", who.Session, handParent(u, token))
	case u.ChildHarness == "" && u.HandSession != "" && u.HandRoot != "" && who.Is == harness.IsChild && (u.HandHarness == "codex" || u.HandHarness == "pi" || u.HandHarness == "opencode") && who.Session != handParent(u, token):
		cause = fmt.Sprintf("the caller is a child of coordinator session %s, the unit was handed out by %s", who.Session, handParent(u, token))
	case !sameChild(owner, who):
		cause = fmt.Sprintf("owner %s/%s, caller %s/%s", owner.Session, owner.Agent, who.Session, who.Agent)
	default:
		return nil
	}
	d := DegradedData{Unit: u.Name, Kind: DegradedChildCaller, Cause: cause, Action: "accepted: the claim token binds the unit"}
	if slices.Contains(c.st.degraded, d) {
		return nil
	}
	return c.emit(in, JournalDegraded, 0, d)
}

// handParent is the coordinator session token was handed out by: its own
// offer's when recorded (a still-valid earlier offer can win the race), else
// the unit's latest hand-out's.
func handParent(u *unit, token string) string {
	if origin := u.Origins[token]; origin.Session != "" {
		return origin.Session
	}
	return u.HandSession
}

func boundChildCaller(u *unit) *Claim {
	owner := u.BoundCaller
	// Old journals already record the first fetching session in Claim.
	if owner == nil && u.Bound != "" && u.Claim != nil && u.Claim.Env {
		owner = &Claim{Harness: u.Claim.Harness, Session: u.Claim.Session, Agent: u.Claim.Agent, Root: u.Claim.Root, Env: true}
		if owner.Harness == "claude" {
			owner.Agent = ""
		} // old Agent was resolved metadata, not a shell identity
	}
	return owner
}

// childClaimError journals an identity refusal (I2) of a mutating verb;
// validate stays read-only. The verb stays refused whatever the write does;
// a failed write is named, since the record is what counts the refusal.
func (e *Engine) childClaimError(run RunID, verb string, err error) error {
	var refused *identityRefusal
	if errors.As(err, &refused) && verb != "validate" && refused.unit != "" {
		if writeErr := e.journalRefusal(run, refused); writeErr != nil {
			return fmt.Errorf("%w (recording this refusal failed: %v)", err, writeErr)
		}
	}
	return err
}

// prepareChild resolves a CLI token once, before result files or effects are
// touched. Library calls keep their existing name-based API.
func (e *Engine) prepareChild(run RunID, verb string, f *flags) error {
	e.opts.childClaim = f.claim
	name := ""
	if len(f.args) > 0 {
		name = f.args[0]
	}
	err := e.withDryRun(run, func(c *cycle) error {
		if (verb == "done" || verb == "validate") && c.unitByClaim(f.claim) == "" {
			// Only the claim that bound the accepted unit is told it
			// finished, whoever presents it: the retry may come from another
			// caller than the first fetcher, by the unit's name as the first
			// call gave it or with none. Anything else, a name that is not
			// the recorded unit's among it, is refused as before.
			if a, ok := c.st.acceptedClaims[f.claim]; ok && (name == "" || name == a.Name) {
				return &alreadyAccepted{verb: verb, name: a.Name}
			}
		}
		if verb == "failed" && (FailureClass(f.class) == FailApproval || c.unitByClaim(f.claim) == "") {
			class, _ := ParseFailureClass(f.class) // the failed verb validated the class
			if r := c.repeatedFailed(name, f.claim, f.reason, class); r != nil {
				return r
			}
		}
		if err := c.childClaimCheck(name, f.claim, e.opts.caller); err != nil {
			return err
		}
		if name == "" {
			name = c.unitByClaim(f.claim)
		}
		if name == "" {
			return fmt.Errorf("%w: %s", ErrRejected, childClaimFix)
		}
		// A shell with no harness environment naming a unit that was handed
		// out to a harness coordinator needs the unit's token too: only a
		// person's operator form (dev) works by name.
		if _, u, ok := c.st.unitByName(name); ok && f.claim == "" && !e.opts.operatorChild && !e.opts.caller.FromEnv && handedToHarness(u) {
			return fmt.Errorf("%w: %s", ErrRejected, childClaimFix)
		}
		return nil
	})
	if err != nil {
		return e.childClaimError(run, verb, err)
	}
	f.args = []string{name}
	return nil
}

// alreadyAccepted is a done or validate run again with a claim whose done the
// engine already accepted: the first call's tool timed out after the commit
// (FANOUT-2 run B). The unit is finished; the call succeeds and says so.
type alreadyAccepted struct{ verb, name string }

func (e *alreadyAccepted) Error() string {
	return fmt.Sprintf("%s: already accepted: an earlier done with this claim finished %s; nothing is left to do for it: end your task and report that it is done", e.verb, e.name)
}

// repeatedFailed is the answer to a failed that repeats, identically, the
// failed that already ended the unit its claim bound: same claim, same class
// and reason, from whichever caller holds the claim. Anything else is nil and checked as before.
func (c *cycle) repeatedFailed(name, claim, reason string, class FailureClass) *alreadyFailed {
	fc, ok := c.st.failedClaims[claim]
	if !ok || (name != "" && name != fc.Name) {
		return nil
	}
	if class == FailApproval || fc.ApprovalAction != "" {
		if class == FailApproval && fc.ApprovalAction != "" && fc.ApprovalAction == reason {
			return &alreadyFailed{name: fc.Name, approval: true}
		}
		return nil
	}
	oc, why := failedOutcome(reason, class)
	if oc != fc.Class || !slices.Equal(fc.Reasons, []string{why}) {
		return nil
	}
	return &alreadyFailed{name: fc.Name, why: why}
}

// alreadyFailed is a failed run again, identically, after the engine recorded
// the first. The unit is ended; the call succeeds and says so.
type alreadyFailed struct {
	name, why string
	approval  bool
}

func (e *alreadyFailed) Error() string {
	if e.approval {
		return fmt.Sprintf("failed: already recorded: %s needs approval. Report the exact action to your coordinator and end your child task.", e.name)
	}
	return fmt.Sprintf("failed: already recorded: an earlier failed with this claim ended %s (%s); nothing is left to do for it: end your task and report that it failed", e.name, e.why)
}

const childSelfCallerReason = "the caller is the coordinator's own session"

// identityRefusal is a first prompt fetch refused because its caller is the
// coordinator's own session (I2): the error the caller sees, with the unit
// and the reason the journal records.
type identityRefusal struct {
	verb, unit, reason, fix string
	caller                  harness.Caller
	segment                 int64
}

func (r *identityRefusal) Error() string { return ErrRejected.Error() + ": " + r.verb + ": " + r.fix }
func (r *identityRefusal) Unwrap() error { return ErrRejected }

// refusalCap is how many identity refusals of one unit from one caller
// session are journaled one by one (a Codex coordinator and its children
// share a session); then one DegradedChildRefusalsCapped
// record says the count is at least refusalCap+1, and nothing more is
// written for that pair, so a looping caller cannot grow the journal.
const refusalCap = 20

// journalRefusal appends the degraded record of one identity refusal under
// the run's lock and nothing else (withTelemetry). Unlike Degrade it never
// skips a record the unit already has: every refusal counts, up to
// refusalCap per unit and caller session in that segment.
func (e *Engine) journalRefusal(run RunID, r *identityRefusal) error {
	return e.withTelemetry(run, func(c *cycle) error {
		segment := c.st.segment()
		if segment == nil || segment.Start != r.segment {
			return nil // the run crossed a boundary before it could record the refusal
		}
		instance, state, attempt, ok := c.handOutEnvelope(r.unit)
		if !ok {
			return nil
		}
		if c.st.insts[instance] == nil {
			instance = "" // the unit's instance is gone: the run holds the record (Degrade)
		}
		n := 0
		for _, d := range c.st.degraded[segment.degradedStart:] {
			if d.Unit != r.unit || d.Session != r.caller.Session {
				continue
			}
			switch d.Kind {
			case DegradedChildRefused:
				n++
			case DegradedChildRefusalsCapped:
				return nil
			}
		}
		caller := fmt.Sprintf(" (caller %s session %s agent %s)", r.caller.Harness, r.caller.Session, r.caller.Agent)
		d := DegradedData{Unit: r.unit, Kind: DegradedChildRefused, Session: r.caller.Session, SelfCaller: true, Cause: r.verb + ": " + r.reason + caller, Action: "refused; the unit stays offered"}
		if n >= refusalCap {
			d = DegradedData{Unit: r.unit, Kind: DegradedChildRefusalsCapped, Session: r.caller.Session, Cause: fmt.Sprintf("more than %d refusals%s", refusalCap, caller), Action: "further refusals from this session on this unit are not recorded"}
		}
		return c.emitAs(instance, state, attempt, JournalDegraded, 0, d)
	})
}

// handOutEnvelope is where unit's latest handed_out record put it: that
// record's instance, state and attempt, its local state renamed by every
// migration committed since, narrowed to that instance's definition as the
// fold narrows it (renamesFor). The journal's own records say it, not a fold against a
// definition the run has not migrated to yet, so a refusal's record is
// attributed as the run's other records are.
func (c *cycle) handOutEnvelope(unit string) (instance, state string, attempt int, ok bool) {
	at := -1
	for i, r := range c.recs {
		if r.Kind != JournalHandedOut {
			continue
		}
		var d HandedOutData
		if json.Unmarshal(r.Data, &d) == nil && d.Name == unit {
			at = i
		}
	}
	if at < 0 {
		return "", "", 0, false
	}
	r := c.recs[at]
	state = r.State
	in := c.st.insts[r.Instance]
	for _, m := range c.recs[at+1:] {
		if m.Kind != JournalMigrated || in == nil {
			continue
		}
		var d MigratedData
		if json.Unmarshal(m.Data, &d) == nil {
			// The fold's own narrowing (renamesFor): a migration's qualified
			// keys apply to this instance's definition only, as local names.
			if to, renamed := c.st.renamesFor(in, d.States)[state]; renamed {
				state = to
			}
		}
	}
	return r.Instance, state, r.Attempt, true
}

// identifyingHarness reports whether h is a harness whose environment
// identifies its agents (every registered adapter's, and the scripted
// driver's): a child of one cannot be a child of another. A generic or
// unknown harness identifies no one.
func identifyingHarness(h string) bool {
	h = normalizeHarness(h)
	return h == ScriptedHarness || harness.Known(h)
}
