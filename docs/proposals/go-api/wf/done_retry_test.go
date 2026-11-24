package wf_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestExecutorReleasesOnlyTheNamedApprovalHold(t *testing.T) {
	def := plainAgentDef("approval-library")
	h := newHarness(t, def, nil)
	run := h.run(def, nil)
	hold := func(action string) wf.HeldUnit {
		t.Helper()
		offers := h.handOut(run, 1)
		if len(offers) != 1 || offers[0].Kind != wf.InstructionAgent {
			t.Fatalf("next offer = %+v", offers)
		}
		unit := offers[0]
		if err := h.e.Started(run, unit.Name, wf.Claim{Harness: "generic", Agent: "child"}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.e.FailedAs(run, unit.Name, action, wf.FailApproval); err != nil {
			t.Fatal(err)
		}
		st := h.status(run)
		if len(st.Held) != 1 || st.Held[0].ApprovalHold <= 0 || st.Held[0].ApprovalAction != action {
			t.Fatalf("status held = %+v", st.Held)
		}
		return st.Held[0]
	}
	first := hold("first action")
	if err := h.e.ReleaseApproval(run, first.Child, first.ApprovalHold); err != nil {
		t.Fatal(err)
	}
	second := hold("second action")
	before := len(h.records(run))
	for _, seq := range []int64{first.ApprovalHold, 0} {
		if err := h.e.ReleaseApproval(run, second.Child, seq); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("release %d = %v, want ErrRejected", seq, err)
		}
	}
	if kept := h.status(run).Held; len(kept) != 1 || kept[0] != second || len(h.records(run)) != before {
		t.Fatalf("refused releases changed the current hold: %+v", kept)
	}
	if err := h.e.ReleaseApproval(run, second.Child, second.ApprovalHold); err != nil {
		t.Fatal(err)
	}
	if offers := h.handOut(run, 1); len(offers) != 1 || offers[0].Kind != wf.InstructionAgent || offers[0].Name == second.Name {
		t.Fatalf("released hold did not get a fresh offer: %+v", offers)
	}
}

