package wf_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// an earlier revision: an effect whose intent has no result is the coordinator's to
// settle: it checks the world, and the settle record keeps what it checked.
// Only an effect the definition marks outside the run (wf.Outside: a push, a
// merge to base) waits for a person.

func uncertainRun(t *testing.T, e wf.ExternalEffect) (*crashHarness, wf.RunID, int64) {
	t.Helper()
	d, _ := effectDef(nil, e)
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, nil)
	c.crashAt(run, `{}`)
	return c, run, c.kind(run, wf.JournalEffectIntent)[0].Seq
}

func renderNext(ins []wf.Instruction) string {
	return wf.RenderNext(wf.NextResult{Instructions: ins}, "claude", func(verb string, args ...string) string {
		return strings.Join(append([]string{"workflow", verb}, args...), " ")
	})
}

func TestTheCoordinatorSettlesAnUncertainEffect(t *testing.T) {
	c, run, intent := uncertainRun(t, mergeEffect(wf.NonIdempotent("writes a ledger")))
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Effect != intent {
		t.Fatalf("instructions = %+v, want a manual verification of effect %d", ins, intent)
	}
	in := ins[0]
	if in.Person {
		t.Errorf("instruction = %+v: an effect inside the run is not a person's to settle", in)
	}
	for _, want := range []string{"check", "--evidence"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("prompt lacks %q: it must say what to check and how to record it:\n%s", want, in.Prompt)
		}
	}
	if !strings.Contains(in.Submit, "--evidence") {
		t.Errorf("Submit = %q, want the settle line to carry --evidence", in.Submit)
	}
	text := renderNext(ins)
	if strings.Contains(text, "needs a person") || strings.Contains(text, "needs a human") {
		t.Errorf("next sends the coordinator to a person for an effect inside the run:\n%s", text)
	}
	if !strings.Contains(text, "Yours to settle") {
		t.Errorf("next does not hand the settlement to the coordinator:\n%s", text)
	}
	if st := c.status(run); strings.Contains(st.NothingReadyReason, "human") {
		t.Errorf("status reason = %q: nothing waits on a person", st.NothingReadyReason)
	}

	const why = "ledger.md absent under records; git status clean: the command did not run"
	if err := c.e.(*wf.Engine).SettleAs(run, intent, false, nil, why, coordinatorOp); err != nil {
		t.Fatal(err)
	}
	settled := c.kind(run, wf.JournalEffectSettled)
	var d wf.EffectSettledData
	if len(settled) != 1 || json.Unmarshal(settled[0].Data, &d) != nil || d.Applied || d.Evidence != why {
		t.Fatalf("effect_settled = %+v, want one not-applied settlement with the evidence %q", settled, why)
	}
}

func TestAnEffectOutsideTheRunKeepsThePersonGate(t *testing.T) {
	c, run, _ := uncertainRun(t, wf.Outside(mergeEffect(wf.NonIdempotent("pushes upstream"))))
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || !ins[0].Person {
		t.Fatalf("instructions = %+v, want a manual verification for a person", ins)
	}
	if text := renderNext(ins); !strings.Contains(text, "needs a person") {
		t.Errorf("next does not send an outside effect to a person:\n%s", text)
	}
	if st := c.status(run); !strings.HasPrefix(st.NothingReadyReason, "waiting on human: verify effect") {
		t.Errorf("status reason = %q, want it waiting on a person", st.NothingReadyReason)
	}
}

// The marker is part of the definition: render shows it, so it is hashed.
func TestOutsideIsInTheDefinition(t *testing.T) {
	plain, _ := effectDef(nil, mergeEffect(wf.KeyOf("merge", "feat")))
	outside, _ := effectDef(nil, wf.Outside(mergeEffect(wf.KeyOf("merge", "feat"))))
	if plain.Hash() == outside.Hash() {
		t.Fatal("marking an effect outside the run does not change the definition hash")
	}
	if !strings.Contains(outside.Render(), "outside the run") {
		t.Errorf("render does not show the outside marker:\n%s", outside.Render())
	}
}

