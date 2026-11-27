package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// agentOps is the typed half of an Agent: it issues the instruction and takes
// the submission.
type agentOps[S, O any] struct{ a *Agent[S, O] }

func (o agentOps[S, O]) routeFn(out O) func(Ctx) edgeUse {
	if o.a.route == nil {
		return nil
	}
	return func(ctx Ctx) edgeUse { return o.a.route(out, ctx).use() }
}

// execute issues the agent instruction: DoneWhen first (a met condition
// completes the state without issuing it), then resolve the prompt, bind,
// append the Inputs and Submission blocks, digest the declared observations and
// journal instruction_issued with a fresh token.
func (o agentOps[S, O]) execute(c *cycle, in *instance, n *node) error {
	k := o.a.k
	met, ok, err := c.doneWhenMet(in, n)
	if err != nil || !ok {
		return err
	}
	if met {
		var out O
		if raw, have := in.Outputs[n.name]; have {
			if resolved, err := c.decodeOwnedPathValues(raw); err == nil {
				raw = resolved
			}
			_ = json.Unmarshal(raw, &out)
		}
		raw, _ := json.Marshal(out)
		ctx, err := c.ctx(in)
		if err != nil {
			return c.halt(in, err.Error())
		}
		u, ok, err := c.route(in, n, o.routeFn(out), any(out), ctx)
		if err != nil || !ok {
			return err
		}
		plan, err := c.planOf(in, u, raw)
		if err != nil {
			return c.halt(in, err.Error())
		}
		return c.emit(in, JournalCompletedByObservation, Accepted, CompletedByObservationData{Output: raw, Plan: plan})
	}

	if err := c.workdirRefused(in); err != nil {
		return c.failErr(in, n, err)
	}
	token := c.tokenFor(in)
	submit := c.commandLine(c.e.opts.SubmitLine(c.run, token))
	c.issuing = in // the render and dispatch below are this issue's (unitModels)
	c.issueReads, c.issueToken = map[string]ObservedData{}, token
	defer func() { c.issuing, c.issueReads, c.issueToken = nil, nil, "" }()
	text, inputs, reads, ok, err := o.render(c, in, n, submit)
	if err != nil || !ok {
		return err
	}
	dispatch, err := c.mapModel(in, hintsOf(k.dispatch))
	if errors.Is(err, ErrUnavailable) {
		return c.fail(in, n, Unavailable, []string{err.Error()}, "")
	}
	if err != nil {
		return c.failErr(in, n, err)
	}
	descs, err := c.bindDescs(in, k.inputs)
	if err != nil {
		return c.failErr(in, n, err)
	}
	inputDigest, err := c.inputDigest(in, descs, "issue")
	switch {
	case errors.Is(err, ErrUnavailable):
		inputDigest = "" // read at issue during an outage: no token, nothing to compare
	case err != nil:
		return err
	}
	return c.emit(in, JournalInstructionIssued, 0, InstructionIssuedData{
		Token: token, Prompt: text, Inputs: inputs, Submit: submit, GitAuthority: k.gitAuth,
		Dispatch: dispatch, Digest: obsDigest(reads), InputDigest: inputDigest, Workspace: c.workdir(in),
	})
}

