package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type turnCLI struct {
	t        *testing.T
	state    string
	registry string
	work     string
	cfg      wf.MainConfig
}

func newTurnCLI(t *testing.T, d *wf.Def, extra ...string) *turnCLI {
	t.Helper()
	return newTurnCLIWithRegistry(t, d, t.TempDir(), extra...)
}

func newTurnCLIWithRegistry(t *testing.T, d *wf.Def, registry string, extra ...string) *turnCLI {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspace with spaces")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := &turnCLI{t: t, registry: registry, state: filepath.Join(root, "run state"), work: root, cfg: wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Binary: "super"}}
	cli.must("A", append([]string{"run", "--workspace", root, "--state-dir", cli.state, "--run-id", "r"}, extra...)...)
	return cli
}

func (t *turnCLI) call(session string, args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := wf.CLIWithEnv(t.cfg, args, &out, &stderr, func(k string) string {
		switch k {
		case "XDG_STATE_HOME":
			return t.registry
		case "CLAUDECODE":
			if session != "" {
				return "1"
			}
		case "CLAUDE_CODE_SESSION_ID":
			return session
		}
		return ""
	})
	return code, out.String(), stderr.String()
}

func (t *turnCLI) must(session string, args ...string) string {
	t.t.Helper()
	code, out, stderr := t.call(session, args...)
	if code != 0 {
		t.t.Fatalf("%v exited %d: %s", args, code, stderr)
	}
	return out
}

func (t *turnCLI) args(verb string, extra ...string) []string {
	args := []string{"wf", verb}
	args = append(args, extra...)
	return append(args, "--state-dir", t.state, "--run-id", "r")
}

func TestCoordinatorTurnQueryIsReadOnlyAndFenced(t *testing.T) {
	cli := newTurnCLI(t, clAgentDef("turn-query"))
	cli.must("A", cli.args("attach-coordinator", "--initial")...)
	before := cli.must("", cli.args("journal")...)
	decision := cli.must("A", cli.args("may-end-turn", "--json")...)
	var got struct {
		Decision   string   `json:"decision"`
		Admission  int64    `json:"admission"`
		Argv       []string `json:"argv"`
		WorkingDir string   `json:"workingDir"`
	}
	if err := json.Unmarshal([]byte(decision), &got); err != nil {
		t.Fatal(err)
	}
	if got.Decision != "continue" || got.Admission <= 0 || got.WorkingDir != cli.work || len(got.Argv) == 0 {
		t.Fatalf("query = %s", decision)
	}
	if after := cli.must("", cli.args("journal")...); after != before {
		t.Fatal("the read-only query changed the journal")
	}

	cli.must("B", cli.args("attach-coordinator", "--initial")...)
	before = cli.must("", cli.args("journal")...)
	if code, _, stderr := cli.call("A", got.Argv[1:]...); code != 4 || !strings.Contains(stderr, "attachment") {
		t.Fatalf("stale continuation exited %d: %s", code, stderr)
	}
	if after := cli.must("", cli.args("journal")...); after != before {
		t.Fatal("a stale continuation changed the journal")
	}
	if code, _, stderr := cli.call("A", cli.args("next", "1", "--json")...); code != 4 || !strings.Contains(stderr, "attachment") {
		t.Fatalf("ordinary next from replaced coordinator exited %d: %s", code, stderr)
	}
}

func TestCoordinatorHandoffAcknowledgementBelongsToTheAttachment(t *testing.T) {
	cli := newTurnCLI(t, clGateDef())
	cli.must("A", cli.args("attach-coordinator", "--initial")...)
	readReceipt := func(session string) int64 {
		t.Helper()
		code, raw, stderr := cli.call(session, cli.args("next", "1", "--json")...)
		if code != 7 {
			t.Fatalf("next exited %d: %s", code, stderr)
		}
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(raw), &ins); err != nil || len(ins) != 1 || ins[0].HandoffReceipt <= 0 {
			t.Fatalf("next = %s (%v), want a receipt", raw, err)
		}
		return ins[0].HandoffReceipt
	}
	decision := func(session, want string) {
		t.Helper()
		got := cli.must(session, cli.args("may-end-turn")...)
		if !strings.HasPrefix(got, want+" ") {
			t.Fatalf("decision = %q, want %q", got, want)
		}
	}
	receipt := readReceipt("A")
	decision("A", "continue")
	if next := readReceipt("A"); next != receipt {
		t.Fatalf("redelivery receipt = %d, want %d", next, receipt)
	}
	ack := func(session string, seq int64) (int, string) {
		code, _, stderr := cli.call(session, cli.args("acknowledge-handoff", "--receipt", strconv.FormatInt(seq, 10))...)
		return code, stderr
	}
	if code, stderr := ack("A", receipt); code != 0 {
		t.Fatalf("A ack exited %d: %s", code, stderr)
	}
	decision("A", "allow")
	cli.must("B", cli.args("attach-coordinator", "--initial")...)
	decision("B", "continue")
	if code, _ := ack("B", receipt); code != 4 {
		t.Fatalf("B accepted previous receipt, exit %d", code)
	}
	newReceipt := readReceipt("B")
	if newReceipt == receipt {
		t.Fatal("a replacement coordinator reused another admission's receipt")
	}
	if code, stderr := ack("B", newReceipt); code != 0 {
		t.Fatalf("B ack exited %d: %s", code, stderr)
	}
	decision("B", "allow")
}