// coordinatorOp is a coordinator session settling an effect.
var coordinatorOp = wf.Operator{Name: "coordinator", Reason: "uncertain effect", Harness: "claude", Session: "coord-1"}

// bindCoordinator makes coord-1 (claude) the run's coordinator session.
func bindCoordinator(t *testing.T, c *crashHarness, run wf.RunID) {
	t.Helper()
	if _, err := c.e.(*wf.Engine).NextWait(run, 1, wf.Coordinator{Harness: "claude", Session: "coord-1"}, 0); err != nil {
		t.Fatal(err)
	}
}

// an earlier revision review round 1: an effect outside the run is settled by an
// operator that is not one of the run's coordinator sessions, in the engine
// itself, not only in what next prints.
func TestACoordinatorDoesNotSettleAnEffectOutsideTheRun(t *testing.T) {
	c, run, intent := uncertainRun(t, wf.Outside(mergeEffect(wf.NonIdempotent("merges to base"))))
	bindCoordinator(t, c, run)
	e := c.e.(*wf.Engine)
	if err := e.SettleAs(run, intent, true, nil, "merge commit on base", coordinatorOp); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "outside the run") {
		t.Fatalf("the coordinator settling an outside effect = %v, want refused", err)
	}
	if len(c.kind(run, wf.JournalEffectSettled)) != 0 {
		t.Fatal("the refused settle journaled a settlement")
	}
	person := wf.Operator{Name: "alex", Reason: "checked base", Harness: "generic"}
	if err := e.SettleAs(run, intent, true, nil, "merge commit on base", person); err != nil {
		t.Fatalf("an operator settling it: %v", err)
	}
	var sd wf.EffectSettledData
	if err := json.Unmarshal(c.kind(run, wf.JournalEffectSettled)[0].Data, &sd); err != nil || sd.Operator != "alex" || sd.Harness != "generic" || sd.Evidence != "merge commit on base" {
		t.Fatalf("effect_settled = %+v, want the operator and the evidence", sd)
	}
}

// An intent journaled before its effect was marked outside the run (no
// Outside flag) is resolved against the definition the run is on now.
func TestALegacyIntentTakesTheCurrentOutsideDeclaration(t *testing.T) {
	c, run, intent := uncertainRun(t, mergeEffect(wf.NonIdempotent("merges to base")))
	marked, _ := effectDef(nil, wf.Outside(mergeEffect(wf.NonIdempotent("merges to base"))))
	c.e = wf.NewEngine(c.j, c.reg, wf.EngineOptions{Defs: []*wf.Def{marked}, Adopt: true, Clock: func() time.Time { return c.now }})
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Effect != intent || !ins[0].Person {
		t.Fatalf("instructions = %+v, want the person's manual verification of effect %d", ins, intent)
	}
	bindCoordinator(t, c, run)
	if err := c.e.(*wf.Engine).SettleAs(run, intent, true, nil, "merged", coordinatorOp); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the coordinator settling a legacy outside intent = %v, want refused", err)
	}
}

// Evidence is required of every SettleAs; a transient check is not settled,
// and evidence that decides nothing leaves the effect open.
func TestSettleNeedsEvidenceAndTransientIsLeftAlone(t *testing.T) {
	c, run, intent := uncertainRun(t, mergeEffect(wf.NonIdempotent("writes a ledger")))
	if err := c.e.(*wf.Engine).SettleAs(run, intent, false, nil, " ", coordinatorOp); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--evidence") {
		t.Fatalf("settle with blank evidence = %v, want refused naming --evidence", err)
	}
	in := c.next(run)[0]
	text := renderNext([]wf.Instruction{in})
	if !strings.Contains(text, "leave it unsettled") {
		t.Errorf("next gives no way to leave an undecided effect open:\n%s", text)
	}
	in.Transient = true
	text = renderNext([]wf.Instruction{in})
	if strings.Contains(text, "Yours to settle") || !strings.Contains(text, "run next again") {
		t.Errorf("a transient check is rendered as one to settle:\n%s", text)
	}
}

