package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type breakpointStart struct{}
type breakpointWork struct{}
type breakpointEnd struct{}

func TestRunBreakpointStopsBeforeExecutionAndRearms(t *testing.T) {
	d := wf.New("breakpoint")
	var executions int
	start := wf.NewCompute[breakpointStart, int](d, func(wf.Ctx) (int, error) { return 0, nil })
	work := wf.NewCompute[breakpointWork, int](d, func(wf.Ctx) (int, error) {
		executions++
		return executions, nil
	})
	end := wf.NewTerminal[breakpointEnd](d, wf.Success)
	intoWork := start.To(work, "start work")
	start.Route(func(int, wf.Ctx) wf.Edge[breakpointStart] { return intoWork }, intoWork)
	again, done := work.To(work, "repeat"), work.To(end, "done")
	work.Route(func(n int, _ wf.Ctx) wf.Edge[breakpointWork] {
		if n == 1 {
			return again
		}
		return done
	}, again, done)

	state := filepath.Join(t.TempDir(), "state")
	run := wf.RunID(strings.TrimSpace(mustSingle(t, d, "run", "--run-id", "test-run", "--halt-before", "breakpointWork", "--state-dir", state, "--workspace", t.TempDir())))
	next := func() []wf.Instruction {
		t.Helper()
		var ins []wf.Instruction
		code, raw, stderr := singleCLI(d, "wf", "next", "1", "--state-dir", state, "--json")
		if code != 7 {
			t.Fatalf("next exited %d, want 7: %s", code, stderr)
		}
		if err := json.Unmarshal([]byte(raw), &ins); err != nil {
			t.Fatalf("decoding next: %v (%s)", err, raw)
		}
		return ins
	}
	for want := 0; want < 2; want++ {
		ins := next()
		if len(ins) != 1 || ins[0].Kind != wf.InstructionStopped || !strings.Contains(ins[0].Prompt, "breakpointWork") || executions != want {
			t.Fatalf("next = %+v, state ran %d times, want breakpoint after %d executions", ins, executions, want)
		}
		status := mustSingle(t, d, "wf", "status", "--state-dir", state)
		if !strings.Contains(status, "breakpoint") || !strings.Contains(status, "breakpointWork") {
			t.Fatalf("status does not report the breakpoint: %s", status)
		}
		// Inspect a new process's view and ensure stop does not discard the breakpoint.
		mustSingle(t, d, "wf", "stop", "--state-dir", state)
		recs, err := wf.ReadJournal(state, run)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, r := range recs {
			if r.Kind.String() == "breakpoint_halted" {
				got++
				if r.State != "breakpointWork" || r.Seq <= 0 {
					t.Fatalf("halt record = %+v", r)
				}
			}
		}
		if got != want+1 {
			t.Fatalf("breakpoint halt records = %d, want %d", got, want+1)
		}
		resumeBreakpoint(t, d, state)
	}
	if ins := next(); executions != 2 || len(ins) != 1 || ins[0].Kind != wf.InstructionEnded {
		t.Fatalf("after resume: next = %+v, state ran %d times", ins, executions)
	}
}

type breakpointLeft struct{}
type breakpointRight struct{}

