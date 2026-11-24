package wf_test

import (
	"encoding/json"
	"errors"
	"fmt"
	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type untilWork struct{}
type untilAfter struct{}
type untilDone struct{}

func untilDef(calls *int) *wf.Def {
	d := wf.New("until-unit")
	work := wf.NewAgent[untilWork, bool](d)
	after := wf.NewCompute[untilAfter, bool](d, func(wf.Ctx) (bool, error) { *calls++; return true, nil })
	done := wf.NewTerminal[untilDone](d, wf.Success)
	edge := work.To(after, "accepted")
	work.Spec(wf.PromptText("work"), wf.Retry[untilWork](wf.Const(2), work.To(done, "gave up")))
	work.Route(func(bool, wf.Ctx) wf.Edge[untilWork] { return edge }, edge)
	finish := after.To(done, "done")
	after.Route(func(bool, wf.Ctx) wf.Edge[untilAfter] { return finish }, finish)
	return d
}

func TestUntilUnitPersistsAndStopsBeforeSuccessor(t *testing.T) {
	calls := 0
	d := untilDef(&calls)
	h := newHarness(t, d, nil)
	h.j = wf.FileJournal(t.TempDir())
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}})
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnitName(run, "untilWork#1"); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("offer: %v %v", ins, err)
	}
	if _, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`true`)}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("successor ran inside Submit")
	}
	fresh := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{d}}).(*wf.Engine)
	if err := fresh.Stop(run, "test"); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Resume(run); err != nil {
		t.Fatal(err)
	}
	for _, get := range []func() ([]wf.Instruction, error){func() ([]wf.Instruction, error) { return fresh.Next(run, "") }, func() ([]wf.Instruction, error) { return fresh.HandOutAs(run, -1, wf.Coordinator{}) }} {
		got, err := get()
		if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionStopped {
			t.Fatalf("boundary disappeared: %+v %v", got, err)
		}
	}
	status, err := fresh.UntilUnitStatus(run)
	if err != nil || !status.Settled || status.Class != wf.Accepted {
		t.Fatalf("status: %+v %v", status, err)
	}
	if calls != 0 {
		t.Fatal("fresh engine ran successor")
	}
	if err := fresh.BindUntilUnit(run, wf.UntilUnitTarget{State: "untilAfter"}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("rebind allowed: %v", err)
	}
}

func TestUntilUnitFiltersParallelOffers(t *testing.T) {
	d := fanDef("until-fan", chItems(1, 2, 3), 3, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	h.handOut(run, 0)
	var target wf.UntilUnitTarget
	for _, in := range h.status(run).Instances {
		if in.Phase == "issued" {
			target = wf.UntilUnitTarget{Instance: in.ID, State: in.State}
			break
		}
	}
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnit(run, target); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.HandOutAs(run, -1, wf.Coordinator{Session: "coordinator"})
	if err != nil || len(ins) != 1 || ins[0].Child != target.Instance {
		t.Fatalf("parallel work escaped: %+v %v target=%+v", ins, err, target)
	}
	if _, err := engine.PromptClaim(run, "", claimToken(t, ins[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	if len(h.kind(run, wf.JournalHandedOut)) != 1 {
		t.Fatal("other units handed out")
	}
}

func TestUntilUnitRefusedAttemptDoesNotRetry(t *testing.T) {
	calls := 0
	d := untilDef(&calls)
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{State: "missing"}); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("invalid target accepted: %v", err)
	}
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{State: "untilWork"}); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{"not":"bool"}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := engine.Next(run, "")
	if err != nil || len(got) != 1 || got[0].Kind != wf.InstructionStopped || !strings.HasPrefix(got[0].Counts, "subagents ") {
		t.Fatalf("refusal retried: %+v %v", got, err)
	}
	if len(h.kind(run, wf.JournalInstructionIssued)) != 1 {
		t.Fatal("retry was issued")
	}
}

func TestUntilUnitStopsBeforeEdgeEffects(t *testing.T) {
	world := newWorld()
	d, _ := effectDef(nil, mergeEffect(wf.KeyOf("until", "effect")))
	h := newHarness(t, d, world)
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{State: "eA"}); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if world.appliedCount() != 0 || len(h.kind(run, wf.JournalEffectIntent)) != 0 || len(h.kind(run, wf.JournalTransition)) != 0 {
		t.Fatal("target's edge effects ran past boundary")
	}
}

func TestUntilUnitRejectsUnrelatedLivePrefix(t *testing.T) {
	d := fanDef("until-live", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	offered := h.handOut(run, 1)
	for _, in := range h.status(run).Instances {
		if in.Phase == "issued" && in.ID != offered[0].Child {
			err := h.e.(*wf.Engine).BindUntilUnit(run, wf.UntilUnitTarget{Instance: in.ID, State: in.State})
			if !errors.Is(err, wf.ErrRejected) {
				t.Fatalf("unrelated live prefix accepted: %v", err)
			}
			return
		}
	}
	t.Fatal("missing unoffered sibling")
}

func TestUntilUnitUnavailableBackoffSettlesBoundary(t *testing.T) {
	d := engBackoffDef()
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{State: "bWork"}); err != nil {
		t.Fatal(err)
	}
	ins, err := engine.Next(run, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`{"Down":true}`)}); err != nil {
		t.Fatal(err)
	}
	status, err := engine.UntilUnitStatus(run)
	if err != nil || !status.Settled || status.Class != wf.Unavailable {
		t.Fatalf("backoff did not settle target: %+v %v", status, err)
	}
	h.now = h.now.Add(time.Hour)
	stopped, err := engine.Next(run, "")
	if err != nil || len(stopped) != 1 || stopped[0].Kind != wf.InstructionStopped {
		t.Fatalf("backoff retried: %+v %v", stopped, err)
	}
}

