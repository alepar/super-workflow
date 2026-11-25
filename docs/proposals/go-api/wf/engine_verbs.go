package wf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// liveUnit resolves a unit name for a verb. A retired or unknown name is
// ErrRejected.
func (c *cycle) liveUnit(verb, name string) (*instance, *unit, error) {
	in, u, ok := c.st.unitByName(name)
	switch {
	case !ok && c.st.retired[name]:
		return nil, nil, fmt.Errorf("%w: %s: unit %s is retired", ErrRejected, verb, name)
	case !ok:
		return nil, nil, fmt.Errorf("%w: %s: no unit %s", ErrRejected, verb, name)
	}
	if !c.untilUnitAllows(in) {
		return nil, nil, fmt.Errorf("%w: outside immutable until-unit boundary", ErrRejected)
	}
	return in, u, nil
}

// claimedUnit is liveUnit for a verb that needs the unit claimed: started,
// running or stalled. Any other state is ErrRejected.
func (c *cycle) claimedUnit(verb, name string) (*instance, *unit, error) {
	in, u, err := c.liveUnit(verb, name)
	if err == nil && !u.State.claimed() {
		err = fmt.Errorf("%w: %s: unit %s is %s, not claimed", ErrRejected, verb, name, u.State)
	}
	return in, u, err
}

// settling is claimedUnit for a verb that settles the unit (done, failed):
// the engine first searches for the child's transcript, unthrottled (A2).
func (c *cycle) settling(verb, name string) (*instance, *unit, error) {
	in, u, err := c.claimedUnit(verb, name)
	if err != nil {
		return in, u, err
	}
	c.searchNow = true
	if err := c.resolveChildren(); err != nil {
		return nil, nil, err
	}
	return in, in.Unit, nil
}

// unitByClaim is the live unit a hand-out minted claim for; "" when none.
func (c *cycle) unitByClaim(claim string) string {
	if claim == "" {
		return ""
	}
	for _, id := range c.st.order {
		if u := c.st.insts[id].Unit; u != nil && slices.Contains(u.Offered, claim) {
			if _, _, ok := c.st.unitByName(u.Name); ok {
				return u.Name
			}
		}
	}
	return ""
}

// waveSession is the session, harness and agent id of the implicit claims the
// wave aliases make: a paged coordinator sees them as orphans of session wave.
const waveSession = "wave"

// claimForWave is the implicit claim of a unit the wave Next just handed out:
// attempt_started under session wave, then prompt_fetched, since the prompt is
// delivered inline and the spawn window never applies. The claim has no
// transcript, so the unit never stalls.
func (c *cycle) claimForWave(in *instance) error {
	d := AttemptStartedData{Name: in.Unit.Name, Harness: waveSession, Session: waveSession, Agent: waveSession}
	if lease := c.mapLeaseOf(in); lease > 0 {
		d.Expires = c.now().Add(lease)
	}
	if err := c.emit(in, JournalAttemptStarted, 0, d); err != nil {
		return err
	}
	return c.emit(in, JournalPromptFetched, 0, PromptFetchedData{Name: in.Unit.Name, PromptHash: "sha256:" + hashBytes([]byte(in.Issued.Prompt)), PromptShapeHash: c.promptShapeHash(in, in.Issued.Prompt)})
}

// Started implements Executor.
func (e *Engine) Started(run RunID, name string, cl Claim) error {
	_, err := e.StartedNote(run, name, cl)
	return err
}