func TestRunBreakpointRequiresQualificationForDuplicateNestedState(t *testing.T) {
	child := func(name string) wf.Child[struct{}, struct{}] {
		d := wf.New(name)
		end := wf.NewTerminal[breakpointEnd](d, wf.Success)
		wf.Yield(end, func(wf.Ctx) struct{} { return struct{}{} })
		return wf.Typed[struct{}, struct{}](d)
	}
	d := wf.New("nested")
	left := wf.NewSub[breakpointLeft](d, child("left"), func(wf.Ctx) struct{} { return struct{}{} })
	right := wf.NewSub[breakpointRight](d, child("right"), func(wf.Ctx) struct{} { return struct{}{} })
	end := wf.NewTerminal[breakpointWork](d, wf.Success)
	left.Exits(wf.OnExit[breakpointEnd](left.To(right, "left done")))
	right.Exits(wf.OnExit[breakpointEnd](right.To(end, "right done")))

	state, work := t.TempDir(), t.TempDir()
	args := []string{"run", "--workspace", work, "--state-dir", state, "--run-id", "nested-run", "--halt-before"}
	code, _, stderr := singleCLI(d, slices.Concat(args, []string{"breakpointEnd"})...)
	if code == 0 || !strings.Contains(stderr, "ambiguous") || !strings.Contains(stderr, "left/breakpointEnd") || !strings.Contains(stderr, "right/breakpointEnd") {
		t.Fatalf("ambiguous run start exited %d: %s", code, stderr)
	}
	mustSingle(t, d, slices.Concat(args, []string{"right/breakpointEnd"})...)
	var ins []wf.Instruction
	code, raw, stderr := singleCLI(d, "wf", "next", "1", "--state-dir", state, "--json")
	if code != 7 {
		t.Fatalf("next exited %d, want 7: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(raw), &ins); err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != wf.InstructionStopped || !strings.Contains(ins[0].Prompt, "right/breakpointEnd") {
		t.Fatalf("next = %+v, want breakpoint at right/breakpointEnd", ins)
	}
}

func TestRunBreakpointReplayHonorsRecordedResume(t *testing.T) {
	d := wf.New("debug-replay")
	start := wf.NewCompute[breakpointStart, int](d, func(wf.Ctx) (int, error) { return 1, nil })
	end := wf.NewTerminal[breakpointEnd](d, wf.Success)
	toEnd := start.To(end, "done")
	start.Route(func(int, wf.Ctx) wf.Edge[breakpointStart] { return toEnd }, toEnd)

	state := t.TempDir()
	run := wf.RunID(strings.TrimSpace(mustSingle(t, d, "run", "--run-id", "test-run", "--halt-before", "breakpointStart", "--state-dir", state, "--workspace", t.TempDir())))
	replay := func() wf.ReplayReport {
		t.Helper()
		recs, err := wf.ReadJournal(state, run)
		if err != nil {
			t.Fatal(err)
		}
		report, err := wf.Replay(recs, d, "")
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	if got := replay().Stop; got.Kind != "breakpoint" || !strings.Contains(got.Now, "breakpointStart") {
		t.Fatalf("replay of paused run stopped at %+v", got)
	}
	resumeBreakpoint(t, d, state)
	if got := replay().Stop; got.Kind != "ended" || !strings.Contains(got.Now, "breakpointEnd") {
		t.Fatalf("replay of resumed run stopped at %+v", got)
	}
}

func resumeBreakpoint(t *testing.T, d *wf.Def, state string) {
	t.Helper()
	var out, stderr bytes.Buffer
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Binary: "super"}
	args := []string{"wf", "resume", "--operator", "tester", "--reason", "continue past breakpoint", "--state-dir", state}
	if code := wf.CLIWithEnv(cfg, args, &out, &stderr, func(string) string { return "" }); code != 0 {
		t.Fatalf("resume exited %d: %s", code, &stderr)
	}
}

func TestRunBreakpointRearmsAtRewoundArrival(t *testing.T) {
	refuse := true
	d := rewindDef(&refuse)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{HaltBefore: []string{"rw-design/rwGate"}})
	e := h.e.(*wf.Engine)
	run := h.run(d, nil)
	ask := h.next(run)[0]
	h.submit(run, ask, `{}`)
	if got := h.status(run).Breakpoint; got == nil || got.State != "rw-design/rwGate" {
		t.Fatalf("source breakpoint = %+v", got)
	}
	if err := e.Resume(run); err != nil {
		t.Fatal(err)
	}
	if got := h.status(run); got.Phase != wf.RunEnded || got.Terminal != "rwStopRoot" {
		t.Fatalf("source = %+v, want ended at rwStopRoot", got)
	}
	fork, err := e.Rewind(run, "died")
	if err != nil {
		t.Fatal(err)
	}
	ins := h.next(fork.Run)
	if len(ins) != 1 || ins[0].Kind != wf.InstructionStopped || ins[0].Breakpoint == nil || ins[0].Breakpoint.State != "rw-design/rwGate" {
		t.Fatalf("rewound next = %+v, want the breakpoint armed again", ins)
	}
}