func TestUntilUnitDetectsChildEndedWithoutDone(t *testing.T) {
	d := clAgentDef("until-ended")
	h := newHarness(t, d, nil)
	h.manualRecovery = true
	root := t.TempDir()
	run := h.run(d, nil)
	engine := h.e.(*wf.Engine)
	if err := engine.BindUntilUnit(run, wf.UntilUnitTarget{State: "clAsk"}); err != nil {
		t.Fatal(err)
	}
	ins := handOutAsH(t, h, run, 1, "sess", "claude")
	token := claimToken(t, ins[0])
	fetchAs(t, h, run, ins[0], hn.Caller{Harness: "claude", Session: "sess", Root: root, FromEnv: true})
	ended, err := os.ReadFile(filepath.Join("..", "harness", "testdata", "ended", "claude-handback.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "projects", "-repo", "sess", "subagents")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	first := `{"type":"user","message":{"role":"user","content":"wf prompt --claim ` + token + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), append([]byte(first), ended...), 0600); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(10 * time.Second)
	stopped, err := engine.HandOutAs(run, 1, wf.Coordinator{Harness: "claude", Session: "sess"})
	if err != nil || len(stopped) != 1 || stopped[0].Kind != wf.InstructionStopped {
		t.Fatalf("ended child was not stopped: %+v %v", stopped, err)
	}
	if len(h.kind(run, wf.JournalRecoveryOffered)) != 1 {
		t.Fatal("missing durable ended-child evidence")
	}
	if len(h.kind(run, wf.JournalInstructionIssued)) != 1 {
		t.Fatal("ended child was reissued")
	}
}

type (
	ucStart struct{}
	ucRun   struct{}
	ucWork  struct{}
	ucOK    struct{}
	ucFail  struct{}
)

// cmdOrWorkChild runs a detached command for item 1 and an agent for the
// others.
func cmdOrWorkChild() wf.Child[chItem, chRes] {
	d := wf.New("uc-item")
	exits := wf.NewEnum(d, "uc-exit", dExit(0), dExit(1), dExit(124), dExit(125))
	start := wf.NewCompute[ucStart, int](d, func(c wf.Ctx) (int, error) { return wf.InputOf[chItem](c).N, nil })
	cmd := wf.NewCommand[ucRun](d, "make", "build")
	work := wf.NewAgent[ucWork, chOut](d)
	ok := wf.NewTerminal[ucOK](d, wf.Success)
	fail := wf.NewTerminal[ucFail](d, wf.Stopped)
	toCmd, toWork := start.To(cmd, "command"), start.To(work, "agent")
	start.Route(func(n int, _ wf.Ctx) wf.Edge[ucStart] {
		if n == 1 {
			return toCmd
		}
		return toWork
	}, toCmd, toWork)
	cmd.Idempotent().Detached()
	cmd.RouteCases(wf.Cases[ucRun, wf.RunResult](wf.ExitAs[dExit], exits,
		wf.On(dExit(0), cmd.To(ok, "passes")),
		wf.On(dExit(1), cmd.To(fail, "fails")),
		wf.On(dExit(124), cmd.To(fail, "timed out")),
		wf.On(dExit(125), cmd.To(fail, "runner failed"))))
	work.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[ucWork](wf.Const(1), work.To(fail, "gave up")))
	done := work.To(ok, "ok")
	work.Route(func(chOut, wf.Ctx) wf.Edge[ucWork] { return done }, done)
	res := func(wf.Ctx) chRes { return chRes{} }
	wf.Yield(ok, res)
	wf.Yield(fail, res)
	return wf.Typed[chItem, chRes](d)
}

// A direct next --until-unit (not a fork) must not bind while a sibling's
// detached command is running (the workflow run review P1 #2): its process keeps
// writing the world and, under the boundary, nothing would take its result.
// Sibling effects that have not started stay allowed.
func TestUntilUnitRefusesSiblingCommandInFlight(t *testing.T) {
	d := wf.New("until-cmd")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, cmdOrWorkChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	fr := newDetRunner()
	h := newHarnessOpts(t, d, newWorld(), detOpts(t, fr))
	run := h.run(d, nil)
	h.next(run)
	if fr.startCount() != 1 {
		t.Fatalf("detached launches = %d, want item 1's command running", fr.startCount())
	}
	err := h.e.(*wf.Engine).BindUntilUnit(run, wf.UntilUnitTarget{Instance: "chFan[2]", State: "ucWork"})
	if !errors.Is(err, wf.ErrRejected) || !strings.Contains(err.Error(), "a command is in flight in chFan[1]") {
		t.Fatalf("bind beside a running sibling command: %v", err)
	}
}