// an earlier revision post-cap (1): the Executor's Settle and SettleRun go through the
// same checks as SettleAs. They name no operator and no evidence, so they
// settle nothing: not an effect outside the run, and not an anonymous
// settlement of any other.
func TestTheAnonymousSettleWritesNothing(t *testing.T) {
	c, run, intent := uncertainRun(t, wf.Outside(mergeEffect(wf.NonIdempotent("merges to base"))))
	bindCoordinator(t, c, run)
	if err := c.e.Settle(run, intent, true); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("Settle of an outside effect = %v, want refused", err)
	}
	local, lrun, lintent := uncertainRun(t, mergeEffect(wf.NonIdempotent("writes a ledger")))
	if err := local.e.Settle(lrun, lintent, false); !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "--evidence") {
		t.Fatalf("an anonymous Settle = %v, want refused naming --evidence", err)
	}
	if len(c.kind(run, wf.JournalEffectSettled))+len(local.kind(lrun, wf.JournalEffectSettled)) != 0 {
		t.Fatal("a refused settle journaled a settlement")
	}
}

// an earlier revision post-cap (2): a legacy intent whose edge, on the definition the run
// is on now, no longer declares a matching effect is a person's.
func TestALegacyIntentWithNoMatchingDeclarationIsAPersons(t *testing.T) {
	c, run, intent := uncertainRun(t, mergeEffect(wf.NonIdempotent("merges to base")))
	other, _ := effectDef(nil, wf.ExternalEffect{Adapter: "fs", Op: "write", Args: map[string]any{"path": "x"}, Key: wf.KeyOf("w")})
	c.e = wf.NewEngine(c.j, c.reg, wf.EngineOptions{Defs: []*wf.Def{other}, Adopt: true, Clock: func() time.Time { return c.now }})
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Effect != intent || !ins[0].Person {
		t.Fatalf("instructions = %+v, want the person's manual verification", ins)
	}
	bindCoordinator(t, c, run)
	if err := c.e.(*wf.Engine).SettleAs(run, intent, true, nil, "merged", coordinatorOp); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("the coordinator settling it = %v, want refused", err)
	}
}

// an earlier revision post-cap (3): a local effect whose key is a template (the plan
// holds the template, the open intent the bound key) is the coordinator's.
func TestALocalTemplatedEffectIsTheCoordinators(t *testing.T) {
	d, _ := effectDef(nil, wf.ExternalEffect{Adapter: "git", Op: "merge", Args: map[string]any{"branch": "feat"}, Key: wf.KeyOf("local", "{topic}")})
	d.Param("topic")
	c := newCrashHarness(t, d, newWorld(), wf.JournalEffectResult)
	run := c.run(d, map[string]string{"topic": "demo"})
	c.crashAt(run, `{}`)
	intent := c.kind(run, wf.JournalEffectIntent)[0].Seq
	ins := c.next(run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Effect != intent || ins[0].Person {
		t.Fatalf("instructions = %+v, want the coordinator's manual verification", ins)
	}
	bindCoordinator(t, c, run)
	if err := c.e.(*wf.Engine).SettleAs(run, intent, true, nil, "merge commit present", coordinatorOp); err != nil {
		t.Fatalf("the coordinator settling a local effect: %v", err)
	}
}

// testSettler settles in the tests that settle an effect for the world they
// script: an operator, so an effect outside the run is its to settle too.
var testSettler = wf.Operator{Name: "test", Reason: "the test's scripted world", Harness: "generic"}

// settleAs is SettleAs by testSettler, with evidence.
func settleAs(e wf.Executor, run wf.RunID, effect int64, applied bool) error {
	return e.(*wf.Engine).SettleAs(run, effect, applied, nil, "checked in the test world", testSettler)
}

// settleRunAs is SettleAs of a command as applied with what it printed.
func settleRunAs(e wf.Executor, run wf.RunID, effect int64, r wf.RunResult) error {
	return e.(*wf.Engine).SettleAs(run, effect, true, &r, "checked in the test world", testSettler)
}