func coordinatorTurnStall(t *testing.T) (*wf.Engine, wf.RunID, wf.Coordinator, wf.TurnEndDecision) {
	t.Helper()
	d := wtGateDef("turn-stall", 0)
	e := wf.NewEngine(wf.FileJournal(t.TempDir()), nil, wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "r", Harness: "claude", Workspace: t.TempDir()}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := wf.Coordinator{Harness: "claude", Session: "coord"}
	gen, err := e.AttachCoordinator(run, co)
	if err != nil {
		t.Fatal(err)
	}
	co.ExpectedAdmission = gen
	record := func(id, decision string) wf.TurnEndDecision {
		t.Helper()
		r, err := e.RecordCoordinatorStop(run, co, id, "")
		if err != nil || r.Decision != decision {
			t.Fatalf("stop %s = %+v, %v; want %s", id, r, err, decision)
		}
		return r
	}
	for i := 1; i <= 5; i++ {
		record(strconv.Itoa(i), "continue")
	}
	before, err := e.Status(run)
	if err != nil || before.CoordinatorTurn == nil || before.CoordinatorTurn.Continuations != 5 {
		t.Fatalf("status after five blocks = %+v, %v", before.CoordinatorTurn, err)
	}
	record("5", "continue") // a duplicate on the boundary does not consume it
	stop := record("6", "allow")
	again := record("6", "allow")
	if stop.Reason != "continuation_stalled" || again.Reason != stop.Reason || again.Seq != stop.Seq+1 {
		t.Fatalf("cap decisions = %+v and %+v", stop, again)
	}
	return e, run, co, stop
}

func TestCoordinatorTurnCapAndOperatorBoundary(t *testing.T) {
	e, run, co, stalled := coordinatorTurnStall(t)
	st, err := e.Status(run)
	if err != nil || st.CoordinatorTurn == nil || st.CoordinatorTurn.Continuations != 5 ||
		st.CoordinatorTurn.StallSeq != stalled.Seq+1 || st.CoordinatorTurn.LastSeq != stalled.Seq {
		t.Fatalf("stall status = %+v, %v", st.CoordinatorTurn, err)
	}
	if err := e.Stop(run, "resume the coordinator"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOutAs(run, 1, co); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("fenced next while paused = %v, want refusal", err)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	gen, err := e.AttachCoordinator(run, co)
	if err != nil || gen == co.ExpectedAdmission {
		t.Fatalf("reattach = %d, %v, want new generation", gen, err)
	}
	if _, err := e.HandOutAs(run, 1, co); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("old fenced next after resume = %v, want refusal", err)
	}
	co.ExpectedAdmission = gen
	again, err := e.RecordCoordinatorStop(run, co, "new", "")
	if err != nil || again.Decision != "continue" {
		t.Fatalf("new segment decision = %+v, %v", again, err)
	}
}