// StartedNote implements Executor.
func (e *Engine) StartedNote(run RunID, name string, cl Claim) (string, error) {
	state := ""
	err := e.withRun(run, func(c *cycle) error {
		if err := c.checkSegment("started", c.e.opts.caller); err != nil {
			return err
		}
		if err := c.checkUntilUnitTelemetry(name); err != nil {
			return err
		}
		if end, ok := c.st.endedUnits[name]; ok {
			state = end.State
			merged, ok := end.Claim.merge(unitClaim(cl))
			if !ok {
				return fmt.Errorf("%w: started: %s", ErrRejected, end.Claim.conflict(name, unitClaim(cl)))
			}
			in := c.st.insts[c.st.names[name].Instance]
			if merged == end.Claim || in == nil {
				return nil
			}
			return c.emit(in, JournalMetadataAttached, 0, lateAttach(name, state, merged))
		}
		in, u, err := c.liveUnit("started", name)
		if err != nil {
			return err
		}
		if c.st.paused && u.State == UnitHanded {
			return fmt.Errorf("%w: started: %s", ErrRejected, stoppedNoNewClaim(c.run))
		}
		if cur := c.st.segment(); u.State == UnitHanded && cur != nil && u.HandSegment != 0 && u.HandSegment != cur.Start {
			// A started would newly claim an earlier segment's offer, which
			// expired at the boundary.
			return fmt.Errorf("%w: started: unit %s: this offer expired when the run was stopped and resumed; exit without working", ErrRejected, name)
		}
		switch {
		case u.State.claimed():
			state = u.State.String()
			if u.Claim != nil {
				merged, ok := u.Claim.merge(unitClaim(cl))
				if ok {
					// A started on a unit living by its implicit lease is a sign
					// of life: it renews that lease (RestartedAt), even when it
					// arrives past it, so a late started never loses a fetched
					// unit. Only a drive's expireTimers does.
					implicit := c.implicitLease(u)
					if merged == *u.Claim && !implicit {
						return nil
					}
					// Metadata can remove an unmonitored deadline. Settle the
					// earliest one first, before changing the claim's liveness.
					now := c.now()
					d, pending := c.dueFor(in, deadlineSpawn, now)
					if lease, ok := c.dueFor(in, deadlineLease, now); ok && !implicit && !(u.State == UnitRunning && c.watched(u)) && (!pending || !lease.Due.After(d.Due)) {
						d, pending = lease, true
					}
					if pending && !now.Before(d.Due) {
						cause := LostCauseSpawn
						if d.Kind == deadlineLease {
							cause = LostCauseLease
						}
						if err := c.loseExpired(in, d, cause); err != nil {
							return err
						}
						return fmt.Errorf("%w: started: unit %s exceeded its %s window", ErrRejected, name, d.Kind)
					}
					if u.State == UnitStarted { // claimed by started, no prompt yet: not late
						return c.emit(in, JournalAttemptStarted, 0, AttemptStartedData{
							Name: name, Harness: merged.Harness, Session: merged.Session, Agent: merged.Agent,
							Transcript: merged.Transcript, Context: merged.Context, Expires: u.LeaseExpires,
						})
					}
					return c.emit(in, JournalMetadataAttached, 0, lateAttach(name, state, merged))
				}
				return fmt.Errorf("%w: started: %s", ErrRejected, u.Claim.conflict(name, unitClaim(cl)))
			}
			return fmt.Errorf("%w: unit %s is already claimed", ErrRejected, name)
		case u.State == UnitHeld:
			// A held unit is no live claim: a blocked failure parked it, and only
			// wf retry releases it. A late started cannot attach to it.
			return fmt.Errorf("%w: started: unit %s is held by a blocked failure; wf retry releases it", ErrRejected, name)
		case u.State != UnitHanded:
			return fmt.Errorf("%w: started: unit %s is %s, not handed out", ErrRejected, name, u.State)
		}
		// A manual claim (A4: started stays for one release, and for a
		// harness with no adapter). The loop never calls it: a child's prompt
		// fetch claims its unit, and a hand-out has no clock before that.
		d := AttemptStartedData{Name: name, Harness: cl.Harness, Session: cl.Session, Agent: cl.Agent, Transcript: cl.Transcript, Context: cl.Context, Root: cl.Root}
		if lease := c.mapLeaseOf(in); lease > 0 {
			d.Expires = c.now().Add(lease)
		}
		if err := c.emit(in, JournalAttemptStarted, 0, d); err != nil {
			return err
		}
		return c.drive()
	})
	return state, err
}