// render builds the full prompt text of the state for in: the resolved and
// bound prompt, the Inputs block and the Submission block ending in submit. It
// also returns the input values and the declared observations it read. ok is
// false when a failure was journaled instead.
func (o agentOps[S, O]) render(c *cycle, in *instance, n *node, submit string) (text string, inputs map[string]string, reads []ObservedData, ok bool, err error) {
	k := o.a.k
	sc := c.scope(in)
	text, ok, err = o.promptText(c, in, n)
	if err != nil || !ok {
		return "", nil, nil, false, err
	}
	ins, err := sc.inputValues(k.prompt.inputs)
	if errors.Is(err, ErrUnavailable) {
		return "", nil, nil, false, c.fail(in, n, Unavailable, []string{err.Error()}, "")
	}
	if err != nil {
		return "", nil, nil, false, c.failErr(in, n, err)
	}
	if err := c.adoptBaseline(in, n, k); err != nil {
		return "", nil, nil, false, err
	}

	originals := map[string]string{}
	for i, pi := range ins {
		originals[pi.name] = pi.value
		if c.promptPresentation != nil && !pi.shell {
			// A shell word stays exactly as bound: a note, a page break or
			// an escape would change the command it completes.
			ins[i].value, err = c.promptPresentation(pi.name, pi.value)
			if err != nil {
				return "", nil, nil, false, err
			}
		}
	}
	var used []bool
	if text, used, err = sc.bindPrompt(text, ins); err != nil {
		return "", nil, nil, false, c.failErr(in, n, err)
	}
	descs, err := c.bindDescs(in, k.observes)
	if err != nil {
		return "", nil, nil, false, c.failErr(in, n, err)
	}
	reads = make([]ObservedData, 0, len(descs))
	for _, d := range descs {
		od, err := c.readObs(in, d, "issue", false)
		if err != nil {
			return "", nil, nil, false, err
		}
		reads = append(reads, od)
	}
	var b strings.Builder
	// The rules for the run open every agent prompt the engine issues, ahead
	// of the definition's text, which stays as written: the same template
	// dispatched outside the engine carries neither the rules nor a
	// placeholder for them.
	b.WriteString(c.rulesHead())
	if pred := in.Unit; pred != nil && pred.Predecessor != "" && c.fetched(pred.Predecessor) {
		name := pred.Name
		if name == "" {
			name = c.st.unitName(in)
		}
		base, k, _ := strings.Cut(name, "#")
		fmt.Fprintf(&b, "attempt %s of %s; attempt %s was lost while running; its workspace %s may hold partial work\n\n",
			k, base, strings.TrimPrefix(pred.Predecessor, base+"#"), c.workdir(in))
	}
	if why := previousRefusal(in); why != nil {
		b.WriteString(attemptNote("Your predecessor's output was refused", why, "Avoid this."))
	}
	if why := previousBlocked(in); why != nil {
		b.WriteString(attemptNote("The attempt before yours ended blocked", why, "The unit was started again with that known: if the same thing still blocks you, run failed again saying what you need."))
	}
	b.WriteString(strings.TrimRight(text, "\n"))
	if blk := inputsBlock(ins, used); blk != "" {
		b.WriteString("\n\n" + blk)
	}
	output := ""
	if in.Unit != nil && (in.Unit.State == UnitHanded || in.Unit.State.claimed()) {
		output, err = c.outputPath(in.Unit.Name)
		if err != nil {
			return "", nil, nil, false, err
		}
	}
	if err := c.copyReturnedArtifacts(in, n); err != nil {
		return "", nil, nil, false, c.failErr(in, n, err)
	}
	if blk, err := c.artifactsBlock(in, n); err != nil {
		return "", nil, nil, false, c.failErr(in, n, err)
	} else if blk != "" {
		b.WriteString("\n\n" + blk)
	}
	b.WriteString("\n\n" + submissionBlock(reflect.TypeFor[O](), in.def.enums, c.workdir(in), submit, output))
	for _, pi := range ins {
		if inputs == nil {
			inputs = map[string]string{}
		}
		inputs[pi.name] = originals[pi.name]
	}
	return b.String(), inputs, reads, true, nil
}

// previousRefusal is the reasons a later attempt of the state is shown: the
// state's last validation refusal, on attempts after the first.
func previousRefusal(in *instance) []string {
	if in.Attempt > 1 && len(in.LastRefusal) > 0 {
		return in.LastRefusal
	}
	return nil
}

// attemptNote is the paragraph that tells a later attempt why the one before
// it ended: lead, the reasons, then tail. The reasons are model text (a
// blocked cause is the child's own words, and a refusal can quote its
// output), so they must not add prompt structure. Reasons that fit on one
// line stay in the sentence, where inline text cannot start a block; any
// other reasons go one per line in a fence after lead (fenceValue), carried
// verbatim.
func attemptNote(lead string, why []string, tail string) string {
	if joined := strings.Join(why, "; "); !strings.ContainsAny(joined, "\r\n") {
		return lead + ": " + joined + ". " + tail + "\n\n"
	}
	return lead + ":\n\n" + fenceValue(strings.Join(why, "\n")) + "\n\n" + tail + "\n\n"
}