func TestRunBreakpointPreventsGateAnswersAndTimeout(t *testing.T) {
	d := wtGateDef("breakpoint-gate", time.Minute)
	makeRun := func(t *testing.T) (*harness, wf.RunID) {
		t.Helper()
		h := newHarnessOpts(t, d, nil, wf.EngineOptions{HaltBefore: []string{"wtAsk"}})
		return h, h.run(d, nil)
	}
	t.Run("answer by name", func(t *testing.T) {
		h, run := makeRun(t)
		if err := h.e.Answer(run, "wtAsk", "go"); !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("answer at suspended gate: %v, want ErrRejected", err)
		}
	})
	t.Run("answer by visit", func(t *testing.T) {
		h, run := makeRun(t)
		err := h.e.AnswerVisit(run, wf.GateAnswer{Gate: "wtAsk", Value: "go", Visit: 1})
		if !errors.Is(err, wf.ErrRejected) {
			t.Fatalf("answer at suspended gate: %v, want ErrRejected", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h, run := makeRun(t)
		h.now = h.now.Add(2 * time.Minute)
		if ins := h.next(run); len(ins) != 1 || ins[0].Kind != wf.InstructionStopped {
			t.Fatalf("next after deadline = %+v, want the breakpoint", ins)
		}
		for _, r := range h.records(run) {
			if r.Kind == wf.JournalGateAnswered || r.Kind == wf.JournalAutoAnswered || r.Kind == wf.JournalRefused || r.Kind == wf.JournalGateOffered {
				t.Errorf("the gate acted while stopped: %s at seq %d", r.Kind, r.Seq)
			}
		}
		if err := h.e.(*wf.Engine).Resume(run); err != nil {
			t.Fatal(err)
		}
		if status := h.status(run); status.Phase != wf.RunEnded || status.Terminal != "wtStop" {
			t.Fatalf("resume after gate deadline = %s / %s, want ended / wtStop", status.Phase, status.Terminal)
		}
	})
}

func TestRunBreakpointOnlyOperatorCanResume(t *testing.T) {
	d := wtGateDef("initial-breakpoint", 0)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{HaltBefore: []string{"wtAsk"}})
	run := h.run(d, nil)
	e := h.e.(*wf.Engine)
	co := wf.Coordinator{Session: "coordinator", Harness: "claude"}
	ins, err := e.HandOutAs(run, 1, co)
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionStopped {
		t.Fatalf("coordinator next = %+v, %v, want breakpoint", ins, err)
	}
	op := wf.Operator{Name: "coordinator", Session: co.Session, Harness: co.Harness, Reason: "continue"}
	if err := e.ResumeAs(run, op); !errors.Is(err, wf.ErrRejected) {
		t.Fatalf("coordinator resume = %v, want ErrRejected", err)
	}
}

type breakpointRenamed struct{}

func breakpointRenameDef(renamed bool) *wf.Def {
	d := wf.New("rename-breakpoint")
	start := wf.NewCompute[breakpointStart, int](d, func(wf.Ctx) (int, error) { return 0, nil })
	var target wf.State
	if renamed {
		target = wf.NewTerminal[breakpointRenamed](d, wf.Success)
	} else {
		target = wf.NewTerminal[breakpointEnd](d, wf.Success)
	}
	toTarget := start.To(target, "done")
	start.Route(func(int, wf.Ctx) wf.Edge[breakpointStart] { return toTarget }, toTarget)
	return d
}

func TestRunBreakpointFollowsDeclaredStateRename(t *testing.T) {

	old, current := breakpointRenameDef(false), breakpointRenameDef(true)
	current.Migrations(wf.Migration{FromHash: old.Hash(), ToHash: current.Hash(), States: map[string]string{"breakpointEnd": "breakpointRenamed"}})
	h := newHarnessOpts(t, old, nil, wf.EngineOptions{HaltBefore: []string{"breakpointEnd"}})
	run := h.run(old, nil)
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{current}}).(*wf.Engine)
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Breakpoint == nil || ins[0].Breakpoint.State != "breakpointRenamed" {
		t.Fatalf("next after migration = %+v, %v, want the renamed breakpoint", ins, err)
	}
	rep, err := wf.Replay(h.records(run), current, "")
	if err != nil || rep.Stop.Kind != "breakpoint" || rep.Stop.State != "breakpointRenamed" {
		t.Fatalf("replay after migration stopped at %+v, %v", rep.Stop, err)
	}
}

func TestRunBreakpointAdoptionRefusesToDropTarget(t *testing.T) {
	old, replacement := breakpointRenameDef(false), breakpointRenameDef(true)
	h := newHarnessOpts(t, old, nil, wf.EngineOptions{HaltBefore: []string{"breakpointEnd"}})
	run := h.run(old, nil)
	before := len(h.records(run))
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{replacement}, Adopt: true}).(*wf.Engine)
	if _, _, err := e.AdoptOnly(run); !errors.Is(err, wf.ErrDefChanged) || !strings.Contains(err.Error(), "breakpointEnd") {
		t.Fatalf("adoption with a removed breakpoint = %v, want ErrDefChanged naming the target", err)
	}
	if after := len(h.records(run)); after != before {
		t.Fatalf("refused adoption wrote %d journal records", after-before)
	}
}

func TestRunBreakpointStatusDoesNotEvaluateGateProposal(t *testing.T) {
	d := wf.New("breakpoint-proposal")
	answers := wf.NewEnum(d, "integer", "set")
	gate := wf.NewGate[breakpointStart, string](d, "Choose a value", answers, wf.Reserved)
	var proposals int
	gate.IntValue("set", 1, 10, func(wf.Ctx) int {
		proposals++
		return 5
	})
	done := wf.NewTerminal[breakpointEnd](d, wf.Success)
	gate.Answers(wf.On("set", gate.To(done, "done")))
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{HaltBefore: []string{"breakpointStart"}})
	run := h.run(d, nil)
	if st := h.status(run); st.Breakpoint == nil || proposals != 0 {
		t.Fatalf("status = %+v, evaluated %d proposals before resume", st.Breakpoint, proposals)
	}
	if err := h.e.(*wf.Engine).Resume(run); err != nil {
		t.Fatal(err)
	}
	if proposals == 0 {
		t.Fatal("resuming did not evaluate the gate proposal")
	}
}