// lateAttach is the metadata_attached payload carrying a claim's merged metadata.
// AttachUsage journals tokens the coordinator saw a child spend, after the
// child ended (wf started <name> --usage tokens=N): a child cannot know its own
// count, and the spawn result reports it. The same count again is a no-op;
// a different count for a unit that already has usage is refused, so one
// attempt never counts twice. A live unit is refused: its child passes done
// --usage itself, or the coordinator attaches once it ends.
func (e *Engine) AttachUsage(run RunID, name string, tokens int) error {
	return e.withRun(run, func(c *cycle) error {
		if err := c.checkSegment("started", c.e.opts.caller); err != nil {
			return err
		}
		if err := c.checkUntilUnitTelemetry(name); err != nil {
			return err
		}
		end, ok := c.st.endedUnits[name]
		if !ok {
			if _, _, live := c.st.unitByName(name); live {
				return fmt.Errorf("%w: started: unit %s has not ended; usage attaches after its child returns (or pass done --usage)", ErrRejected, name)
			}
			return fmt.Errorf("%w: started: no unit %s", ErrRejected, name)
		}
		switch spent := c.st.unitTokens[name]; {
		case spent == tokens:
			return nil
		case spent != 0:
			return fmt.Errorf("%w: started: unit %s already has usage tokens=%d", ErrRejected, name, spent)
		}
		in := c.st.insts[c.st.names[name].Instance]
		if in == nil {
			return fmt.Errorf("%w: started: unit %s has no instance to attach usage to", ErrRejected, name)
		}
		d := lateAttach(name, end.State, unitClaim{})
		d.Tokens = tokens
		return c.emit(in, JournalMetadataAttached, 0, d)
	})
}

// Degrade journals one degraded record per entry of ds for the unit name, a
// name the run handed out, live or ended. An entry the unit already has is
// skipped, so a done run again after a refusal records nothing twice. An
// ended run still takes them: a record changes no state, and the
// coordinator's escalation for the run's last unit arrives after its done.
func (e *Engine) Degrade(run RunID, name string, ds []DegradedData) error {
	return e.degrade(run, name, ds, false)
}

// DegradeChecked is Degrade for a coordinator's telemetry (started
// --escalated): inside its lock the caller is checked against the run's
// harness segment first, as started itself is.
func (e *Engine) DegradeChecked(run RunID, name string, ds []DegradedData) error {
	return e.degrade(run, name, ds, true)
}

func (e *Engine) degrade(run RunID, name string, ds []DegradedData, checked bool) error {
	if len(ds) == 0 {
		return nil
	}
	return e.withRun(run, func(c *cycle) error {
		if checked {
			if err := c.checkSegment("started", c.e.opts.caller); err != nil {
				return err
			}
		}
		return c.recordDegraded(name, ds)
	})
}

// recordDegraded journals ds for unit name, each once: a record already in
// the journal is not written again, so a verb run again after a busy run
// does not repeat its --escalated reports.
func (c *cycle) recordDegraded(name string, ds []DegradedData) error {
	if len(ds) == 0 {
		return nil
	}
	if err := c.checkUntilUnitTelemetry(name); err != nil {
		return err
	}
	ref, ok := c.st.names[name]
	if !ok {
		return fmt.Errorf("%w: no unit %s", ErrRejected, name)
	}
	instance := ref.Instance
	if c.st.insts[instance] == nil {
		instance = "" // the unit's instance is gone: the run holds the record
	}
	for _, d := range ds {
		d.Unit = name
		if slices.Contains(c.st.degraded, d) {
			continue
		}
		if err := c.emitAs(instance, ref.State, ref.Attempt, JournalDegraded, 0, d); err != nil {
			return err
		}
	}
	return nil
}

func lateAttach(name, state string, m unitClaim) MetadataAttachedData {
	return MetadataAttachedData{Name: name, Harness: m.Harness, Session: m.Session, Agent: m.Agent,
		Transcript: m.Transcript, Context: m.Context, Root: m.Root, State: state, Late: true}
}

// promptBodyFile is the name of the file a fetched prompt's full text is
// written to (E1).
const promptBodyFile = "prompt.md"

// deliverPrompt is what wf prompt prints for the rendered text of unit name
// (E1): page(bodyPath), one short page, with the full text written to
// runDir/prompt/<hash>/prompt.md, the hash covering the name and the text. A
// re-fetch of the same text finds its file already written whole, and a
// changed text gets a new directory, so no file a child is reading is ever
// rewritten. dir is that directory. With no runDir there is nowhere to write
// it, and text is returned whole.
func deliverPrompt(text, runDir, name string, page func(body string) string) (msg, dir string, err error) {
	if runDir == "" {
		return text, "", nil
	}
	sum := sha256.Sum256([]byte(name + "\x00" + text))
	dir = filepath.Join(runDir, "prompt", hex.EncodeToString(sum[:8]))
	body := filepath.Join(dir, promptBodyFile)
	if fi, err := os.Stat(body); err != nil || fi.Size() != int64(len(text)) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
		tmp := body + ".tmp"
		if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
			return "", "", err
		}
		if err := os.Rename(tmp, body); err != nil {
			return "", "", err
		}
	}
	return page(body), dir, nil
}