func TestApprovalHoldPreservesRetriesAndClaimOwnership(t *testing.T) {
	def := plainAgentDef("approval-hold")
	d := newDrCLI(t, def)
	in := d.hand()
	claim := claimToken(t, in)
	if code, msg := d.as("kid", claim, "prompt"); code != 0 {
		t.Fatalf("prompt: exit %d, %s", code, msg)
	}
	marker := filepath.Join(d.state, "unexpected")
	action := "touch " + marker + "\n# approval required"
	cause, outcome := action+": missing prompt", "refused\nno prompt shown"
	report := func(kid, reason string) (int, string) {
		return d.as(kid, claim, "failed", "--class", "approval", "--reason", reason, "--escalated", cause+": "+outcome)
	}
	if code, msg := report("kid", " "); code != 4 || !strings.Contains(msg, "approval requires the exact command or tool call") {
		t.Fatalf("blank approval report: exit %d, %q", code, msg)
	}
	if code, msg := report("kid", action); code != 0 || !strings.Contains(msg, "needs approval") {
		t.Fatalf("approval report: exit %d, %q", code, msg)
	}
	journal := func() string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(d.state, "runs", "r1", "journal.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	before := journal()
	if code, msg := report("kid", action); code != 0 || !strings.Contains(msg, "already recorded") || journal() != before {
		t.Fatalf("identical approval report: exit %d, %q", code, msg)
	}
	// The same report with the same claim from another caller is the same
	// retry: the claim is what it is matched on.
	if code, msg := report("other", action); code != 0 || !strings.Contains(msg, "already recorded") || journal() != before {
		t.Fatalf("the same approval report from another caller: exit %d, %q", code, msg)
	}
	if code, msg := report("kid", action+" changed"); code != 4 {
		t.Fatalf("changed approval report: exit %d, %q", code, msg)
	}
	t.Setenv("CODEX_THREAD_ID", "boss")
	next := mustSingle(t, def, "wf", "next", "1", "--state-dir", d.state)
	if !strings.Contains(next, "needs approval") || !strings.Contains(next, strconv.Quote(action)) ||
		!strings.Contains(next, "wf retry") || !strings.Contains(next, "leave it parked") {
		t.Fatalf("next did not surface the exact action and recovery:\n%s", next)
	}
	status := mustSingle(t, def, "wf", "status", "--state-dir", d.state)
	if !strings.Contains(status, "needs approval") || !strings.Contains(status, strconv.Quote(action)) {
		t.Fatalf("status did not surface the exact action:\n%s", status)
	}
	if strings.Contains(status, action) || strings.Contains(status, outcome) ||
		!strings.Contains(status, strconv.Quote(cause)) || !strings.Contains(status, strconv.Quote(outcome)) {
		t.Errorf("status did not quote the escalation telemetry:\n%s", status)
	}
	var st struct {
		Phase string
		Held  []struct{ ApprovalAction string }
	}
	if status := mustSingle(t, def, "wf", "status", "--json", "--state-dir", d.state); json.Unmarshal([]byte(status), &st) != nil ||
		st.Phase != wf.RunBlocked || len(st.Held) != 1 || st.Held[0].ApprovalAction != action {
		t.Fatalf("status did not preserve the typed hold:\n%s", status)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("report ran the requested action: stat = %v", err)
	}
	retry := func(hold wf.HoldView) (int, string, string) {
		return singleCLI(def, append([]string{"wf", "retry", "--state-dir", d.state}, approvalReleaseArgs(hold)...)...)
	}
	firstHold := onlyApprovalHold(t, d)
	if code, _, er := retry(firstHold); code != 0 {
		t.Fatalf("first release: exit %d, %s", code, er)
	}
	nextUnit := d.hand()
	if nextUnit.Name == in.Name {
		t.Fatal("clearing the hold reused the old unit")
	}
	if code, msg := report("kid", action); code != 0 || !strings.Contains(msg, "already recorded") {
		t.Fatalf("identical late approval report: exit %d, %q", code, msg)
	}
	nextClaim := claimToken(t, nextUnit)
	if code, msg := d.as("retry-kid", nextClaim, "prompt"); code != 0 {
		t.Fatalf("prompt: exit %d, %q", code, msg)
	}
	const nextAction = "different action"
	if code, msg := d.as("retry-kid", nextClaim, "failed", "--class", "approval", "--reason", nextAction); code != 0 {
		t.Fatalf("second report: exit %d, %q", code, msg)
	}
	secondHold := onlyApprovalHold(t, d)
	before = journal()
	if code, _, er := retry(firstHold); code != 4 || !strings.Contains(er, "current approval hold") ||
		!strings.Contains(er, nextUnit.Name) || journal() != before {
		t.Fatalf("stale approval release: exit %d, %q", code, er)
	}
	if code, _, er := singleCLI(def, "wf", "retry", "--state-dir", d.state); code != 4 || journal() != before {
		t.Fatalf("unscoped approval release: exit %d, %q", code, er)
	}
	if code, _, er := retry(secondHold); code != 0 {
		t.Fatalf("second release: exit %d, %s", code, er)
	}
	d.fail("after-approval", d.hand())
	d.finish("last-kid", d.hand(), `{"OK":true}`)
}

func approvalReleaseArgs(hold wf.HoldView) []string {
	if _, rest, ok := strings.Cut(hold.Fix, " --approval-hold "); ok {
		return []string{"--approval-hold", strings.Fields(rest)[0]}
	}
	return nil
}

func onlyApprovalHold(t *testing.T, d *drCLI) wf.HoldView {
	t.Helper()
	t.Setenv("CODEX_THREAD_ID", "boss")
	var ins []wf.Instruction
	out := mustSingle(t, d.def, "wf", "next", "1", "--json", "--state-dir", d.state)
	if json.Unmarshal([]byte(out), &ins) != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionWaiting || len(ins[0].Holds) != 1 {
		t.Fatalf("next did not report one hold: %s", out)
	}
	return ins[0].Holds[0]
}

func TestApprovalHoldStaysScopedToItsUnit(t *testing.T) {
	def := fanDef("approval-siblings", chItems(1, 2), 2, wf.AllSettled())
	d := newDrCLI(t, def)
	parked := d.hand()
	claim := claimToken(t, parked)
	if code, msg := d.as("parked", claim, "prompt"); code != 0 {
		t.Fatalf("prompt: exit %d, %q", code, msg)
	}
	const action = "tool_call(input)"
	if code, msg := d.as("parked", claim, "failed", "--class", "approval", "--reason", action); code != 0 {
		t.Fatalf("approval report: exit %d, %q", code, msg)
	}
	sibling := d.hand()
	if sibling.Child == parked.Child {
		t.Fatal("next offered the held child instead of its sibling")
	}
	d.finish("sibling", sibling, `{"N":2}`)
	hold := onlyApprovalHold(t, d)
	if hold.Child != parked.Child || hold.ApprovalAction != action || !strings.Contains(hold.Fix, "--child") {
		t.Fatalf("next hold = %+v, want the exact action and a retry scoped to %s", hold, parked.Child)
	}
	mustSingle(t, def, append([]string{"wf", "retry", "--child", hold.Child, "--state-dir", d.state}, approvalReleaseArgs(hold)...)...)
	if got := d.hand(); got.Child != parked.Child || got.Name == parked.Name {
		t.Fatalf("retry offered %+v, want a fresh unit for %s", got, parked.Child)
	}
}

// A done whose tool call timed out after the engine accepted it is run again
// with the same claim (FANOUT-2 run B: RoastDedupe#1, CoverageDisposition#6).
// The retry says the unit is finished, exit 0; it is not refused as a claim
// the child never had.
func TestDoneRetryAfterAcceptanceSaysFinished(t *testing.T) {
	strictCallers(t)
	def := fanDef("done-retry", chItems(1, 2), 2, wf.AllSettled())
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("next: %v %v", ins, err)
	}
	a := agentsOf(ins)[0]
	writeCodexChild(t, os.Getenv("CODEX_HOME"), "boss", "kid-a", claimToken(t, a))
	t.Setenv("CODEX_THREAD_ID", "kid-a")
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, a), "--state-dir", state)
	out := writeAllocated(t, a.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", "--claim", claimToken(t, a), "--output", out, "--state-dir", state)
	for _, verb := range []string{"done", "validate"} {
		code, stdout, er := singleCLI(def, "wf", verb, "--claim", claimToken(t, a), "--output", out, "--state-dir", state)
		if code != 0 || !strings.Contains(stdout+er, "already accepted") {
			t.Errorf("%s again after acceptance: exit %d, %q %q", verb, code, stdout, er)
		}
	}
	// A claim that was never accepted is still refused.
	if code, _, _ := singleCLI(def, "wf", "done", "--claim", "nosuchclaim", "--output", out, "--state-dir", state); code != 4 {
		t.Errorf("unknown claim: exit %d", code)
	}
}