func TestCoordinatorStopRechecksGateAnswerAndProgress(t *testing.T) {
	for _, answerBeforeStop := range []bool{true, false} {
		t.Run(strconv.FormatBool(answerBeforeStop), func(t *testing.T) {
			def := turnRecheckDef()
			cli := newTurnCLI(t, def)
			gen, err := strconv.ParseInt(strings.TrimSpace(cli.must("A", cli.args("attach-coordinator", "--initial")...)), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			e := wf.NewEngine(wf.FileJournal(cli.state), nil, wf.EngineOptions{Defs: []*wf.Def{def}, StateRoot: cli.state}).(*wf.Engine)
			co := wf.Coordinator{Harness: "claude", Session: "A", ExpectedAdmission: gen}
			for i := 1; i <= 5; i++ {
				stop, err := e.RecordCoordinatorStop("r", co, strconv.Itoa(i), cli.work)
				if err != nil || stop.Decision != "continue" {
					t.Fatalf("stop %d = %+v, %v", i, stop, err)
				}
			}
			ins, err := e.HandOutAs("r", 1, co)
			if err != nil || len(ins) != 1 || ins[0].HandoffReceipt == 0 {
				t.Fatalf("next = %+v, %v", ins, err)
			}
			cli.must("A", cli.args("acknowledge-handoff", "--receipt", strconv.FormatInt(ins[0].HandoffReceipt, 10))...)
			query, err := e.MayEndTurn("r", co)
			if err != nil || query.Reason != "person_handoff" {
				t.Fatalf("query = %+v, %v", query, err)
			}
			answer := func() {
				t.Helper()
				if err := e.Answer("r", ins[0].State, "go"); err != nil {
					t.Fatal(err)
				}
			}
			if answerBeforeStop {
				answer()
			}
			stop, err := e.RecordCoordinatorStop("r", co, "sixth", cli.work)
			if err != nil {
				t.Fatal(err)
			}
			if answerBeforeStop {
				if stop.Decision != "continue" || stop.Progress <= query.Progress {
					t.Fatalf("answer before Stop = %+v, query was %+v", stop, query)
				}
			} else {
				if stop.Reason != "person_handoff" || stop.Seq != query.Seq {
					t.Fatalf("Stop before answer = %+v, query was %+v", stop, query)
				}
				answer()
			}
			status, err := e.Status("r")
			if err != nil || status.Phase != wf.RunRunning || status.CoordinatorTurn.StallSeq != 0 {
				t.Fatalf("answer left status %+v, %v", status, err)
			}
		})
	}
}

func TestCoordinatorCrossWorkspaceSwitchInvalidatesPreviousRun(t *testing.T) {
	d := clAgentDef("turn-cross-workspace")
	registry := t.TempDir()
	a := newTurnCLIWithRegistry(t, d, registry)
	b := newTurnCLIWithRegistry(t, clAgentDef("turn-other-client"), registry)
	raw := a.must("A", a.args("attach-coordinator", "--initial")...)
	old, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	b.must("A", b.args("attach-coordinator")...)
	e := wf.NewEngine(wf.FileJournal(a.state), nil, wf.EngineOptions{Defs: []*wf.Def{d}, StateRoot: a.state}).(*wf.Engine)
	co := wf.Coordinator{Harness: "claude", Session: "A", ExpectedAdmission: old}
	stop, err := e.RecordCoordinatorStop("r", co, "old", a.work)
	if err != nil || stop.Decision != "unavailable" {
		t.Fatalf("old run remained active: %+v, %v", stop, err)
	}
	if _, err := e.HandOutAs("r", 1, co); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("next on the old run = %v, want refusal", err)
	}
}

type turnWork struct{}

func turnRecheckDef() *wf.Def {
	d := wf.New("turn-recheck")
	gate := wf.NewGate[wtAsk](d, "Go?", wf.NewEnum(d, "answer", "go"), wf.PolicyAnswerable)
	work := wf.NewAgent[turnWork, string](d)
	terminal := wf.NewTerminal[wtStop](d, wf.Success)
	gate.Answers(wf.On("go", gate.To(work, "go")))
	done := work.To(terminal, "done")
	failed := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, failed)
	work.Spec(wf.PromptText("Return a result."), wf.Retry[turnWork](wf.Const(1), work.To(failed, "gave up")))
	work.Route(func(string, wf.Ctx) wf.Edge[turnWork] { return done }, done)
	return d
}

func TestCoordinatorAcknowledgesAllPersonGatesOfAFixedMap(t *testing.T) {
	d := wf.New("turn-many-gates")
	child := wf.New("turn-gate-child")
	gate := wf.NewGate[wtAsk](child, "Go?", wf.NewEnum(child, "answer", "go"), wf.PolicyAnswerable)
	end := wf.NewTerminal[wtStop](child, wf.Success)
	wf.Yield(end, func(wf.Ctx) chRes { return chRes{} })
	gate.Answers(wf.On("go", gate.To(end, "go")))
	m := wf.NewMap[chFan, chItem, int, chRes](d, chItems(1, 2), func(i chItem) int { return i.N }, wf.Typed[chItem, chRes](child))
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	cli := newTurnCLI(t, d)
	cli.must("A", cli.args("attach-coordinator", "--initial")...)
	var ins []wf.Instruction
	raw := cli.must("A", cli.args("next", "2", "--json")...)
	if err := json.Unmarshal([]byte(raw), &ins); err != nil || len(ins) != 2 || ins[0].HandoffReceipt == 0 ||
		ins[0].HandoffReceipt != ins[1].HandoffReceipt {
		t.Fatalf("the two person gates did not receive a shared receipt: %s, %v", raw, err)
	}
	receipt := strconv.FormatInt(ins[0].HandoffReceipt, 10)
	cli.must("A", cli.args("acknowledge-handoff", "--receipt", receipt)...)
	if decision := cli.must("A", cli.args("may-end-turn")...); decision != "allow person_handoff\n" {
		t.Fatalf("two person gates left decision %q", decision)
	}
}