// Prompt implements Executor.
func (e *Engine) Prompt(run RunID, name string) (string, error) {
	return e.PromptClaim(run, name, "")
}

// PromptClaim implements Executor.
func (e *Engine) PromptClaim(run RunID, name, claim string) (string, error) {
	f, err := e.PromptFetch(run, name, claim)
	return f.Delivered, err
}

// PromptFetch is a unit's prompt fetch (wf prompt): what the child is shown
// (Delivered: the prompt itself, or the instruction to read its parts), the
// full rendered prompt, and its input values, which the prompt text holds
// once each and no longer lists under its own heading when a token
// consumed it. A machine reader (an eval harness) takes Prompt and Inputs.
type PromptFetch struct {
	PromptHash      string            `json:"promptHash"`
	PromptShapeHash string            `json:"promptShapeHash"`
	Delivered       string            `json:"delivered"`
	Prompt          string            `json:"prompt"`
	Inputs          map[string]string `json:"inputs,omitempty"`
}

// PromptFetch fetches a unit's prompt as PromptClaim does and returns all of
// it (see the PromptFetch type).
func (e *Engine) PromptFetch(run RunID, name, claim string) (PromptFetch, error) {
	return e.PromptFetchAs(run, name, claim, harness.Caller{})
}

// PromptFetchAs is PromptFetch for the child whose own environment says who
// it is (A1): the claim it makes takes its harness, its coordinator's session,
// its own agent id where the environment gives one, and the transcript root
// the engine searches for the rest. A name of "" finds the unit by the claim
// token alone. A caller the environment shows to be the coordinator itself
// is refused (A3b): a unit needs its own agent.
func (e *Engine) PromptFetchAs(run RunID, name, claim string, who harness.Caller) (PromptFetch, error) {
	return e.promptFetch(run, name, claim, who, false)
}

// PromptFetchWhole is PromptFetchAs for a reader handed the whole prompt
// (wf prompt --json): the fetch is journaled Whole.
func (e *Engine) PromptFetchWhole(run RunID, name, claim string, who harness.Caller) (PromptFetch, error) {
	return e.promptFetch(run, name, claim, who, true)
}

func (e *Engine) promptFetch(run RunID, name, claim string, who harness.Caller, whole bool) (PromptFetch, error) {
	if on, err := e.pagedPrompts(run); err != nil {
		return PromptFetch{}, err
	} else if on {
		page, err := e.PromptPage(run, name, claim, who, PromptPageRequest{Page: 1})
		return PromptFetch{Delivered: page.Delivered}, err
	}

	// A child's verb needs the unit's claim token; nothing else admits it.
	if err := e.ChildVerbCheck(run, "prompt", name, claim, who); err != nil {
		return PromptFetch{}, e.childClaimError(run, "prompt", err)
	}
	var text string
	var f PromptFetch
	err := e.withRun(run, func(c *cycle) error {
		in, u, fetched, err := c.fetchable(name, claim, who)
		if err != nil {
			return err
		}
		name = fetched
		// A fetch is proof of life, so it is accepted even at or past the
		// spawn window of a unit started claimed: a fetched unit has no spawn
		// window (deadline.go). Only a drive's expireTimers loses a unit to
		// it, and a fetch after that finds the name retired.
		n := in.node()
		p, ok := n.kind.(prompter)
		if !ok {
			return fmt.Errorf("%w: prompt: state %s has no prompt", ErrRejected, n.name)
		}
		args := []string{name, "--output", "<file>"}
		output, err := c.outputPath(name)
		if err != nil {
			return err
		}
		if output != "" {
			args = []string{name}
		}
		if claim != "" {
			args = append([]string{"--claim", claim}, args[1:]...)
		}
		if text, f.Inputs, err = p.renderPrompt(c, in, n, c.verbLine("done", args...)); err != nil {
			return err
		}
		text += "\n\n" + c.childRules(name, output, claim) + "\n\n" + c.finishLine(name, output, claim) + "\n"
		f.Prompt = text
		f.PromptHash = "sha256:" + hashBytes([]byte(text))
		f.PromptShapeHash = c.promptShapeHash(in, text)
		runDir := ""
		if output != "" { // a run dir exists: the unit has an allocated output
			runDir = c.e.opts.RunDir(c.run)
		}
		promptDir := ""
		workspace := c.workdir(in)
		if text, promptDir, err = deliverPrompt(text, runDir, name, func(body string) string {
			return c.childPage(name, output, workspace, body, claim)
		}); err != nil {
			return err
		}
		startedFirst := u.State.claimed() // a started claimed it before this fetch
		if u.State == UnitHanded {
			d := AttemptStartedData{Name: name, Harness: who.Harness, Session: who.Session, Agent: who.Agent, Root: who.Root, Env: who.FromEnv}
			if lease := c.mapLeaseOf(in); lease > 0 {
				d.Expires = c.now().Add(lease)
			}
			if err := c.emit(in, JournalAttemptStarted, 0, d); err != nil {
				return err
			}
		}
		if startedFirst && u.Bound == "" && claim != "" && u.Claim != nil && who.FromEnv {
			// Claimed by a started before its child's first fetch: the child's
			// own environment fills what the started left empty.
			add := MetadataAttachedData{Name: name, State: u.State.String(), Resolved: true, Env: true}
			if u.Claim.Agent == "" {
				add.Agent = who.Agent
			}
			if u.Claim.Root == "" {
				add.Root = who.Root
			}
			if u.Claim.Harness == "" {
				add.Harness = who.Harness
			}
			if u.Claim.Session == "" {
				add.Session = who.Session
			}
			if err := c.emit(in, JournalMetadataAttached, 0, add); err != nil {
				return err
			}
		}
		bind := ""
		if u.Bound == "" {
			bind = claim
		}
		return c.emit(in, JournalPromptFetched, 0, PromptFetchedData{Name: name, Claim: bind, Caller: callerClaim(who), PromptDir: promptDir, Whole: whole, PromptHash: f.PromptHash, PromptShapeHash: f.PromptShapeHash})
	})
	if err != nil {
		return PromptFetch{}, e.childClaimError(run, "prompt", err)
	}
	f.Delivered = text
	return f, nil
}