// previousBlocked is the cause a later attempt of the state is shown when
// the attempt before it ended on a failure with no route and the unit was
// started again (instance.LastBlocked).
func previousBlocked(in *instance) []string {
	if in.Attempt > 1 && len(in.LastBlocked) > 0 {
		return in.LastBlocked
	}
	return nil
}

// fetched reports whether a prompt_fetched record names the unit: it ran, so a
// reissue may find partial work.
func (c *cycle) fetched(name string) bool {
	for _, r := range c.recs {
		if r.Kind != JournalPromptFetched {
			continue
		}
		var d PromptFetchedData
		if json.Unmarshal(r.Data, &d) == nil && d.Name == name {
			return true
		}
	}
	return false
}

// prompter is implemented by the ops of a kind that renders a prompt at fetch
// time (wf prompt).
type prompter interface {
	renderPrompt(c *cycle, in *instance, n *node, submit string) (string, map[string]string, error)
}

// renderPrompt renders the state's prompt for a subagent that fetches it. A failed
// render is journaled by render; the caller sees ErrRejected.
func (o agentOps[S, O]) renderPrompt(c *cycle, in *instance, n *node, submit string) (string, map[string]string, error) {
	text, inputs, _, ok, err := o.render(c, in, n, submit)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("%w: the prompt cannot be rendered: see the journal", ErrRejected)
	}
	return text, inputs, nil
}

// promptText resolves the prompt template text: inline text, a file, or the
// PromptFor candidate the selector picks for the instance's input. ok is false
// when a failure was journaled instead.
func (o agentOps[S, O]) promptText(c *cycle, in *instance, n *node) (string, bool, error) {
	p := o.a.k.prompt
	ref := p.path
	switch {
	case p.item != nil:
		if p.sel == nil {
			return "", false, c.fail(in, n, Blocked, []string{"PromptFor has no selector"}, "")
		}
		item := reflect.New(p.item)
		if len(in.Input) > 0 {
			raw, err := c.decodeOwnedPathValues(in.Input)
			if err != nil {
				return "", false, c.fail(in, n, Blocked, []string{err.Error()}, "")
			}
			if err := json.Unmarshal(raw, item.Interface()); err != nil {
				return "", false, c.fail(in, n, Blocked, []string{"instance input does not decode as the PromptFor item: " + err.Error()}, "")
			}
		}
		ref = p.sel(item.Elem().Interface())
		if !contains(p.candidates, ref) {
			rd, err := c.decide(in, n, Blocked, []string{fmt.Sprintf("prompt selector returned %q, outside its candidates", ref)}, "")
			if err != nil {
				return "", false, err
			}
			return "", false, c.emit(in, JournalPromptOffCandidates, Blocked, PromptOffCandidatesData{
				Returned: ref, Candidates: p.candidates, RefusedData: rd,
			})
		}
	case ref == "":
		return p.text, true, nil
	}
	pr, err := ParsePromptRef(ref)
	if err != nil {
		return "", false, c.failErr(in, n, err)
	}
	params := map[string]string{}
	for _, m := range promptPlaceholder.FindAllString(ref, -1) {
		name := m[1 : len(m)-1]
		v, ok := c.scope(in).lookup(name)
		if !ok && name == recordsName {
			return "", false, c.failErr(in, n, c.scope(in).unresolved(m, "prompt reference "+ref))
		}
		if ok {
			params[name] = v
		}
	}
	// In a fork, a prompt file bound to a source path is never read: the
	// agent would be briefed from the source run's file.
	if path, err := pr.Path(params, c.workdir(in)); err == nil {
		if err := c.checkForkFile(path, "prompt file"); err != nil {
			return "", false, c.failErr(in, n, err)
		}
	}
	text, err := pr.Resolve(params, c.workdir(in))
	if errors.Is(err, ErrPromptNotFound) && c.e.replaySource != nil {
		// A replay applies no effect, so a file an earlier effect wrote is
		// taken from the source run.
		if path, perr := pr.Path(params, c.workdir(in)); perr == nil && c.e.replaySource(path) {
			text, err = pr.Resolve(params, c.workdir(in))
		}
	}
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return "", false, c.fail(in, n, Unavailable, []string{err.Error()}, "")
	case err != nil:
		return "", false, c.failErr(in, n, err)
	}
	return text, true, nil
}

