package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A person gate reached by manufacture (no --apply-trigger) ends with the
// gate offered: its offer journaled behind an available checkpoint, the
// report naming that offer, and fanout taking the gate by its qualified name
// (m4: finish/Menu, FeedbackGate and ConfirmDiscard were unforkable).
func TestManufactureEndsWithTheGateOffered(t *testing.T) {
	_, _, ws := manufactureCLI(t)
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	gd := policyDef(wf.PolicyAnswerable)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: gd}}, Adapters: reg}
	p := writeScript(t, `{"edge":"policy/pAsk/edge[0]","registration":"policy","target_state":"policy/pAsk","hash":"`+gd.Hash()+`","run_id":"g",
		"steps":[],"trigger":{"op":"answer","value":"go"}}`)
	out := filepath.Join(t.TempDir(), "gate")
	if code, stdout, stderr := cliCall(cfg, "dev", "manufacture", "policy", "--script", p, "--workspace", ws, "--out", out, "--state-dir", "state"); code != 0 {
		t.Fatalf("manufacture %d:\n%s\n%s", code, stdout, stderr)
	}
	var rep wf.ManufactureReport
	raw, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil || json.Unmarshal(raw, &rep) != nil {
		t.Fatalf("report %s %v", raw, err)
	}
	if !rep.Reached || rep.Target == nil || rep.Target.Seq == 0 || rep.Target.SnapshotSeq == 0 {
		t.Fatalf("report target %s: want the gate's offer and its checkpoint", raw)
	}
	state := filepath.Join(out, "workspace", "state")
	recs, err := wf.ReadJournal(state, rep.Run)
	if err != nil {
		t.Fatal(err)
	}
	if r := recs[rep.Target.Seq-1]; r.Kind != wf.JournalGateOffered || r.State != "pAsk" {
		t.Fatalf("target seq %d is %s %s, want the gate's gate_offered", rep.Target.Seq, r.Kind, r.State)
	}
	code, stdout, stderr := cliCall(cfg, "dev", "fanout", string(rep.Run), "--state-dir", state, "--workspace", filepath.Join(out, "workspace"), "--out", filepath.Join(t.TempDir(), "b"),
		"--parallel", "1", "--coordinator-cmd", `["true"]`, "--dry-run", "--states", "pAsk")
	if code != 0 || strings.Contains(stderr, "must name one unique") {
		t.Fatalf("fanout of the gate %d:\n%s\n%s", code, stdout, stderr)
	}
}

// gateRun runs policyDef with snapshots on to its person gate, offered.
func gateRun(t *testing.T) (*wf.Engine, *wf.Adapters, *wf.Def, string, string, wf.RunID) {
	t.Helper()
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := policyDef(wf.PolicyAnswerable)
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
		t.Fatalf("next = %+v %v, want the gate", ins, err)
	}
	return e, reg, d, ws, state, run
}

// A gate's offer is journaled once per visit, behind a checkpoint.
func TestGateOfferedOncePerVisit(t *testing.T) {
	e, _, _, _, state, run := gateRun(t)
	for i := 0; i < 3; i++ { // presenting it again journals nothing more
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var offers []wf.JournalRecord
	for _, r := range recs {
		if r.Kind == wf.JournalGateOffered {
			offers = append(offers, r)
		}
	}
	if len(offers) != 1 {
		t.Fatalf("%d gate_offered records, want one per visit", len(offers))
	}
	if prev := recs[offers[0].Seq-2]; prev.Kind != wf.JournalWorldSnapshot {
		t.Fatalf("record before gate_offered is %s, want its checkpoint", prev.Kind)
	}
}

type (
	goAsk  struct{}
	goWork struct{}
	goDone struct{}
	goStop struct{}
	goAns  string
)

// A gate's offer counts no turn: under a one-turn budget the agent after
// the gate is still issued.
func TestGateOfferCountsNoTurn(t *testing.T) {
	d := wf.New("gate-turn")
	ans := wf.NewEnum(d, "go-answer", goAns("go"), goAns("stop"))
	g := wf.NewGate[goAsk, goAns](d, "Go?", ans, wf.PolicyAnswerable)
	w := wf.NewAgent[goWork, string](d)
	done := wf.NewTerminal[goDone](d, wf.Success)
	stop := wf.NewTerminal[goStop](d, wf.Stopped)
	d.OnFailure(wf.Blocked, stop)
	g.Answers(wf.On(goAns("go"), g.To(w, "go")), wf.On(goAns("stop"), g.To(stop, "stop")))
	w.Spec(wf.PromptText("work"), wf.Retry[goWork](wf.Const(1), w.To(stop, "gave up")))
	fin := w.To(done, "done")
	w.Route(func(string, wf.Ctx) wf.Edge[goWork] { return fin }, fin)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1}})
	run := h.run(d, nil)
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate {
		t.Fatalf("next = %+v, want the gate", ins)
	}
	h.next(run) // presented again
	if err := h.e.Answer(run, "goAsk", "go"); err != nil {
		t.Fatal(err)
	}
	if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionAgent {
		t.Fatalf("next after the answer = %+v, want the agent within the one-turn budget", ins)
	}
	if n := len(h.kind(run, wf.JournalGateOffered)); n != 1 {
		t.Fatalf("%d gate offers, want 1", n)
	}
}

// A fork cut at the gate's checkpoint re-presents the gate; the fork's
// coordinator answers it, which settles the fork's until-unit boundary.
func TestForkedGateIsAnsweredByTheForksCoordinator(t *testing.T) {
	e, reg, d, _, _, run := gateRun(t)
	plan, err := e.PlanFanout(run, "pAsk")
	if err != nil || len(plan.Targets) != 1 || !plan.Targets[0].Gate {
		t.Fatalf("plan = %+v %v, want the gate", plan, err)
	}
	target := plan.Targets[0]
	fork, err := e.ForkSnapshot(run, target.SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	fe := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
	if err := fe.BindUntilUnitName(run, target.Unit); err != nil {
		t.Fatalf("bind %s: %v", target.Unit, err)
	}
	ins, err := fe.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionHumanGate || ins[0].State != "pAsk" {
		t.Fatalf("fork next = %+v %v, want the gate re-presented", ins, err)
	}
	if err := fe.Answer(run, "pAsk", "go"); err != nil {
		t.Fatalf("answer in the fork: %v", err)
	}
	st, err := fe.UntilUnitStatus(run)
	if err != nil || !st.Settled || st.Class != wf.Accepted {
		t.Fatalf("until-unit = %+v %v, want settled by the answer", st, err)
	}
}

// A journal written before gate offers were journaled replays unchanged.
func TestAJournalWithoutGateOffersReplays(t *testing.T) {
	e, _, d, _, state, run := gateRun(t)
	if err := e.Answer(run, "pAsk", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	recs, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	var old []wf.JournalRecord
	for _, r := range recs {
		if r.Kind == wf.JournalGateOffered {
			continue
		}
		r.Seq = int64(len(old) + 1)
		old = append(old, r)
	}
	for _, journal := range [][]wf.JournalRecord{recs, old} {
		rep, err := wf.Replay(journal, d, "")
		if err != nil || len(rep.Differences) != 0 {
			t.Fatalf("replay of %d records: %+v %v", len(journal), rep.Differences, err)
		}
	}
}