// allocatedOutput resolves a claimed unit before the CLI reads any result bytes.
func (e *Engine) allocatedOutput(run RunID, name string) (string, error) {
	return e.allocatedOutputFor("done", run, name)
}

// allocatedOutputFor is a dry lookup for either submission verb: no result
// has passed preflight yet, so not even a migration is written.
func (e *Engine) allocatedOutputFor(verb string, run RunID, name string) (string, error) {
	var output string
	err := e.withDryRun(run, func(c *cycle) error {
		if _, _, err := c.claimedUnit(verb, name); err != nil {
			return err
		}
		var err error
		output, err = c.outputPath(name)
		return err
	})
	return output, err
}

// allocatedArtifacts is the allocated path of each artifact the claimed
// unit's state produces, by name: what its prompt's Artifacts section prints,
// computed from the run's state rather than read back from the prompt.
func (e *Engine) allocatedArtifacts(run RunID, name string) (map[string]string, error) {
	out := map[string]string{}
	err := e.withDryRun(run, func(c *cycle) error {
		in, _, err := c.claimedUnit("prompt", name)
		if err != nil {
			return err
		}
		for _, a := range allocatedOf(in.node()) {
			p, err := c.allocatedPath(in, a)
			if err != nil {
				return err
			}
			out[a.name] = p
		}
		return nil
	})
	return out, err
}

// Done implements Executor.
func (e *Engine) Done(run RunID, name string, s Submission) (SubmitResult, error) {
	return e.DoneEscalated(run, name, s, nil)
}