func fileHash(workspace, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return hashBytes(b)
}

// artifactsBlock lists the allocated artifacts state n produces, with the path
// each must be written to; "" when it declares none.
func (c *cycle) artifactsBlock(in *instance, n *node) (string, error) {
	as := allocatedOf(n)
	if len(as) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("## Artifacts\n\nWrite each artifact this step produces to its allocated path, exactly as printed; done is refused while a required one is missing.\n\n")
	for _, a := range as {
		p, err := c.allocatedPath(in, a)
		if err != nil {
			return "", err
		}
		opt := ""
		if a.optional {
			opt = " (optional: leave it unwritten when there is nothing to record)"
		}
		fmt.Fprintf(&b, "- %s: %s%s\n", a.name, p, opt)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// submit takes a submission for the open attempt: journal submitted, then
// decide it.
func (o agentOps[S, O]) submit(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error) {
	arts := map[string]ArtifactRef{}
	for name, p := range s.Artifacts {
		arts[name] = ArtifactRef{Path: p, Hash: fileHash(c.workdir(in), p)}
	}
	// An allocated artifact is recorded at its allocated path, whatever the
	// submission says: the path is the engine's.
	for _, a := range allocatedOf(n) {
		p, err := c.allocatedPath(in, a)
		if err != nil {
			return SubmitResult{}, err
		}
		arts[a.name] = ArtifactRef{Path: p, Hash: fileHash(c.workdir(in), p)}
	}
	fetched := c.lastPromptFetch(in)
	// A citation is journaled as a run-relative reference.
	output, err := c.citationsMarked(s.Output)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := c.emit(in, JournalSubmitted, 0, SubmittedData{Token: s.Token, Output: output, Artifacts: arts, Tokens: s.Tokens, Normalized: s.Normalized, PromptHash: fetched.PromptHash, PromptShapeHash: fetched.PromptShapeHash}); err != nil {
		return SubmitResult{}, err
	}
	return o.decide(c, in, n, s)
}

// decide is everything after the submitted record: decode into O, re-read the
// declared observations (journaled), run the checks, then route. A journaled
// submission with no decision is decided again by it.
func (o agentOps[S, O]) decide(c *cycle, in *instance, n *node, s Submission) (SubmitResult, error) {
	k := o.a.k
	failed := func(class OutcomeClass, reasons []string, fp string) (SubmitResult, error) {
		return SubmitResult{Class: class, Refusal: reasons}, c.fail(in, n, class, reasons, fp)
	}
	failedErr := func(err error) (SubmitResult, error) {
		return SubmitResult{Class: Blocked, Refusal: []string{err.Error()}}, c.failErr(in, n, err)
	}
	refuse := func(reasons []string, fp string) (SubmitResult, error) { return c.refuse(in, n, reasons, fp) }

	// A journaled submission (decided again, or replayed) holds its
	// citations as run-relative references: resolve them for this run.
	// A marker never reaches typed decoding unresolved.
	resolved, err := c.decodeOwnedPathValues(s.Output)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("resolving the submission's recorded path references: %w", err)
	}
	s.Output = resolved
	out, stage, err := checkedAgentOutput[O](s.Output, in.def)
	if err != nil {
		reason := err.Error()
		if stage == "decode" {
			reason = "undecodable submission: " + decodeReason(err)
		}
		return refuse([]string{reason}, stage+":"+hashBytes(s.Output)[:16])
	}
	// A declared allocated artifact the producer did not write is refused with
	// the attempt open (A2): the producer was told the path, and fixes it in
	// place.
	// A file that exists satisfies the check, whichever attempt wrote it: the
	// producer attests that it is this attempt's artifact.
	var missing []string
	for _, a := range allocatedOf(n) {
		if a.optional {
			continue
		}
		p, err := c.allocatedPath(in, a)
		if err != nil {
			return SubmitResult{}, err
		}
		if fileHash(c.workdir(in), p) == "" {
			missing = append(missing, fmt.Sprintf("artifact %s was not written to %s", a.name, p))
		}
	}
	if len(missing) > 0 {
		return refuse(missing, "artifacts:"+strings.Join(missing, ";"))
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return refuse([]string{"submission does not re-encode: " + err.Error()}, "")
	}

	descs, err := c.bindDescs(in, k.observes)
	if err != nil {
		return failedErr(err)
	}
	reads := make([]ObservedData, 0, len(descs))
	read := map[string]bool{}
	for _, d := range descs {
		od, err := c.readObs(in, d, "check", true)
		if err != nil {
			return SubmitResult{}, err
		}
		reads = append(reads, od)
		read[canonDesc(d)] = true
	}
	// A changed observation token is Stale only where the state routes Stale:
	// an agent's declared observations are its checks', which read what the
	// agent itself changes, so a default re-issue would discard its work.
	if in.Issued != nil && obsDigest(reads) != in.Issued.Digest {
		if _, ok := classRoute(n, Stale); ok {
			return failed(Stale, []string{"declared observations changed since the instruction was issued"}, "")
		}
	}
	// The input observations (Agent.Observes) are the staleness token: a
	// change re-issues the step or takes its Stale route.
	if len(k.inputs) > 0 && in.Issued != nil {
		inputDescs, err := c.bindDescs(in, k.inputs)
		if err != nil {
			return failedErr(err)
		}
		dg, err := c.inputDigest(in, inputDescs, "check")
		switch {
		case errors.Is(err, ErrUnavailable):
			return failed(Unavailable, []string{err.Error()}, "")
		case err != nil:
			return failedErr(err)
		}
		// The baseline is the reads journaled for this issue's token (an
		// adopted attempt's first render included); a journal from before
		// they carried it keeps the issue's InputDigest.
		base := in.Issued.InputDigest
		if b, ok := c.issueBaseline(in, inputDescs); ok {
			base = b
		}
		if base != "" && dg != base {
			return failed(Stale, []string{"input observations changed since the instruction was issued"}, "")
		}
	}
	// Submission-scoped observations: built from out, read and journaled
	// after the Stale comparison, which covers only the definition-time set.
	for _, ck := range o.a.checks {
		from, err := c.bindDescs(in, ck.fromDescs(out))
		if err != nil {
			return failedErr(err)
		}
		for _, d := range from {
			if read[canonDesc(d)] {
				continue
			}
			if _, err := c.readObs(in, d, "check", true); err != nil {
				return SubmitResult{}, err
			}
			read[canonDesc(d)] = true
		}
	}

	contexts := &CheckContexts{Version: 1, Checks: []RecordedCheck{}}
	var refusals, names []string
	var blocked error
	unavailable := false
	for _, ck := range o.a.checks {
		var declared []ObservationDesc
		for _, r := range ck.observes {
			declared = append(declared, r.Desc())
		}
		declared = append(declared, ck.fromDescs(out)...)
		ctx, err := c.ctx(in, declared...)
		if err != nil {
			return failedErr(err)
		}
		record := RecordedCheck{Name: ck.Name}
		cerr := ck.fn(out, &recordingCtx{live: ctx, record: &record, declared: declared})
		contexts.Checks = append(contexts.Checks, record)
		switch {
		case ctx.blocked:
			blocked = ctx.blockedErr()
		case cerr != nil:
			names = append(names, ck.Name)
			refusals = append(refusals, ck.Name+": "+cerr.Error())
			unavailable = unavailable || errors.Is(cerr, ErrUnavailable)
		}
	}
	switch {
	case blocked != nil:
		return failedErr(blocked)
	case unavailable:
		return failed(Unavailable, refusals, "")
	case len(refusals) > 0:
		sort.Strings(names)
		fp := strings.Join(names, ",") + "|" + hashBytes(raw)[:16]
		if r := retryOf(n); r != nil && r.fp != nil {
			if ctx, err := c.ctx(in, descsOfObs(k.observes)...); err == nil {
				fp = r.fp(ctx)
			}
		}
		return refuse(refusals, fp)
	}

	ctx, err := c.ctx(in)
	if err != nil {
		return failedErr(err)
	}
	if msg, bad := n.outOfCases(any(out)); bad {
		return refuse([]string{msg}, "")
	}
	u, ok, err := c.route(in, n, o.routeFn(out), any(out), ctx)
	if err != nil {
		return SubmitResult{}, err
	}
	if !ok {
		return SubmitResult{Class: Blocked, Refusal: []string{"route outside its declared candidates or undeclared read"}}, nil
	}
	plan, err := c.planOf(in, u, raw)
	if err != nil {
		return failedErr(err)
	}
	if raw, err = c.citationsMarked(raw); err != nil {
		return SubmitResult{}, err
	}
	if err := c.emit(in, JournalAccepted, Accepted, AcceptedData{Output: raw, Plan: plan, Context: contexts}); err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Class: Accepted}, nil
}