type (
	drA    struct{}
	drB    struct{}
	drDone struct{}
	drStop struct{}
	drOut  struct{ Loop bool }
)

// drCLI drives def from a Codex coordinator "boss" and its children: hand
// returns the next unit handed out, as kids returns a child's verb's exit.
type drCLI struct {
	t     *testing.T
	def   *wf.Def
	state string
}

func newDrCLI(t *testing.T, def *wf.Def) *drCLI {
	strictCallers(t)
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_SESSION_ID", "boss")
	t.Setenv("CODEX_THREAD_ID", "boss")
	d := &drCLI{t: t, def: def, state: filepath.Join(t.TempDir(), "state")}
	mustSingle(t, def, "run", "--state-dir", d.state, "--workspace", t.TempDir(), "--run-id", "r1")
	return d
}

func (d *drCLI) hand() wf.Instruction {
	d.t.Helper()
	d.t.Setenv("CODEX_THREAD_ID", "boss")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(d.t, d.def, "wf", "next", "1", "--json", "--state-dir", d.state)), &ins); err != nil || len(agentsOf(ins)) != 1 {
		d.t.Fatalf("next: %v %v", ins, err)
	}
	return agentsOf(ins)[0]
}

// as runs a unit verb from child kid, whose rollout names claim.
func (d *drCLI) as(kid, claim string, args ...string) (int, string) {
	d.t.Helper()
	writeCodexChild(d.t, os.Getenv("CODEX_HOME"), "boss", kid, claim)
	d.t.Setenv("CODEX_THREAD_ID", kid)
	code, out, er := singleCLI(d.def, append(append([]string{"wf"}, args...), "--claim", claim, "--state-dir", d.state)...)
	return code, out + er
}