// DoneEscalated is Done that also journals the attempt's --escalated reports
// (escalated), under the same lock as the result: a done either records both
// or, on a busy run, nothing.
func (e *Engine) DoneEscalated(run RunID, name string, s Submission, escalated []DegradedData) (SubmitResult, error) {
	if err := e.childOffer(run, "done", name, ""); err != nil {
		return SubmitResult{}, e.childClaimError(run, "done", err)
	}
	// Refusals are checked on a dry cycle first, so the drive's transcript
	// resolution is not journaled for a call that is refused (wf-checker's
	// protocol probe on J).
	if err := e.withDryRun(run, func(c *cycle) error {
		if err := c.childClaimCheck(name, e.opts.childClaim, e.opts.caller); err != nil {
			return err
		}
		in, _, err := c.settling("done", name)
		if err != nil {
			return err
		}
		s = unfencedFor(in.node(), s)
		return c.unitResultPaths(name, s.Output)
	}); err != nil {
		return SubmitResult{}, e.childClaimError(run, "done", err)
	}
	var res SubmitResult
	err := e.withRun(run, func(c *cycle) error {
		if err := c.childClaimCheck(name, e.opts.childClaim, e.opts.caller); err != nil {
			return err
		}
		if err := c.recordDegraded(name, escalated); err != nil {
			return err
		}
		in, _, err := c.settling("done", name)
		if err != nil {
			return err
		}
		if err := c.unitResultPaths(name, s.Output); err != nil {
			return err
		}
		if in.Settled {
			// A seat settled for its join while it ran: its output is a late
			// vote, and the decision is never reopened.
			res = SubmitResult{Class: Accepted}
			if err := c.emit(in, JournalLateVote, 0, LateVoteData{Name: name, Output: s.Output, Tokens: s.Tokens, Normalized: s.Normalized}); err != nil {
				return err
			}
			return c.drainCleanups()
		}
		tok, _ := c.st.tokenForName(name)
		s.Token, s.Child = tok, in.ID
		res, err = c.submit(s)
		return err
	})
	return res, e.childClaimError(run, "done", err)
}

// Validate implements Executor: Done's checks on a dry cycle. A settled unit
// has nothing left to validate.
func (e *Engine) Validate(run RunID, name string, s Submission) (SubmitResult, error) {
	if err := e.childOffer(run, "validate", name, ""); err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	renew := false
	err := e.withDryRun(run, func(c *cycle) error {
		if err := c.childClaimCheck(name, e.opts.childClaim, e.opts.caller); err != nil {
			return err
		}
		in, _, err := c.settling("validate", name)
		if err != nil {
			return err
		}
		if in.Settled {
			return fmt.Errorf("%w: validate: unit %s already settled", ErrRejected, name)
		}
		s = unfencedFor(in.node(), s)
		tok, _ := c.st.tokenForName(name)
		s.Token, s.Child = tok, in.ID
		renew = c.renewDue(in)
		res, _, err = c.classify(s)
		return err
	})
	if err == nil && renew {
		err = e.renewByCall(run, name)
	}
	return res, e.childClaimError(run, "validate", err)
}

// missingOutputFP is the progress fingerprint of a done with no output file.
const missingOutputFP = "missing-output"

// errOutputMissing is the first done of an attempt on a claimed unit whose
// allocated output file does not exist. The attempt stays open, as it does
// for a result path error: the child writes the file and runs done again.
var errOutputMissing = errors.New("no output file")

// refuseMissingOutput is done on a claimed unit whose allocated output file
// does not exist. The first one of an attempt is errOutputMissing naming
// path: a submission_refused is journaled and the attempt stays open. A
// second one on the same attempt is a validation refusal through
// cycle.refuse, like a malformed output: it spends the attempt, so a child
// that keeps running done without writing its output takes the state's retry
// and no-progress routes whatever else it does. Nothing is read: path is
// only named in the reason. A token that no longer names the open attempt is
// rejected as any done's is.
func (e *Engine) refuseMissingOutput(run RunID, name, path string, escalated []DegradedData) (SubmitResult, error) {
	var res SubmitResult
	err := e.withRun(run, func(c *cycle) error {
		if err := c.childClaimCheck(name, e.opts.childClaim, e.opts.caller); err != nil {
			return err
		}
		if err := c.recordDegraded(name, escalated); err != nil {
			return err
		}
		in, _, err := c.claimedUnit("done", name)
		if err != nil {
			return err
		}
		tok, _ := c.st.tokenForName(name)
		if in.Settled {
			return fmt.Errorf("%w: done: unit %s has no output at %s", ErrRejected, name, path)
		}
		if reason := c.tokenProblem(in, tok, 0); reason != "" {
			return c.reject(in, tok, reason, &res)
		}
		reasons := []string{"no output file at " + path}
		if in.Refusals > 0 && in.RefusalFP == missingOutputFP {
			if res, err = c.refuse(in, in.node(), reasons, missingOutputFP); err != nil {
				return err
			}
			return c.drive()
		}
		if err := c.emit(in, JournalSubmissionRefused, Refused, SubmissionRefusedData{Token: tok, Reasons: reasons, Left: 1, Fingerprint: missingOutputFP}); err != nil {
			return err
		}
		return fmt.Errorf("%w at %s", errOutputMissing, path)
	})
	return res, e.childClaimError(run, "done", err)
}