func descsOfObs(ds []ObservationDesc) []ObservationDesc { return ds }

// refuse is the single decision point for a validation refusal of a
// submission, for every Refused outcome of agentOps.decide (undecodable
// output, no re-encode, check refusals, outOfCases): the refusal spends the
// attempt, as Refused with the fingerprint the caller computed (C5). A child checks its output first with validate, which
// never counts. It does not touch Blocked, Unavailable or Stale outcomes or
// rejections, compute fingerprints or print anything.
func (c *cycle) refuse(in *instance, n *node, reasons []string, fp string) (SubmitResult, error) {
	return SubmitResult{Class: Refused, Refusal: reasons}, c.failSubmitted(in, n, reasons, fp)
}

// mapModel sets the concrete model of a dispatch hint at issue: the current
// segment's models (segmentModels, an earlier revision) for its harness, through
// spawnDispatch. The hand-out resolves again for its own segment
// (callerDispatch), so what a child spawns with is the hand-out's. The
// state's own hints, and so the definition hash, are untouched.
func (c *cycle) mapModel(in *instance, h DispatchHints) (DispatchHints, error) {
	d, err := c.resolveDispatch(h, c.segmentHarness(len(c.st.segments)-1), c.unitModels(in, JournalInstructionIssued))
	if err != nil {
		return DispatchHints{}, err
	}
	// Never the coordinator's harness as a fallback: a unit whose harness
	// is unavailable fails with class Unavailable.
	if why := c.familyUnavailable(string(d.Family)); d.Family != "" && why != "" {
		return DispatchHints{}, fmt.Errorf("%w: %s", ErrUnavailable, why)
	}
	return d, nil
}