func (d *drCLI) finish(kid string, in wf.Instruction, output string) {
	d.t.Helper()
	claim := claimToken(d.t, in)
	if code, out := d.as(kid, claim, "prompt"); code != 0 {
		d.t.Fatalf("prompt %s: %d %s", in.Name, code, out)
	}
	out := writeAllocated(d.t, in.Output, output)
	if code, msg := d.as(kid, claim, "done", "--output", out); code != 0 {
		d.t.Fatalf("done %s: %d %s", in.Name, code, msg)
	}
}

// recover answers the recovery offer of the unit named name, as the
// coordinator.
func (d *drCLI) recover(name, answer string) {
	d.t.Helper()
	d.t.Setenv("CODEX_THREAD_ID", "boss")
	mustSingle(d.t, d.def, "wf", "recover", name, answer, "--state-dir", d.state)
}

func (d *drCLI) fail(kid string, in wf.Instruction) {
	d.t.Helper()
	claim := claimToken(d.t, in)
	if code, out := d.as(kid, claim, "prompt"); code != 0 {
		d.t.Fatalf("prompt %s: %d %s", in.Name, code, out)
	}
	if code, out := d.as(kid, claim, "failed", "--reason", "x", "--class", "transient"); code != 0 {
		d.t.Fatalf("failed %s: %d %s", in.Name, code, out)
	}
}

// A failed attempt's claim is not finished because a later unit of the same
// instance, on the failure route, was accepted on its own attempt 1.
func TestDoneRetryOfAFailedClaimIsRefused(t *testing.T) {
	def := wf.New("done-retry-route")
	a := wf.NewAgent[drA, drOut](def)
	b := wf.NewAgent[drB, drOut](def)
	done := wf.NewTerminal[drDone](def, wf.Success)
	stop := wf.NewTerminal[drStop](def, wf.Stopped)
	def.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("a"), wf.Retry[drA](wf.Const(1), a.To(b, "a gave up: b")))
	aDone := a.To(done, "ok")
	a.Route(func(drOut, wf.Ctx) wf.Edge[drA] { return aDone }, aDone)
	b.Spec(wf.PromptText("b"), wf.Retry[drB](wf.Const(1), b.To(stop, "gave up")))
	bDone := b.To(done, "ok")
	b.Route(func(drOut, wf.Ctx) wf.Edge[drB] { return bDone }, bDone)
	d := newDrCLI(t, def)
	unitA := d.hand()
	d.fail("kid-a", unitA)
	d.recover(unitA.Name, "give-up")
	unitB := d.hand()
	d.finish("kid-b", unitB, `{"Loop":false}`)
	out := writeAllocated(t, unitA.Output, `{"Loop":false}`)
	if code, msg := d.as("kid-a", claimToken(t, unitA), "done", "--output", out); code != 4 || strings.Contains(msg, "already accepted") {
		t.Errorf("done of a's failed claim after b was accepted: exit %d, %q", code, msg)
	}
	if code, msg := d.as("kid-b", claimToken(t, unitB), "done", "--output", writeAllocated(t, unitB.Output, `{"Loop":false}`)); code != 0 || !strings.Contains(msg, "already accepted") {
		t.Errorf("b's own retry: exit %d, %q", code, msg)
	}
}