// Failed implements Executor.
func (e *Engine) Failed(run RunID, name, reason string) (SubmitResult, error) {
	return e.FailedAs(run, name, reason, FailTransient)
}

// FailedAs implements Executor.
func (e *Engine) FailedAs(run RunID, name, reason string, class FailureClass) (SubmitResult, error) {
	return e.FailedEscalated(run, name, reason, class, nil)
}

// FailedEscalated is FailedAs that also journals the attempt's --escalated
// reports under the same lock as the failure, as DoneEscalated does.
func (e *Engine) FailedEscalated(run RunID, name, reason string, class FailureClass, escalated []DegradedData) (SubmitResult, error) {
	if class == FailApproval && strings.TrimSpace(reason) == "" {
		return SubmitResult{}, fmt.Errorf("%w: approval requires the exact command or tool call in the reason", ErrRejected)
	}
	repeat := func(c *cycle) *alreadyFailed {
		if class != FailApproval {
			return nil
		}
		return c.repeatedFailed(name, e.opts.childClaim, reason, class)
	}
	if err := e.childOffer(run, "failed", name, ""); err != nil {
		return SubmitResult{}, e.childClaimError(run, "failed", err)
	}
	if err := e.withDryRun(run, func(c *cycle) error { // a refused failed appends nothing
		if r := repeat(c); r != nil {
			return r
		}
		_, _, err := c.settling("failed", name)
		return err
	}); err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err := e.withRun(run, func(c *cycle) error {
		if r := repeat(c); r != nil {
			return r
		}
		if err := c.childClaimCheck(name, e.opts.childClaim, e.opts.caller); err != nil {
			return err
		}
		if err := c.recordDegraded(name, escalated); err != nil {
			return err
		}
		in, _, err := c.settling("failed", name)
		if err != nil {
			return err
		}
		if in.Settled {
			if err := c.emit(in, JournalWithdrawn, 0, WithdrawnData{Name: name, Reason: "late failure"}); err != nil {
				return err
			}
			return c.drainCleanups()
		}
		tok, _ := c.st.tokenForName(name)
		res, err = c.failToken(tok, reason, class)
		return err
	})
	return res, e.childClaimError(run, "failed", err)
}

// fetchable is the unit a prompt fetch names, by name or by claim token,
// with the checks every fetch passes: it is live, handed or claimed, offered
// with the claim, not bound to another, and not the coordinator's own first
// fetch (B1).
func (c *cycle) fetchable(name, claim string, who harness.Caller) (*instance, *unit, string, error) {
	if err := c.childClaimCheck(name, claim, who); err != nil {
		return nil, nil, "", err
	}
	if name == "" {
		if name = c.unitByClaim(claim); name == "" {
			return nil, nil, "", fmt.Errorf("%w: prompt: no unit of run %s was handed out with claim token %s", ErrRejected, c.run, claim)
		}
	}
	in, u, err := c.liveUnit("prompt", name)
	if err != nil {
		return nil, nil, "", err
	}
	// The first fetch is the child's (B1): one from the coordinator's own
	// session is refused, whether or not a started claimed the unit first.
	if who.Is == harness.IsSelf && u.Bound == "" {
		return nil, nil, "", &identityRefusal{verb: "prompt", unit: name, reason: childSelfCallerReason, fix: childCommandFix, caller: who, segment: c.st.segment().Start}
	}
	if u.State != UnitHanded && !u.State.claimed() {
		return nil, nil, "", fmt.Errorf("%w: prompt: unit %s is %s, not handed or claimed", ErrRejected, name, u.State)
	}
	// G9: after wf stop nothing new is claimed; a child already running may
	// fetch again and finish.
	if c.st.paused && u.State == UnitHanded {
		return nil, nil, "", fmt.Errorf("%w: prompt: %s", ErrRejected, stoppedNoNewClaim(c.run))
	}
	if claim != "" && !slices.Contains(u.Offered, claim) {
		return nil, nil, "", fmt.Errorf("%w: prompt: unit %s was never handed out with claim token %s", ErrRejected, name, claim)
	}
	if claim != "" && u.Bound != "" && u.Bound != claim {
		return nil, nil, "", fmt.Errorf("%w: prompt: unit %s: another child already owns this unit; exit without working", ErrRejected, name)
	}
	if c.expiredOffer(u, claim) {
		// Before a fetch binds it, a token must come from the segment its
		// unit is offered or claimed in: a still-handed unit's current one,
		// a started unit's hand-out (so a started before a boundary keeps its
		// own offer, and one started on a re-offer does not revive an expired
		// token). An earlier segment's offer expired at the boundary.
		return nil, nil, "", fmt.Errorf("%w: prompt: unit %s: this offer expired when the run was stopped and resumed; exit without working", ErrRejected, name)
	}
	return in, u, name, nil
}