// resolveDispatch is what a unit with hints h spawns with for a coordinator
// of harness coord whose segment's models are sm. A unit of another harness
// (Family): its tier resolves on that harness's table, it names no agent
// type (a headless launch has none), and its Family is the resolved
// harness. A family that resolves to coord is an ordinary subagent. A
// FamilyOther with no other family for coord is ErrUnavailable.
func (c *cycle) resolveDispatch(h DispatchHints, coord string, sm SegmentModels) (DispatchHints, error) {
	if h.Family == "" {
		return spawnDispatch(h, coord, sm), nil
	}
	target, ok := resolveFamily(h.Family, coord, c.headless())
	switch {
	case !ok:
		return DispatchHints{}, errNoFamily(normalizeHarness(coord))
	case target == "":
		h.Family = ""
		return spawnDispatch(h, coord, sm), nil
	}
	d := spawnDispatch(h, target, c.childModels(sm, coord, target))
	d.AgentType, d.Family = "", Family(target)
	return d, nil
}

// inputDigest reads already-bound input observations and returns their
// digest. An unavailable read is ErrUnavailable: an outage is not a change.
func (c *cycle) inputDigest(in *instance, descs []ObservationDesc, purpose string) (string, error) {
	if len(descs) == 0 {
		return "", nil
	}
	reads := make([]ObservedData, 0, len(descs))
	for _, d := range descs {
		var od ObservedData
		var err error
		if c.issuing == in && purpose == "issue" {
			od, err = c.issueRead(in, d, purpose)
		} else {
			od, err = c.readObs(in, d, purpose, true)
		}
		if err != nil {
			return "", err
		}
		if od.Unavailable {
			return "", fmt.Errorf("%w: input %s.%s", ErrUnavailable, d.Adapter, d.Op)
		}
		reads = append(reads, od)
	}
	return obsDigest(reads), nil
}

