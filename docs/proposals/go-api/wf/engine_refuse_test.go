package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// C5: a child checks its output with validate, which runs done's checks and
// records nothing; a refused done spends the attempt, and later attempts are
// shown why.

const enumRefusal = `output.Verdict: value maybe is outside enum oe-verdict (pass, fail)`

func refusedLine(reasons ...string) string {
	return "Your predecessor's output was refused: " + strings.Join(reasons, "; ") + ". Avoid this."
}

// refusedRecords decodes the run's refused records.
func refusedRecords(t *testing.T, h *harness, run wf.RunID) []wf.RefusedData {
	t.Helper()
	var out []wf.RefusedData
	for _, r := range h.kind(run, wf.JournalRefused) {
		var d wf.RefusedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// startedUnit hands out the next unit, claims it and fetches its prompt.
func startedUnit(t *testing.T, h *harness, run wf.RunID, agent string) (wf.Instruction, string) {
	t.Helper()
	ins := h.handOut(run, 1)
	if len(ins) != 1 {
		t.Fatalf("HandOut(1) = %+v, want one unit", ins)
	}
	if err := h.e.Started(run, ins[0].Name, wf.Claim{Harness: "claude", Session: "sess-1", Agent: agent}); err != nil {
		t.Fatal(err)
	}
	prompt, err := h.e.Prompt(run, ins[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	return ins[0], prompt
}

func done(t *testing.T, h *harness, run wf.RunID, name, out string) wf.SubmitResult {
	t.Helper()
	res, err := h.e.Done(run, name, wf.Submission{Output: json.RawMessage(out)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A child validates free text in an enum field: refused with exit 5 and
// nothing recorded. It fixes the output, validates it clean, and done is
// accepted on attempt 1 with no refusal journaled.
func TestValidateThenDoneOnSameAttempt(t *testing.T) {
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: outOfEnumDef(false)}}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	cli := func(args ...string) (int, string, string) {
		var o, er bytes.Buffer
		code := fixtureCLI(cfg, append(args, "--state-dir", state), &o, &er)
		return code, o.String(), er.String()
	}
	code, o, er := cli("run", "out-of-enum", "--workspace", ws, "--run-id", "oe-1")
	if code != 0 {
		t.Fatalf("run = %d %s", code, er)
	}
	run := strings.TrimSpace(o)
	var handed []wf.Instruction
	if code, o, er = cli("next", run, "1", "--json"); code != 0 || json.Unmarshal([]byte(o), &handed) != nil || len(handed) != 1 {
		t.Fatalf("next 1 = %d %q %s", code, o, er)
	}
	in := handed[0]
	if code, _, er = cli("dev", "started", run, in.Name, "--harness", "claude", "--session", "s", "--agent", "a"); code != 0 {
		t.Fatalf("started = %d %s", code, er)
	}
	journal := func() []string {
		code, o, er := cli("journal", run)
		if code != 0 {
			t.Fatalf("journal = %d %s", code, er)
		}
		return strings.Split(strings.TrimSpace(o), "\n")
	}

	if code, _, er = cli("validate", run, in.Name); code != 5 || !strings.Contains(er, "validate: refused: no output file at "+in.Output) {
		t.Fatalf("validate with no output = %d %q, want 5 naming the file", code, er)
	}
	before := journal()
	writeAllocated(t, in.Output, `{"Verdict":"maybe"}`)
	code, o, er = cli("validate", run, in.Name)
	// A fixable refusal ends with fix-and-retry, not the stop tail.
	want := "validate: refused: " + enumRefusal + "\nvalidate: nothing was recorded: fix the output and validate again, then run done\n" + wf.FixableRefusalTail + "\n"
	if code != 5 || er != want {
		t.Fatalf("validate with free text = %d %q, want 5 %q", code, er, want)
	}
	var res wf.SubmitResult
	if err := json.Unmarshal([]byte(o), &res); err != nil || res.Class != wf.Refused || !slices.Equal(res.Refusal, []string{enumRefusal}) {
		t.Fatalf("validate printed %q (%v), want Refused", o, err)
	}
	if after := journal(); !slices.Equal(after, before) {
		t.Fatalf("validate wrote %d journal records", len(after)-len(before))
	}

	writeAllocated(t, in.Output, `{"Verdict":"pass"}`)
	if code, _, er = cli("validate", run, in.Name); code != 0 || er != "validate: ok: done would accept this output\n" {
		t.Fatalf("validate with the fixed output = %d %q, want 0", code, er)
	}
	if code, o, er = cli("done", run, in.Name); code != 0 {
		t.Fatalf("done with the fixed output = %d %q %s, want 0", code, o, er)
	}
	code, o, er = cli("status", run, "--json")
	var st wf.RunStatus
	if code != 0 || json.Unmarshal([]byte(o), &st) != nil {
		t.Fatalf("status = %d %q %s", code, o, er)
	}
	statusOf(t, st, wf.RunEnded, "eDone")
	count := map[wf.JournalKind]int{}
	for _, line := range journal() {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		count[r.Kind]++
	}
	if count[wf.JournalInstructionIssued] != 1 || count[wf.JournalSubmitted] != 1 || count[wf.JournalRefused] != 0 || count[wf.JournalSubmissionRefused] != 0 {
		t.Fatalf("journal kinds = %v, want one instruction_issued, one submitted and no refusal", count)
	}
}

// validate rejects a unit that is not live and writes nothing (its F2 read
// gate is in TestDoneRefusedUntilTheBodyIsRead).
func TestValidateKeepsDonesGates(t *testing.T) {
	d := outOfEnumDef(false)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	ins := h.handOut(run, 1)
	if err := h.e.Started(run, ins[0].Name, wf.Claim{Harness: "claude", Session: "sess-1", Agent: "a"}); err != nil {
		t.Fatal(err)
	}
	n := len(h.records(run))
	if res, err := h.e.Validate(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"Verdict":"pass"}`)}); err != nil || res.Class != wf.Accepted {
		t.Fatalf("validate of a good output = %+v, %v, want Accepted", res, err)
	}
	if _, err := h.e.Validate(run, "nosuch#1", wf.Submission{Output: json.RawMessage(`{}`)}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("validate of an unknown unit = %v, want ErrRejected", err)
	}
	if got := len(h.records(run)); got != n {
		t.Fatalf("validate wrote %d records", got-n)
	}
}

// A refused done spends the attempt with the fingerprint the check computes;
// the next attempt carries the reasons in its instruction and its prompt.
func TestRefusalsSpendAttemptAndCarryReasons(t *testing.T) {
	d := outOfEnumDef(false) // Retry Const(2), exhausted edge to eC
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	first, prompt := startedUnit(t, h, run, "a")
	if first.PreviousRefusal != nil || strings.Contains(prompt, "predecessor's output was refused") {
		t.Fatalf("attempt 1 shows a refusal: %+v\n%s", first.PreviousRefusal, prompt)
	}
	res := done(t, h, run, first.Name, `{"Verdict":"maybe"}`)
	if res.Class != wf.Refused || !slices.Equal(res.Refusal, []string{enumRefusal}) {
		t.Fatalf("refused done = %+v, want the attempt spent", res)
	}
	rd := refusedRecords(t, h, run)
	if len(rd) != 1 || !rd[0].Spent || !rd[0].Submitted || !slices.Equal(rd[0].Reasons, []string{enumRefusal}) {
		t.Fatalf("refused = %+v, want one spent submission refusal", rd)
	}
	if n := len(h.kind(run, wf.JournalSubmissionRefused)); n != 0 {
		t.Fatalf("submission_refused = %d, want none", n)
	}

	second, prompt := startedUnit(t, h, run, "b")
	if second.Attempt != 2 || !slices.Equal(second.PreviousRefusal, []string{enumRefusal}) {
		t.Fatalf("attempt 2 = %+v, want previousRefusal %q", second, enumRefusal)
	}
	if !strings.Contains(prompt, refusedLine(enumRefusal)) {
		t.Fatalf("attempt 2 prompt lacks the predecessor-refused line:\n%s", prompt)
	}
	if res := done(t, h, run, second.Name, `{"Verdict":"pass"}`); res.Class != wf.Accepted {
		t.Fatalf("attempt 2 fixed = %+v, want Accepted", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "eDone")
}

// A halted attempt keeps its reasons across the halt and wf retry: the
// attempt retried grants shows them.
func TestRefusalReasonsSurviveRetried(t *testing.T) {
	d := lsDef("rf-halt", false) // no retry policy: a spent refusal halts
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in := h.next(run)[0]
	if res := h.submit(run, in, `"x"`); res.Class != wf.Refused {
		t.Fatalf("spent refusal = %+v", res)
	}
	if st := h.status(run); st.Phase != wf.RunBlocked {
		t.Fatalf("status = %+v, want halted", st)
	}
	if err := h.e.Retry(run, "", false); err != nil {
		t.Fatal(err)
	}
	again := h.next(run)
	if len(again) != 1 || again[0].Attempt != 2 || len(again[0].PreviousRefusal) != 1 ||
		!strings.HasPrefix(again[0].PreviousRefusal[0], "undecodable submission: ") ||
		!strings.Contains(again[0].Prompt, refusedLine(again[0].PreviousRefusal...)) {
		t.Fatalf("after retry next = %+v, want attempt 2 showing the refusal", again)
	}
}

type covFinding struct{ Type, Subject string }

type covOut struct{ Findings []covFinding }

type (
	covReview struct{}
	covDone   struct{}
	covStop   struct{}
)

// The coverage-review shape of the phase-2 loss: the validator matches the bare
// requirement id, and a GAP subject of free text leaves R2 unmapped. validate
// shows the child the refusal; it fixes the subject and done is accepted on
// attempt 1.
func TestCoverageRefusalRecoversWithoutRetry(t *testing.T) {
	d := wf.New("cov-review")
	a := wf.NewAgent[covReview, covOut](d)
	ok := wf.NewTerminal[covDone](d, wf.Success)
	stop := wf.NewTerminal[covStop](d, wf.Stopped)
	a.Spec(wf.PromptText("Review coverage."), wf.Retry[covReview](wf.Const(2), a.To(stop, "coverage review failed twice")),
		wf.Checkf("coverage", func(o covOut, _ wf.Ctx) error {
			for _, f := range o.Findings {
				if f.Type == "GAP" && f.Subject == "R2" {
					return nil
				}
			}
			return errors.New("requirement R2 is unmapped and has no GAP finding")
		}))
	a.Then(a.To(ok, "reviewed"))

	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in, _ := startedUnit(t, h, run, "a")
	res, err := h.e.Validate(run, in.Name, wf.Submission{Output: json.RawMessage(`{"Findings":[{"Type":"GAP","Subject":"R2 / B1 (cross-platform)"}]}`)})
	if err != nil || res.Class != wf.Refused || !slices.Equal(res.Refusal, []string{"coverage: requirement R2 is unmapped and has no GAP finding"}) {
		t.Fatalf("validate free-text subject = %+v, %v, want a refusal naming R2", res, err)
	}
	if res := done(t, h, run, in.Name, `{"Findings":[{"Type":"GAP","Subject":"R2"}]}`); res.Class != wf.Accepted {
		t.Fatalf("fixed subject = %+v, want Accepted", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "covDone")
	if n := len(refusedRecords(t, h, run)); n != 0 || len(h.kind(run, wf.JournalInstructionIssued)) != 1 {
		t.Fatalf("refused = %d, issued = %d: want no retry spent", n, len(h.kind(run, wf.JournalInstructionIssued)))
	}
}

// oldOpenRefusal journals the open refusal an engine before C5 left on the
// unit's attempt (submitted, then submission_refused), so the tests below
// show that a run journaled then still resolves it.
func oldOpenRefusal(t *testing.T, h *harness, run wf.RunID, out, fp string, reasons ...string) {
	t.Helper()
	issued := h.kind(run, wf.JournalInstructionIssued)
	last := issued[len(issued)-1]
	var d wf.InstructionIssuedData
	if err := json.Unmarshal(last.Data, &d); err != nil {
		t.Fatal(err)
	}
	unlock, err := h.j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, r := range []wf.JournalRecord{
		{Kind: wf.JournalSubmitted, Data: mustJSON(t, wf.SubmittedData{Token: d.Token, Output: json.RawMessage(out)})},
		{Kind: wf.JournalSubmissionRefused, Class: wf.Refused, Data: mustJSON(t, wf.SubmissionRefusedData{Token: d.Token, Reasons: reasons, Left: 2, Fingerprint: fp})},
	} {
		r.Run, r.Instance, r.State, r.Attempt, r.DefHash, r.At = run, last.Instance, last.State, last.Attempt, last.DefHash, h.now
		if err := h.j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
}

const oldReason, oldFP = "undecodable submission: old", "decode:old"

// An attempt an older engine left with an open refusal, unfixed until its
// lease expires and the coordinator answers died, fails as Refused with the
// refusal's reasons, spending a retry, instead of being lost for free.
func TestLeaseExpiryAfterOpenRefusalFailsRefused(t *testing.T) {
	d := lsDef("rf-lease", true)
	h := newHarnessOpts(t, d, newWorld(), lsOpts())
	run := h.run(d, nil)
	startedUnit(t, h, run, "a")
	oldOpenRefusal(t, h, run, `"x"`, oldFP, oldReason)
	h.now = h.now.Add(31 * time.Minute) // past the 30m fetched-unit lease: asked
	h.handOut(run, 1)
	if err := h.e.Heartbeat(run, wf.HeartbeatTarget{Name: "lsAsk#1"}, false); err != nil {
		t.Fatal(err)
	}
	next := h.handOut(run, 1)
	if n := len(lostRecords(t, h, run)); n != 0 {
		t.Fatalf("attempt_lost = %d, want none: the open refusal fails the attempt", n)
	}
	rd := refusedRecords(t, h, run)
	if len(rd) != 1 || !rd[0].Spent || !rd[0].Submitted || !slices.Equal(rd[0].Reasons, []string{oldReason}) || rd[0].Fingerprint != oldFP {
		t.Fatalf("refused = %+v, want the open refusal spent with its fingerprint", rd)
	}
	if len(next) != 1 || next[0].Attempt != 2 || !slices.Equal(next[0].PreviousRefusal, rd[0].Reasons) {
		t.Fatalf("after the expiry HandOut = %+v, want attempt 2 showing the refusal", next)
	}
}

// A died report or a session orphan of an attempt an older engine left with
// an open refusal fails it as Refused, spending a retry, as a lease expiry
// does.
func TestDiedOrOrphanAfterOpenRefusalFailsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(unit string) wf.HeartbeatTarget
	}{
		{"died", func(unit string) wf.HeartbeatTarget { return wf.HeartbeatTarget{Name: unit} }},
		{"orphan", func(string) wf.HeartbeatTarget { return wf.HeartbeatTarget{Session: "sess-1"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := lsDef("rf-"+tc.name, true)
			h := newHarnessOpts(t, d, newWorld(), lsOpts())
			run := h.run(d, nil)
			in, _ := startedUnit(t, h, run, "a")
			oldOpenRefusal(t, h, run, `"x"`, oldFP, oldReason)
			if err := h.e.Heartbeat(run, tc.target(in.Name), false); err != nil {
				t.Fatal(err)
			}
			if n := len(lostRecords(t, h, run)); n != 0 {
				t.Fatalf("attempt_lost = %d, want none: the open refusal fails the attempt", n)
			}
			rd := refusedRecords(t, h, run)
			if len(rd) != 1 || !rd[0].Spent || !rd[0].Submitted || rd[0].Fingerprint != oldFP {
				t.Fatalf("refused = %+v, want the open refusal spent with its fingerprint", rd)
			}
			next := h.handOut(run, 1)
			if len(next) != 1 || next[0].Attempt != 2 || !slices.Equal(next[0].PreviousRefusal, rd[0].Reasons) {
				t.Fatalf("after the loss HandOut = %+v, want attempt 2 showing the refusal", next)
			}
		})
	}
}

type (
	rbAsk    struct{}
	rbDone   struct{}
	rbGaveUp struct{}
	rbOut    struct{ V string }
)

// rbDef is one agent whose probe check refuses V "bad" and is Unavailable on
// V "busy"; with no Unavailable route, Unavailable takes the default backoff.
func rbDef() *wf.Def {
	d := wf.New("rf-backoff")
	a := wf.NewAgent[rbAsk, rbOut](d)
	ok := wf.NewTerminal[rbDone](d, wf.Success)
	gaveUp := wf.NewTerminal[rbGaveUp](d, wf.Stopped)
	a.Spec(wf.PromptText("Probe."), wf.Retry[rbAsk](wf.Const(2), a.To(gaveUp, "gave up")),
		wf.Checkf("probe", func(o rbOut, _ wf.Ctx) error {
			switch o.V {
			case "bad":
				return errors.New("bad value")
			case "busy":
				return fmt.Errorf("probe down: %w", wf.ErrUnavailable)
			}
			return nil
		}))
	next := a.To(ok, "ok")
	a.Route(func(rbOut, wf.Ctx) wf.Edge[rbAsk] { return next }, next)
	return d
}

// refusedThenBackoff leaves an older engine's open refusal on the first
// attempt, makes the next done Unavailable so
// the state backs off, and drives past the due time, so the backoff re-issues
// the instruction under a new token on the same attempt; the old unit
// retires and its replacement awaits handout.
func refusedThenBackoff(t *testing.T) (*harness, wf.RunID) {
	t.Helper()
	d := rbDef()
	h := newHarnessOpts(t, d, newWorld(), lsOpts())
	run := h.run(d, nil)
	first, _ := startedUnit(t, h, run, "a")
	oldOpenRefusal(t, h, run, `{"V":"bad"}`, "probe|old", "probe: bad value")
	if res := done(t, h, run, first.Name, `{"V":"busy"}`); res.Class != wf.Unavailable {
		t.Fatalf("unavailable check = %+v, want Unavailable", res)
	}
	if n := len(h.kind(run, wf.JournalBackoffScheduled)); n != 1 {
		t.Fatalf("backoff_scheduled = %d, want 1", n)
	}
	h.now = h.now.Add(wf.DefaultUnavailableBackoff[0] + time.Second)
	h.handOut(run, 0)
	if issued := h.kind(run, wf.JournalInstructionIssued); len(issued) != 2 {
		t.Fatalf("instruction_issued = %d, want the backoff's re-issue", len(issued))
	}
	return h, run
}

// The refusal of a submission that later backed off is not open on the
// instruction the backoff re-issues; its unclaimed replacement has no lease.
func TestBackoffClosesOpenRefusal(t *testing.T) {
	h, run := refusedThenBackoff(t)
	h.now = h.now.Add(31 * time.Minute) // past the 30m fetched-unit lease
	h.handOut(run, 0)
	if rd := refusedRecords(t, h, run); len(rd) != 0 {
		t.Fatalf("refused = %+v, want none: no refusal is open", rd)
	}
	if got := lostRecords(t, h, run); len(got) != 0 {
		t.Fatalf("attempt_lost = %+v, want none for the unclaimed replacement", got)
	}
}

// wf failed is an abandonment, not a refusal of output: the next attempt is
// shown nothing.
func TestFailedAbandonmentIsNoRefusal(t *testing.T) {
	d := clAgentDef("rf-failed")
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in, _ := startedUnit(t, h, run, "a")
	if _, err := h.e.Failed(run, in.Name, "cannot do it"); err != nil {
		t.Fatal(err)
	}
	rd := refusedRecords(t, h, run)
	if len(rd) != 1 || rd[0].Submitted {
		t.Fatalf("refused = %+v, want one abandonment not marked Submitted", rd)
	}
	second, prompt := startedUnit(t, h, run, "b")
	if second.Attempt != 2 || second.PreviousRefusal != nil || strings.Contains(prompt, "predecessor's output was refused") {
		t.Fatalf("attempt 2 = %+v, prompt:\n%s\nwant no previousRefusal and no predecessor-refused line", second, prompt)
	}
	if b, _ := json.Marshal(second); strings.Contains(string(b), "previousRefusal") {
		t.Fatalf("attempt 2 JSON names previousRefusal: %s", b)
	}
}

// Replay maps an older engine's open refusal to validate: the run that fixed
// its output on attempt 1 replays to the same end with no attempt spent.
func TestReplayOpenRefusalAsValidate(t *testing.T) {
	d := lsDef("rf-replay", false) // one attempt: a spent refusal would halt
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	in, _ := startedUnit(t, h, run, "a")
	oldOpenRefusal(t, h, run, `"x"`, oldFP, oldReason)
	if res := done(t, h, run, in.Name, `{}`); res.Class != wf.Accepted {
		t.Fatalf("fixed output = %+v", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "lsDone")
	rep, err := wf.Replay(h.records(run), d, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stop.Kind != "ended" || len(rep.Differences) != 0 {
		t.Fatalf("replay = %+v, want the same end with no difference", rep)
	}
}

// validate never changes the journal, on any path: the transcript's F2
// bookkeeping (unresolved or unread), a missing output and a migration are
// each journaled by done's path and not by validate's.
func TestValidateNeverWrites(t *testing.T) {
	unchanged := func(t *testing.T, h *harness, run wf.RunID, call func()) {
		t.Helper()
		before := len(h.records(run))
		call()
		if after := len(h.records(run)); after != before {
			t.Fatalf("validate appended %d records: %v", after-before, h.records(run)[before:])
		}
	}
	t.Run("transcripts", func(t *testing.T) {
		for _, transcript := range []string{"", filepath.Join(t.TempDir(), "agent.jsonl")} {
			d := outOfEnumDef(false)
			h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunDir: func(wf.RunID) string { return filepath.Join(t.TempDir(), "rd") }})
			run := h.run(d, nil)
			in := h.handOut(run, 1)[0]
			if transcript != "" {
				if err := os.WriteFile(transcript, []byte(`{"type":"assistant"}`+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.e.Started(run, in.Name, wf.Claim{Harness: "claude", Session: "s", Agent: "a", Transcript: transcript}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.e.(*wf.Engine).PromptFetch(run, in.Name, ""); err != nil {
				t.Fatal(err)
			}
			unchanged(t, h, run, func() {
				_, _ = h.e.Validate(run, in.Name, wf.Submission{Output: json.RawMessage(`{"Verdict":"pass"}`)})
			})
		}
	})
	t.Run("missing output", func(t *testing.T) {
		e := newCLIEnv(t)
		run := e.start("agent-cli")
		in := allocatedNext(t, e, run)
		e.must("dev", "started", string(run), in.Name, "--harness", "claude", "--session", "s", "--agent", "a", "--state-dir", e.state)
		e.must("prompt", string(run), in.Name, "--state-dir", e.state) // the body exists: F2 has bookkeeping to do
		_, before, _ := e.cli("journal", string(run), "--state-dir", e.state)
		if code, _, er := e.cli("validate", string(run), in.Name, "--state-dir", e.state); code != 5 {
			t.Fatalf("validate with no output = %d %s", code, er)
		}
		if _, after, _ := e.cli("journal", string(run), "--state-dir", e.state); after != before {
			t.Fatalf("validate with no output wrote the journal:\n%s", strings.TrimPrefix(after, before))
		}
	})
	t.Run("changed hash through the CLI", func(t *testing.T) {
		// The same states, a new prompt, a declared migration: validate's
		// output lookup must not persist the migration either.
		v1, v2 := sizedAgentDef("vmig", "Old prompt."), sizedAgentDef("vmig", "New prompt.")
		v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash()})
		state := filepath.Join(t.TempDir(), "state")
		mustSingle(t, v1, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(mustSingle(t, v1, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
			t.Fatalf("next = %v, %v", ins, err)
		}
		mustSingle(t, v1, "wf", "dev", "started", ins[0].Name, "--run-id", "r1", "--harness", "claude", "--session", "s", "--agent", "a", "--state-dir", state)
		before := mustSingle(t, v1, "wf", "journal", "--state-dir", state)
		if code, _, er := singleCLI(v2, "wf", "validate", ins[0].Name, "--run-id", "r1", "--state-dir", state); code != 5 || !strings.Contains(er, "nothing was recorded") {
			t.Fatalf("validate on the new definition = %d %s", code, er)
		}
		if after := mustSingle(t, v1, "wf", "journal", "--state-dir", state); after != before {
			t.Fatalf("validate wrote the journal:\n%s", strings.TrimPrefix(after, before))
		}
		mustSingle(t, v2, "wf", "next", "0", "--state-dir", state)
		if after := mustSingle(t, v1, "wf", "journal", "--state-dir", state); !strings.Contains(after, `"migrated"`) {
			t.Fatalf("a writing verb on the new definition did not migrate:\n%s", after)
		}
	})
	t.Run("changed hash", func(t *testing.T) {
		v1 := migDef(false)
		h := newHarnessOpts(t, v1, nil, wf.EngineOptions{RunID: "m1"})
		run := h.run(v1, nil)
		old := h.next(run)[0]
		v2 := migDef(true)
		v2.Migrations(wf.Migration{FromHash: v1.Hash(), ToHash: v2.Hash(), States: map[string]string{"mOld": "mNew"}})
		e2 := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{v2}, RunID: "m1"})
		unchanged(t, h, run, func() { _, _ = e2.Validate(run, old.Name, wf.Submission{Output: json.RawMessage(`{}`)}) })
		// done's path does migrate: the dry path was the one that mattered.
		if _, err := e2.Status(run); err != nil {
			t.Fatal(err)
		}
		if _, err := e2.Next(run, ""); err != nil || len(h.kind(run, wf.JournalMigrated)) != 1 {
			t.Fatalf("a writing verb did not migrate: %v", err)
		}
	})
}

type (
	rfvAsk  struct{}
	rfvDone struct{}
	rfvStop struct{}
)

// rvDef routes true to done and false to stop; refuse adds a check that
// refuses false.
func rvDef(refuse bool) *wf.Def {
	d := wf.New("rf-replay-verdict")
	a := wf.NewAgent[rfvAsk, bool](d)
	done := wf.NewTerminal[rfvDone](d, wf.Success)
	stop := wf.NewTerminal[rfvStop](d, wf.Stopped)
	var checks []wf.Check[bool]
	if refuse {
		checks = append(checks, wf.Checkf("true-only", func(v bool, _ wf.Ctx) error {
			if !v {
				return errors.New("false is refused")
			}
			return nil
		}))
	}
	a.Spec(wf.PromptText("p"), wf.Retry[rfvAsk](wf.Const(2), a.To(stop, "gave up")), checks...)
	yes, no := a.To(done, "yes"), a.To(stop, "no")
	a.Route(func(v bool, _ wf.Ctx) wf.Edge[rfvAsk] {
		if v {
			return yes
		}
		return no
	}, yes, no)
	return d
}

// A legacy open refusal the definition no longer makes is a difference: the
// replay reports it, takes the definition's route for that output and does
// not consume the run's correction.
func TestReplayReportsAChangedRefusal(t *testing.T) {
	v1 := rvDef(true)
	h := newHarness(t, v1, nil)
	run := h.run(v1, nil)
	in, _ := startedUnit(t, h, run, "a")
	oldOpenRefusal(t, h, run, `false`, "true-only|old", "true-only: false is refused")
	if res := done(t, h, run, in.Name, `true`); res.Class != wf.Accepted {
		t.Fatalf("correction = %+v", res)
	}
	statusOf(t, h.status(run), wf.RunEnded, "rfvDone")
	rep, err := wf.Replay(h.records(run), rvDef(false), "")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, d := range rep.Differences {
		kinds = append(kinds, d.Kind)
	}
	if !slices.Contains(kinds, "refusal") || !slices.Contains(kinds, "route") {
		t.Fatalf("differences %v, want the changed refusal and the route it takes:\n%s", kinds, rep)
	}
	if !strings.Contains(rep.String(), "refused: true-only: false is refused") || !strings.Contains(rep.String(), "-> rfvStop") {
		t.Fatalf("report does not name them:\n%s", rep)
	}
}