func (c *cycle) expiredOffer(u *unit, claim string) bool {
	o, ok := u.Origins[claim]
	seg := c.st.segment()
	return ok && u.Bound == "" && o.Segment != 0 && seg != nil && o.Segment != tokenSegment(u, seg.Start)
}

// stoppedNoNewClaim is why a stopped run refuses a unit's first claim
// (G9): a child already running may fetch again and finish.
func stoppedNoNewClaim(run RunID) string {
	return fmt.Sprintf("run %s is stopped: no new unit starts until a person resumes it; exit without working", run)
}

// childOffer is the form check every child verb passes before anything else,
// on a dry cycle: the claim names this unit (childClaimCheck) and its offer
// did not expire at a stop and resume. The claim token is what binds a call
// to its unit. The harness's records are read for usage and liveness
// (resolveChildren), never to admit a call: a child whose launch they do not
// show proceeds.
func (e *Engine) childOffer(run RunID, verb, name, claim string, callers ...harness.Caller) error {
	who := e.opts.caller
	if claim == "" {
		claim = e.opts.childClaim
	}
	if len(callers) > 0 && callers[0].FromEnv {
		who = callers[0]
	}
	if e.opts.operatorChild {
		return nil
	}
	return e.withDryRun(run, func(c *cycle) error { // a read: validate never writes
		if err := c.childClaimCheck(name, claim, who); err != nil {
			return err
		}
		if name == "" {
			if name = c.unitByClaim(claim); name == "" {
				return nil // the verb itself refuses an unknown claim
			}
		}
		ref, ok := c.st.names[name]
		if !ok {
			return nil
		}
		u := c.st.insts[ref.Instance].Unit
		if u == nil || u.Name != name || (claim != "" && !slices.Contains(u.Offered, claim)) {
			return nil // the verb itself refuses what is not this unit's
		}
		if c.expiredOffer(u, claim) {
			return fmt.Errorf("%w: %s: unit %s: this offer expired when the run was stopped and resumed; exit without working", ErrRejected, verb, name)
		}
		return nil
	})
}

// mtimeSlack is how far a file's mtime may trail the journal stamp it is
// compared with (coarse kernel timestamps).
const mtimeSlack = 2 * time.Second

// ChildVerbCheck decides on dry cycles whether a child's verb on unit name
// (or the unit claim names) is refused, before anything is journaled: the
// claim's form (childOffer) and the unit's own checks (a prompt's fetchable, a
// submission's settling). A refused child verb journals
// nothing, not even the drive's transcript resolution or a migration
// (wf-checker's protocol probe on J). who is the caller, for prompt.
func (e *Engine) ChildVerbCheck(run RunID, verb, name, claim string, who harness.Caller) error {
	// One verb's process runs the same check more than once (the CLI's
	// preflight, then the verb): a pass is not repeated. The verb's own
	// cycle checks the claim again under the lock.
	key := strings.Join([]string{string(run), verb, name, claim, who.Harness, who.Session, who.Agent}, "\x00")
	e.mu.Lock()
	passed := e.opts.oneVerb && e.verbChecked[key]
	e.mu.Unlock()
	if passed {
		return nil
	}
	if err := e.childOffer(run, verb, name, claim, who); err != nil {
		return err
	}
	err := e.withDryRun(run, func(c *cycle) error {
		if verb == "prompt" {
			_, _, _, err := c.fetchable(name, claim, who)
			return err
		}
		_, _, err := c.settling(verb, name)
		return err
	})
	if err == nil && e.opts.oneVerb {
		e.mu.Lock()
		if e.verbChecked == nil {
			e.verbChecked = map[string]bool{}
		}
		e.verbChecked[key] = true
		e.mu.Unlock()
	}
	return err
}

// tokenSegment is the harness segment a first fetch's token must come from:
// a still-handed unit's current segment, else (a started claim not yet
// fetched) the segment of the hand-out it was claimed under.
func tokenSegment(u *unit, current int64) int64 {
	if u.State == UnitHanded {
		return current
	}
	return u.HandSegment
}