// issueRead is the issue in progress's one read of d: read and journaled
// for its token the first time, and journaled again under purpose (the
// value a further input over d rendered) without another read.
func (c *cycle) issueRead(in *instance, d ObservationDesc, purpose string) (ObservedData, error) {
	canon := canonDesc(d)
	od, ok := c.issueReads[canon]
	if !ok {
		od, err := c.readObsFor(in, d, purpose, c.issueToken, true)
		if err == nil {
			c.issueReads[canon] = od
		}
		return od, err
	}
	if od.Purpose != purpose {
		od.Purpose, od.Alias = purpose, true
		if err := c.emit(in, JournalObserved, 0, od); err != nil {
			return od, err
		}
	}
	return od, nil
}

// tokenRead is the latest input observation of canon journaled for the issue
// token, under purpose ("" any); a journal from before reads carried their
// issue has none.
func (c *cycle) tokenRead(in *instance, token, purpose, canon string) (ObservedData, bool) {
	if token == "" {
		return ObservedData{}, false
	}
	for i := len(c.recs) - 1; i >= 0; i-- {
		r := c.recs[i]
		if r.Kind != JournalObserved || r.Instance != in.ID {
			continue
		}
		var od ObservedData
		if json.Unmarshal(r.Data, &od) == nil && od.Issue == token && (purpose == "" || od.Purpose == purpose) && canonDesc(od.Desc) == canon {
			return od, true
		}
	}
	return ObservedData{}, false
}

// issueBaseline is the staleness baseline of the open issue from the reads
// journaled for its token, over descs in order: ok is false when one has
// none (a journal from before reads carried their issue: the issue's own
// InputDigest stands), and the baseline is "" (nothing to compare) when one
// was unavailable. An ordinary read error digests as itself (obsDigest).
func (c *cycle) issueBaseline(in *instance, descs []ObservationDesc) (string, bool) {
	if in.Issued == nil {
		return "", false
	}
	reads := make([]ObservedData, 0, len(descs))
	for _, d := range descs {
		od, ok := c.tokenRead(in, in.Issued.Token, "", canonDesc(d))
		if !ok {
			return "", false
		}
		if od.Unavailable {
			return "", true // an outage is not a change: nothing to compare
		}
		reads = append(reads, od)
	}
	return obsDigest(reads), true
}

// adoptBaseline completes the open issue's baseline after an adoption read
// an observed input for it: each input observation with no read for the
// token is read now, as the issue had none of the adopted definition's.
func (c *cycle) adoptBaseline(in *instance, n *node, k *agentKind) error {
	if c.issuing == in || in.Issued == nil || len(k.inputs) == 0 {
		return nil
	}
	descs, err := c.bindDescs(in, k.inputs)
	if err != nil {
		return nil // the render reports it
	}
	token := in.Issued.Token
	adopted := false
	for _, d := range descs {
		if _, ok := c.tokenRead(in, token, "", canonDesc(d)); ok {
			adopted = true
		}
	}
	if !adopted {
		return nil
	}
	for _, d := range descs {
		if _, ok := c.tokenRead(in, token, "", canonDesc(d)); ok {
			continue
		}
		if _, err := c.readObsFor(in, d, "issue", token, true); err != nil {
			return err
		}
	}
	return nil
}