// On a loop, each visit's accepted claim is finished and a failed attempt's
// is not, whatever their attempt numbers.
func TestDoneRetryAcrossALoopRevisit(t *testing.T) {
	def := wf.New("done-retry-loop")
	a := wf.NewAgent[drA, drOut](def)
	b := wf.NewAgent[drB, drOut](def)
	done := wf.NewTerminal[drDone](def, wf.Success)
	stop := wf.NewTerminal[drStop](def, wf.Stopped)
	def.OnFailure(wf.Blocked, stop)
	a.Spec(wf.PromptText("a"), wf.Retry[drA](wf.Const(2), a.To(stop, "gave up")))
	toB, fin := a.To(b, "again"), a.To(done, "ok")
	a.Route(func(o drOut, _ wf.Ctx) wf.Edge[drA] {
		if o.Loop {
			return toB
		}
		return fin
	}, toB, fin)
	b.Spec(wf.PromptText("b"), wf.Retry[drB](wf.Const(1), b.To(stop, "gave up")))
	back := b.To(a, "back")
	b.Route(func(drOut, wf.Ctx) wf.Edge[drB] { return back }, back)
	d := newDrCLI(t, def)
	failed := d.hand()
	d.fail("kid-1", failed) // attempt 2 follows by itself
	first := d.hand()
	d.finish("kid-2", first, `{"Loop":true}`)
	d.finish("kid-b", d.hand(), `{"Loop":false}`)
	second := d.hand()
	d.finish("kid-3", second, `{"Loop":false}`)
	for _, c := range []struct {
		kid  string
		unit wf.Instruction
		ok   bool
	}{{"kid-1", failed, false}, {"kid-2", first, true}, {"kid-3", second, true}} {
		out := writeAllocated(t, c.unit.Output, `{"Loop":false}`)
		code, msg := d.as(c.kid, claimToken(t, c.unit), "done", "--output", out)
		if accepted := code == 0 && strings.Contains(msg, "already accepted"); accepted != c.ok {
			t.Errorf("%s retry of %s: exit %d, %q; want finished=%v", c.kid, c.unit.Name, code, msg, c.ok)
		}
	}
}

// A re-offered unit has two claims; only the one that bound it is told the
// unit finished, whichever caller presents it. The second claim is the
// fresh offer made once the unstarted grace passed: a whole wait inside the
// grace repeats the first offer under its own claim.
func TestDoneRetryOnlyForTheBindingClaim(t *testing.T) {
	defer wf.SetUnstartedGrace(50 * time.Millisecond)()
	def := fanDef("done-retry-reoffer", chItems(1), 1, wf.AllSettled())
	d := newDrCLI(t, def)
	first := d.hand()
	t.Setenv("CODEX_THREAD_ID", "boss")
	time.Sleep(60 * time.Millisecond) // past the grace
	_, text, er := singleCLI(def, "wf", "next", "--timeout", "100ms", "--state-dir", d.state)
	_, rest, ok := strings.Cut(text, " --claim ")
	if !ok || !strings.Contains(text, "not started yet: ") {
		t.Fatalf("no re-offer: %q %q", text, er)
	}
	second := strings.Fields(rest)[0]
	if second == claimToken(t, first) {
		t.Fatal("re-offer kept the claim")
	}
	winner := first
	winner.Brief = strings.Replace(first.Brief, claimToken(t, first), second, 1)
	d.finish("kid-a", winner, `{"OK":true}`)
	out := writeAllocated(t, first.Output, `{"OK":true}`)
	if code, msg := d.as("kid-a", claimToken(t, first), "done", "--output", out); code != 4 || strings.Contains(msg, "already accepted") {
		t.Errorf("the losing claim: exit %d, %q", code, msg)
	}
	if code, msg := d.as("kid-x", second, "done", "--output", out); code != 0 || !strings.Contains(msg, "already accepted") {
		t.Errorf("another caller's retry with the winning claim: exit %d, %q", code, msg)
	}
	if code, msg := d.as("kid-a", second, "done", "--output", out); code != 0 || !strings.Contains(msg, "already accepted") {
		t.Errorf("the owner's retry: exit %d, %q", code, msg)
	}
}