func TestRunBreakpointBareNextAdmitsCoordinator(t *testing.T) {
	d := wtGateDef("named-breakpoint", 0)
	state := t.TempDir()
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Binary: "super"}
	call := func(session string, args ...string) (int, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		env := func(key string) string {
			switch key {
			case "CLAUDECODE":
				return "1"
			case "CLAUDE_CODE_SESSION_ID":
				return session
			default:
				return ""
			}
		}
		code := wf.CLIWithEnv(cfg, append(args, "--state-dir", state), &out, &stderr, env)
		return code, out.String() + stderr.String()
	}
	if code, out := call("coordinator", "run", "--workspace", t.TempDir(), "--run-id", "held", "--halt-before", "wtAsk"); code != 0 {
		t.Fatalf("run exited %d: %s", code, out)
	}
	if code, out := call("coordinator", "next"); code != 7 || !strings.Contains(out, "breakpoint") {
		t.Fatalf("bare next exited %d: %s", code, out)
	}
	args := []string{"wf", "resume", "--operator", "tester", "--reason", "continue"}
	if code, out := call("coordinator", args...); code != 4 || !strings.Contains(out, "its own stop") {
		t.Fatalf("coordinator resume exited %d: %s", code, out)
	}
	if code, out := call("operator", args...); code != 0 {
		t.Fatalf("operator resume exited %d: %s", code, out)
	}
}

func TestRunBreakpointActiveStatusFollowsDeclaredRename(t *testing.T) {
	old, current := breakpointRenameDef(false), breakpointRenameDef(true)
	current.Migrations(wf.Migration{FromHash: old.Hash(), ToHash: current.Hash(), States: map[string]string{"breakpointEnd": "breakpointRenamed"}})
	h := newHarnessOpts(t, old, nil, wf.EngineOptions{HaltBefore: []string{"breakpointEnd"}, RunID: "renamed"})
	run := h.run(old, nil)
	h.next(run)
	before := h.status(run).Breakpoint
	if before == nil {
		t.Fatal("initial breakpoint did not halt")
	}
	e := wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{current}, RunID: run}).(*wf.Engine)
	if _, err := e.Run(current, nil, nil); err != nil {
		t.Fatal(err)
	}
	st, err := e.Status(run)
	b := st.Breakpoint
	if err != nil || b == nil || b.State != "breakpointRenamed" || b.Seq != before.Seq || b.ArrivalSeq != before.ArrivalSeq {
		t.Fatalf("migrated breakpoint = %+v, %v, want renamed state with original sequences", b, err)
	}
}

func TestRunBreakpointWaitReportsHalt(t *testing.T) {
	d := breakpointRenameDef(false)
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{HaltBefore: []string{"breakpointEnd"}})
	run := h.run(d, nil)
	res, err := h.e.(*wf.Engine).Wait(run, 1, 10*time.Millisecond)
	if err != nil || len(res.Events) != 1 || res.Events[0].Kind != wf.EventBreakpoint || res.Events[0].State != "breakpointEnd" {
		t.Fatalf("wait = %+v, %v, want the breakpoint event", res, err)
	}
	again, err := h.e.(*wf.Engine).Wait(run, res.Seq, 10*time.Millisecond)
	if err != nil || len(again.Events) != 1 || again.Events[0] != res.Events[0] {
		t.Fatalf("wait after cursor = %+v, %v, want the active breakpoint again", again, err)
	}
}

func TestRunBreakpointMapStatusDoesNotProjectItems(t *testing.T) {
	d := wf.New("breakpoint-map")
	var calls int
	fan := wf.NewMap[chFan](d, chItems(1), func(chItem) string {
		calls++
		return "one"
	}, chChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	fan.Parallel(1).Join(wf.AllSettled(), fan.To(merged, "met"), fan.To(partial, "partial"))

	state := t.TempDir()
	mustSingle(t, d, "run", "--run-id", "map", "--workspace", t.TempDir(), "--state-dir", state, "--halt-before", "chFan")
	if calls != 0 {
		t.Errorf("run bootstrap called the halted Map's key function %d times", calls)
	}
	calls = 0
	mustSingle(t, d, "wf", "status", "--state-dir", state, "--json")
	if calls != 0 {
		t.Errorf("status called the halted Map's key function %d times", calls)
	}
}