// decodeReason says what a result's decode error objects to in the result's
// own terms: the field, what it holds, and what to give instead,
// naming the Go type (and an integer's range) the field takes. Any other
// decode error keeps its own text.
func decodeReason(err error) string {
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) || te.Type == nil {
		return err.Error()
	}
	field := te.Field
	if field == "" {
		field = "the result"
	} else {
		field = "result field " + field
	}
	return fmt.Sprintf("%s is a JSON %s: give %s instead", field, te.Value, jsonWant(te.Type))
}

// jsonWant names the JSON value a Go type decodes from, with the type and,
// for an integer, its range.
func jsonWant(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a JSON string"
	case reflect.Bool:
		return "a JSON boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		bits := t.Bits()
		return fmt.Sprintf("a whole number from %d to %d (Go %s)", int64(-1)<<(bits-1), int64(1)<<(bits-1)-1, t)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return fmt.Sprintf("a whole number from 0 to %d (Go %s)", uint64(1)<<(t.Bits()-1)*2-1, t)
	case reflect.Float32, reflect.Float64:
		return fmt.Sprintf("a JSON number (Go %s)", t)
	case reflect.Slice, reflect.Array:
		return "a JSON array"
	case reflect.Pointer:
		return jsonWant(t.Elem())
	}
	return "a JSON object"
}

// observedInput is an ObservedAs input's rendered value. The issue in
// progress reads it once per descriptor and journals it for its token; any
// later render takes the read journaled for the open issue's token, and
// reads (and journals) it once for that token when there is none, as for an
// attempt adopted onto the definition while open.
func (c *cycle) observedInput(in *instance, d inputDesc) (string, error) {
	descs, err := c.bindDescs(in, []ObservationDesc{*d.obs})
	if err != nil {
		return "", err
	}
	desc := descs[0]
	if err := c.checkForkValue(desc.Args, "prompt input "+d.name); err != nil {
		return "", err
	}
	purpose, canon := "input:"+d.name, canonDesc(desc)
	var od ObservedData
	switch {
	case c.issuing == in:
		if od, err = c.issueRead(in, desc, purpose); err != nil {
			return "", err
		}
	default:
		token := ""
		if in.Issued != nil {
			token = in.Issued.Token
		}
		var ok bool
		if od, ok = c.tokenRead(in, token, purpose, canon); ok {
			break
		}
		if od, ok = c.tokenRead(in, token, "", canon); ok {
			od.Purpose, od.Alias = purpose, true
			if err := c.emit(in, JournalObserved, 0, od); err != nil {
				return "", err
			}
			break
		}
		if od, err = c.readObsFor(in, desc, purpose, token, true); err != nil {
			return "", err
		}
	}
	switch {
	case od.Unavailable:
		return "", inputOutage("input " + d.name + ": " + od.Error)
	case od.Error != "":
		return "", fmt.Errorf("input %s: %s", d.name, od.Error)
	}
	return d.render(od.Value)
}

// inputOutage is an observed prompt input's unavailable read: its text is the
// read's own error, already naming the outage.
type inputOutage string

func (e inputOutage) Error() string { return string(e) }
func (inputOutage) Unwrap() error   { return ErrUnavailable }

// fenceNote is the submitted record's note for a result file read from
// inside its code fence.
const fenceNote = "result file was one fenced code block; parsed its contents"

// unfencedFor applies the one tolerated normalization of a result file to s:
// a file the CLI handed over as text (not JSON) that is exactly one fenced
// block around valid JSON is replaced by the block's contents, with a note.
// It applies only where the text itself does not decode into n's output type,
// so a state whose output takes text keeps the file as written.
func unfencedFor(n *node, s Submission) Submission {
	k, ok := n.kind.(*agentKind)
	if !ok || !s.textWrapped || k.outT == nil {
		return s
	}
	if json.Unmarshal(s.Output, reflect.New(k.outT).Interface()) == nil {
		return s
	}
	var text string
	if json.Unmarshal(s.Output, &text) != nil {
		return s
	}
	inner, ok := unfenced([]byte(text))
	if !ok {
		return s
	}
	s.Output, s.Normalized, s.textWrapped = inner, fenceNote, false
	return s
}