// A failed whose exec tool yielded while it ran is issued again, identically,
// by the same child after the first was journaled (the regression case seq 3188-3210).
// The repeat says the failure is already recorded, exit 0, with no stop
// tail, and appends nothing, whichever caller holds the claim; a different
// reason or class is still refused. Each call is its own CLI over the state directory and the
// first call's reply is never read, so this is also the repeat after a crash
// between the record and the reply.
func TestFailedRepeatAfterRecordSaysRecorded(t *testing.T) {
	for _, c := range []struct {
		name    string
		retries int
	}{{"exhausted", 1}, {"retried", 2}} {
		t.Run(c.name, func(t *testing.T) {
			def := wf.New("failed-repeat-" + c.name)
			a := wf.NewAgent[drA, drOut](def)
			b := wf.NewAgent[drB, drOut](def)
			done := wf.NewTerminal[drDone](def, wf.Success)
			stop := wf.NewTerminal[drStop](def, wf.Stopped)
			def.OnFailure(wf.Blocked, stop)
			a.Spec(wf.PromptText("a"), wf.Retry[drA](wf.Const(c.retries), a.To(b, "a gave up: b")))
			aDone := a.To(done, "ok")
			a.Route(func(drOut, wf.Ctx) wf.Edge[drA] { return aDone }, aDone)
			b.Spec(wf.PromptText("b"), wf.Retry[drB](wf.Const(1), b.To(stop, "gave up")))
			bDone := b.To(done, "ok")
			b.Route(func(drOut, wf.Ctx) wf.Edge[drB] { return bDone }, bDone)
			d := newDrCLI(t, def)
			unit := d.hand()
			claim := claimToken(t, unit)
			d.fail("kid-a", unit)
			journal := filepath.Join(d.state, "runs", "r1", "journal.jsonl")
			before, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			// Only the child's own failed is marked; the fold counts nothing else.
			if !strings.Contains(string(before), `"ChildFailed":true`) {
				t.Fatal("the child's failed was not journaled as ChildFailed")
			}
			code, msg := d.as("kid-a", claim, "failed", "--reason", "x", "--class", "transient")
			if code != 0 || !strings.Contains(msg, "already recorded") || strings.Contains(msg, "stop") {
				t.Errorf("identical repeat: exit %d, %q", code, msg)
			}
			if code, msg := d.as("kid-a", claim, "failed", "--reason", "x"); code != 0 || !strings.Contains(msg, "already recorded") {
				t.Errorf("repeat with the default class: exit %d, %q", code, msg)
			}
			if after, _ := os.ReadFile(journal); len(after) != len(before) {
				t.Errorf("an identical repeat appended %d journal bytes", len(after)-len(before))
			}
			for _, r := range []struct {
				kid  string
				args []string
			}{
				{"kid-a", []string{"--reason", "y"}},
				{"kid-a", []string{"--reason", "x", "--class", "blocked"}},
				{"kid-x", []string{"--reason", "y"}},
			} {
				if code, msg := d.as(r.kid, claim, append([]string{"failed"}, r.args...)...); code != 4 ||
					strings.Contains(msg, "already recorded") ||
					strings.Contains(msg, "stop and report this to the person who started the run") ||
					!strings.Contains(msg, "end only your child task") {
					t.Errorf("%s failed %v after the record: exit %d, %q", r.kid, r.args, code, msg)
				}
			}
			if code, msg := d.as("kid-x", claim, "failed", "--reason", "x"); code != 0 || !strings.Contains(msg, "already recorded") {
				t.Errorf("the identical repeat from another caller: exit %d, %q", code, msg)
			}
			// The coordinator was never told to stop: its next still runs.
			t.Setenv("CODEX_THREAD_ID", "boss")
			if code, out, er := singleCLI(def, "wf", "next", "--timeout", "100ms", "--state-dir", d.state); code != 0 {
				t.Errorf("coordinator next after the repeats: exit %d, %q %q", code, out, er)
			}
		})
	}
}
