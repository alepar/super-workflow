package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestFanoutCLIDryRunAndFakeCoordinator(t *testing.T) {
	// Explicit fixture identities keep the test independent of its host harness.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	t.Setenv("HERMES_SESSION_ID", "")
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "golden", RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	sourceJournal := filepath.Join(state, "runs", string(run), "journal.jsonl")
	before, err := os.ReadFile(sourceJournal)
	if err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}
	out := filepath.Join(t.TempDir(), "batch")
	args := []string{"dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--out", out, "--parallel", "2", "--coordinator-cmd", `["sh","-c","printf 'done'"]`}
	code, stdout, stderr := cliCall(cfg, append(args, "--dry-run")...)
	if code != 0 || !strings.Contains(stdout, "Defect detector only") || !strings.Contains(stdout, "snapshotStart#1") {
		t.Fatalf("dry-run %d: %s %s", code, stdout, stderr)
	}
	if _, err = os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("dry-run created output")
	}
	code, stdout, stderr = cliCall(cfg, args...)
	if code != 8 || !strings.Contains(stdout, "stalled") {
		t.Fatalf("fake %d: %s %s", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Forks []struct {
			Fork    wf.SnapshotFork
			Outcome wf.FanoutOutcome
		}
	}
	if err = json.Unmarshal(data, &report); err != nil || len(report.Forks) != 1 {
		t.Fatalf("report %s %v", data, err)
	}
	fork := report.Forks[0].Fork
	forkEngine := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir}).(*wf.Engine)
	bound, err := forkEngine.UntilUnitStatus(run)
	if err != nil || !bound.Bound {
		t.Fatalf("not durably bound: %+v %v", bound, err)
	}
	after, err := os.ReadFile(sourceJournal)
	if err != nil || string(after) != string(before) {
		t.Fatal("source journal changed")
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "data")); string(got) != "source" {
		t.Fatal("source world changed")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal([]string{"env", "WF_FANOUT_HELPER=1", bin, "-test.run=^TestFanoutCoordinatorHelper$", "--", "{workspace}", "{state}", "{run}"})
	if err != nil {
		t.Fatal(err)
	}
	doneOut := filepath.Join(t.TempDir(), "completed")
	doneArgs := append([]string(nil), args...)
	for i := range doneArgs {
		if doneArgs[i] == out {
			doneArgs[i] = doneOut
		}
		if i > 0 && doneArgs[i-1] == "--coordinator-cmd" {
			doneArgs[i] = string(command)
		}
	}
	doneCode, doneText, doneErr := cliCall(cfg, doneArgs...)
	if doneCode != 0 || !strings.Contains(doneText, "done") || !strings.Contains(doneText, "12") {
		t.Fatalf("completed probe %d: %s %s", doneCode, doneText, doneErr)
	}
	code, _, _ = cliCall(cfg, args...)
	if code != 2 {
		t.Fatalf("reused out: %d", code)
	}
}

// A separate process acts as the fake coordinator: it drives the restored
// engine and submits a fake child's output. It never calls a model service.
func TestFanoutCoordinatorHelper(t *testing.T) {
	if os.Getenv("WF_FANOUT_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	args := os.Args[len(os.Args)-3:]
	ws, state, run := args[0], args[1], wf.RunID(args[2])
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("next %v %v", ins, err)
	}
	if _, err = e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"done"`), Tokens: 12}); err != nil {
		t.Fatal(err)
	}
	fresh := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state})
	ins, err = fresh.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionStopped {
		t.Fatalf("boundary %v %v", ins, err)
	}
}

type (
	pxA     struct{}
	pxSetup struct{}
	pxB     struct{}
	pxDone  struct{}
)

// fanoutWithAnUnrelocatableEvent runs a fanout over a run whose state pxB is
// offered after an untyped source path was journaled, and reads its report.
func fanoutWithAnUnrelocatableEvent(t *testing.T, states string, outIn ...func(ws, state string) string) (code int, stdout, stderr, out string, forks []struct {
	Target  wf.FanoutTarget
	Outcome wf.FanoutOutcome
	Error   string
}) {
	t.Helper()
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	d := wf.New("per-fork")
	a := wf.NewAgent[pxA, string](d)
	// A command whose result names an untyped absolute source path as its
	// full output: a fork whose prefix holds it cannot be relocated.
	setup := wf.NewCommand[pxSetup](d, "true")
	b := wf.NewAgent[pxB, string](d)
	done := wf.NewTerminal[pxDone](d, wf.Success)
	a.Spec(wf.PromptText("a"), wf.Retry[pxA](wf.Const(1), a.To(done, "stop")))
	toSetup := a.To(setup, "a done")
	a.Route(func(string, wf.Ctx) wf.Edge[pxA] { return toSetup }, toSetup)
	setup.RouteExit(setup.To(b, "prepared"), setup.To(done, "red"))
	b.Spec(wf.PromptText("b"), wf.Retry[pxB](wf.Const(1), b.To(done, "stop")))
	finish := b.To(done, "b done")
	b.Route(func(string, wf.Ctx) wf.Edge[pxB] { return finish }, finish)
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, &unrelocatableOutput{filepath.Join(state, "worktrees", "legacy")}} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "perfork", RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].State != "pxA" {
		t.Fatalf("a: %+v %v", ins, err)
	}
	if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	if ins, err = e.Next(run, ""); err != nil || len(ins) != 1 || ins[0].State != "pxB" {
		t.Fatalf("b: %+v %v", ins, err)
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}
	out = filepath.Join(t.TempDir(), "batch")
	if len(outIn) > 0 {
		out = outIn[0](ws, state)
	}
	code, stdout, stderr = cliCall(cfg, "dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--out", out, "--states", states, "--parallel", "2", "--timeout", "5s", "--coordinator-cmd", `["sh","-c","printf 'done'"]`)
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if len(outIn) > 0 && err != nil {
		return code, stdout, stderr, out, nil
	}
	if err != nil {
		t.Fatalf("no report (exit %d): %s %s", code, stdout, stderr)
	}
	var report struct {
		Forks []struct {
			Target  wf.FanoutTarget
			Outcome wf.FanoutOutcome
			Error   string
		}
		Skipped []wf.FanoutSkip
	}
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatalf("report %s %v", data, err)
	}
	lastSkipped = report.Skipped
	return code, stdout, stderr, out, report.Forks
}

// lastSkipped is the Skipped of the last report fanoutWithAnUnrelocatableEvent read.
var lastSkipped []wf.FanoutSkip

// An offer whose journal prefix holds an event that cannot be relocated is
// left out of an implicit selection by the same check a fork makes, and
// named; the offers before that event still fork.
func TestFanoutRefusesOnlyTheForksThatCannotRelocate(t *testing.T) {
	code, stdout, stderr, _, forks := fanoutWithAnUnrelocatableEvent(t, "all")
	if len(forks) != 1 || forks[0].Target.LocalState != "pxA" || forks[0].Outcome.Status == "refused" || forks[0].Error != "" {
		t.Fatalf("forks %+v", forks)
	}
	if len(lastSkipped) != 1 || lastSkipped[0].State != "pxB" || !strings.Contains(lastSkipped[0].Reason, "untyped paths") {
		t.Errorf("skipped %+v", lastSkipped)
	}
	if !strings.Contains(stderr, "fanout: skipped pxB offer") {
		t.Errorf("stderr does not name the skip: %s", stderr)
	}
	if code != 8 {
		t.Errorf("exit %d: %s %s", code, stdout, stderr)
	}
}

// When every selected fork is refused, the batch still reports each refusal
// and exits 8.
func TestFanoutAllRefusedStillReports(t *testing.T) {
	code, stdout, stderr, _, forks := fanoutWithAnUnrelocatableEvent(t, "pxB")
	if len(forks) != 1 || forks[0].Outcome.Status != "refused" || len(forks[0].Outcome.Refused) != 1 {
		t.Fatalf("forks %+v", forks)
	}
	if code != 8 || !strings.Contains(stdout, "refused: ") {
		t.Errorf("exit %d, want 8 naming the refusal: %s %s", code, stdout, stderr)
	}
}

// A batch whose --out lies under a symbolic link into the source workspace
// is refused before anything is written, whether its forks would run or all
// be refused: the source is left as it was.
func TestFanoutRefusesAnOutputBehindALinkIntoTheSource(t *testing.T) {
	for _, states := range []string{"pxA", "pxB"} {
		t.Run(states, func(t *testing.T) {
			var ws string
			var before []string
			code, stdout, stderr, _, forks := fanoutWithAnUnrelocatableEvent(t, states, func(w, state string) string {
				ws = w
				link := filepath.Join(t.TempDir(), "into-source")
				if err := os.Symlink(w, link); err != nil {
					t.Fatal(err)
				}
				before = listTree(t, w)
				return filepath.Join(link, "batch")
			})
			if code == 0 || code == 8 || forks != nil || !strings.Contains(stdout+stderr, "symlink ancestor") {
				t.Errorf("exit %d, forks %v: %s %s", code, forks, stdout, stderr)
			}
			if after := listTree(t, ws); strings.Join(after, "\n") != strings.Join(before, "\n") {
				t.Errorf("the source workspace changed:\n%v\n%v", before, after)
			}
		})
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// liveSiblingsRun is a parallel run whose third offer was made while the
// first item's child was live (claimed, never finished): the offer the
// fanout sweep found unforkable in every parallel phase. rt is its snapshot
// adapter; fail, when set, makes captures fail from the third offer on.
func liveSiblingsRun(t *testing.T, fail bool) (*wf.Engine, *wf.Def, *wf.Adapters, wf.RunID, string, string, wf.Instruction) {
	t.Helper()
	return liveSiblingsRunWith(t, liveSiblings{fail: fail})
}

// liveSiblings varies liveSiblingsRun: lostCap is the engines' lost cap;
// refuseFirst has the live first child submit an invalid output first, so
// its attempt holds an open refusal.
type liveSiblings struct {
	fail, refuseFirst, moveDuring bool
	lostCap                       int
}

func liveSiblingsRunWith(t *testing.T, o liveSiblings) (*wf.Engine, *wf.Def, *wf.Adapters, wf.RunID, string, string, wf.Instruction) {
	t.Helper()
	fail := o.fail
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	rt := &snapshotRuntime{}
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	d := fanDef("live-siblings", chItems(1, 2, 3), 2, wf.AllSettled())
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "live", LostCap: o.lostCap,
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "boss"})
	if err != nil || len(agentsOf(ins)) != 2 {
		t.Fatalf("hand-out: %+v %v", ins, err)
	}
	first, second := agentsOf(ins)[0], agentsOf(ins)[1]
	if _, err := e.PromptFetchAs(run, first.Name, claimToken(t, first), codexChild("boss", "k1")); err != nil { // first stays live
		t.Fatal(err)
	}
	if o.refuseFirst {
		// An open refusal as the journal holds one (oldOpenRefusal): a
		// submission refused and not yet fixed.
		recs, _ := wf.ReadJournal(state, run)
		var last wf.JournalRecord
		for _, r := range recs {
			if r.Kind == wf.JournalInstructionIssued && r.Instance == first.Child {
				last = r
			}
		}
		var issued wf.InstructionIssuedData
		if err := json.Unmarshal(last.Data, &issued); err != nil {
			t.Fatal(err)
		}
		j := wf.FileJournal(state)
		unlock, err := j.Lock(run)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range []wf.JournalRecord{
			{Kind: wf.JournalSubmitted, Data: mustJSON(t, wf.SubmittedData{Token: issued.Token, Output: json.RawMessage(`"x"`)})},
			{Kind: wf.JournalSubmissionRefused, Class: wf.Refused, Data: mustJSON(t, wf.SubmissionRefusedData{Token: issued.Token, Reasons: []string{"undecodable"}, Left: 2, Fingerprint: "decode:x"})},
		} {
			r.Run, r.Instance, r.State, r.Attempt, r.DefHash, r.At = run, last.Instance, last.State, last.Attempt, last.DefHash, last.At
			if err := j.Append(r); err != nil {
				t.Fatal(err)
			}
		}
		unlock()
	}
	if _, err := e.PromptFetchAs(run, second.Name, claimToken(t, second), codexChild("boss", "k2")); err != nil {
		t.Fatal(err)
	}
	rt.fail, rt.moveDuring = fail, o.moveDuring
	if _, err := e.Done(run, second.Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(state, run)
	issued := 0
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued {
			issued++
		}
	}
	if issued != 3 { // the third was issued as the second settled, while the first was live
		t.Fatalf("issued %d", issued)
	}
	return e, d, reg, run, ws, state, first
}

// A fork is a crash-restart in a new segment: an offer made while
// a sibling's child was live is captured and forkable like any other. The
// plan takes every state's latest offer, substitutes and skips nothing, and
// in the fork the sibling, whose child is in the source world only, is lost
// as an orphan of the source coordinator (uncapped, no retry spent) and
// offered again, so the fork binds its target.
func TestFanoutForksOffersWithLiveSiblingsAsCrashRestart(t *testing.T) {
	e, d, reg, run, ws, state, first := liveSiblingsRun(t, false)
	plan, err := e.PlanFanout(run, "distinct")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(state, run)
	var lastIssue int64
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued {
			lastIssue = r.Seq
		}
	}
	if len(plan.Targets) != 1 || plan.Targets[0].IssueSeq != lastIssue || plan.Targets[0].Substituted != "" || len(plan.Skipped) != 0 {
		t.Fatalf("plan %+v (latest offer %d)", plan, lastIssue)
	}
	all, err := e.PlanFanout(run, "all")
	if err != nil || len(all.Targets) != 3 || len(all.Skipped) != 0 {
		t.Fatalf("all: %+v %v", all, err)
	}
	target := plan.Targets[0]
	fork, err := e.ForkSnapshot(run, target.SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	restored := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
	if err := restored.BindUntilUnitName(run, target.Unit); err != nil {
		t.Fatalf("bind %s: %v", target.Unit, err)
	}
	got, _ := wf.ReadJournal(fork.StateDir, run)
	lost := 0
	for _, r := range got[target.SnapshotSeq:] {
		if r.Kind == wf.JournalAttemptLost {
			var data wf.AttemptLostData
			if err := json.Unmarshal(r.Data, &data); err != nil || data.Name != first.Name || data.Cause != wf.LostCauseFork {
				t.Fatalf("lost %s %v", r.Data, err)
			}
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("the live sibling %s was lost %d times in the fork", first.Name, lost)
	}
	// Binding again (each later cycle) loses nothing more.
	if err := restored.BindUntilUnitName(run, target.Unit); err != nil {
		t.Fatal(err)
	}
	if again, _ := wf.ReadJournal(fork.StateDir, run); len(again) != len(got) {
		t.Fatalf("a later cycle appended %d records", len(again)-len(got))
	}
	if src, _ := wf.ReadJournal(state, run); len(src) != len(recs) {
		t.Fatal("the fork wrote the source journal")
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}
	code, stdout, stderr := cliCall(cfg, "dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--out", filepath.Join(t.TempDir(), "b"), "--parallel", "1", "--coordinator-cmd", `["true"]`, "--dry-run", "--states", "all")
	if code != 0 || strings.Contains(stderr, "skipped") || strings.Count(stdout, `"Target"`) != 3 {
		t.Fatalf("dry run %d:\n%s\n%s", code, stdout, stderr)
	}
}

// A capture with live children reads a world they may be writing: one that
// fails leaves that offer unforkable with its reason, never fails the
// source run's verb (a quiescent capture's failure still does).
func TestLiveCaptureFailureLeavesOfferUnforkable(t *testing.T) {
	e, _, _, run, _, state, _ := liveSiblingsRun(t, true)
	recs, _ := wf.ReadJournal(state, run)
	var last wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			last = wf.WorldSnapshotData{}
			if err := json.Unmarshal(r.Data, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !strings.HasPrefix(last.Unavailable, "capture with live children failed: ") {
		t.Fatalf("last checkpoint %+v", last)
	}
	plan, err := e.PlanFanout(run, "distinct")
	if err != nil || len(plan.Skipped) != 1 || !strings.HasPrefix(plan.Skipped[0].Reason, "capture with live children failed: ") || plan.Targets[0].Substituted == "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
}

type (
	revA     struct{}
	revSetup struct{}
	revDone  struct{}
)

// revisitRun is a run of state A visited twice: an untyped source path is
// journaled between the visits when straddle is set, before the first one
// otherwise.
func revisitRun(t *testing.T, straddle bool) (*wf.Engine, wf.MainConfig, wf.RunID, string, string, []int64) {
	t.Helper()
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	// The first state declared starts the run.
	d := wf.New("revisit")
	var setup *wf.Command[revSetup]
	var a *wf.Agent[revA, string]
	if straddle {
		a = wf.NewAgent[revA, string](d)
		setup = wf.NewCommand[revSetup](d, "true")
	} else {
		setup = wf.NewCommand[revSetup](d, "true")
		a = wf.NewAgent[revA, string](d)
	}
	done := wf.NewTerminal[revDone](d, wf.Success)
	a.Spec(wf.PromptText("a"), wf.Retry[revA](wf.Const(1), a.To(done, "stop")))
	toSetup, fin := a.To(setup, "again"), a.To(done, "done")
	a.Route(func(o string, _ wf.Ctx) wf.Edge[revA] {
		if o == "again" {
			return toSetup
		}
		return fin
	}, toSetup, fin)
	setup.RouteExit(setup.To(a, "prepared"), setup.To(done, "red"))
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, &unrelocatableOutput{filepath.Join(state, "worktrees", "legacy")}} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "revisit",
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var offers []int64
	for i, out := range []string{`"again"`, ""} {
		ins, err := e.Next(run, "")
		if err != nil || len(ins) != 1 || ins[0].State != "revA" {
			t.Fatalf("visit %d: %+v %v", i+1, ins, err)
		}
		recs, _ := wf.ReadJournal(state, run)
		for _, r := range recs {
			if r.Kind == wf.JournalInstructionIssued && (len(offers) == 0 || r.Seq > offers[len(offers)-1]) {
				offers = append(offers, r.Seq)
			}
		}
		if out != "" {
			if _, err := e.Submit(run, wf.Submission{Token: ins[0].Token, Output: json.RawMessage(out)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return e, wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}, run, ws, state, offers
}

// A state revisited after an untyped source path was journaled: its newest
// offer fails the fork's relocation check, so the plan, its dry run and the
// real fanout all use the earlier visit's offer, flag the substitution and
// name the offer left out.
func TestFanoutFallsBackPastAnOfferItCannotRelocate(t *testing.T) {
	e, cfg, run, ws, state, offers := revisitRun(t, true)
	if len(offers) != 2 {
		t.Fatalf("offers %v", offers)
	}
	plan, err := e.PlanFanout(run, "distinct")
	if err != nil || len(plan.Targets) != 1 || plan.Targets[0].IssueSeq != offers[0] ||
		!strings.Contains(plan.Targets[0].Substituted, fmt.Sprintf("latest offer %d:", offers[1])) || !strings.Contains(plan.Targets[0].Substituted, "untyped paths") {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].IssueSeq != offers[1] {
		t.Fatalf("skipped %+v", plan.Skipped)
	}
	args := []string{"dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--parallel", "1", "--timeout", "5s", "--coordinator-cmd", `["sh","-c","printf 'done'"]`}
	code, stdout, stderr := cliCall(cfg, append(args, "--out", filepath.Join(t.TempDir(), "dry"), "--dry-run")...)
	if code != 0 || !strings.Contains(stdout, fmt.Sprintf(`"IssueSeq":%d`, offers[0])) || !strings.Contains(stderr, fmt.Sprintf("offer %d:", offers[1])) {
		t.Fatalf("dry run %d:\n%s\n%s", code, stdout, stderr)
	}
	out := filepath.Join(t.TempDir(), "batch")
	_, stdout, stderr = cliCall(cfg, append(args, "--out", out)...)
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatalf("no report: %s %s", stdout, stderr)
	}
	var report struct {
		Forks []struct {
			Target  wf.FanoutTarget
			Outcome wf.FanoutOutcome
			Error   string
		}
		Skipped []wf.FanoutSkip
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Forks) != 1 || len(report.Skipped) != 1 {
		t.Fatalf("report %s %v", data, err)
	}
	f := report.Forks[0]
	if f.Target.IssueSeq != offers[0] || f.Target.Substituted == "" || f.Outcome.Status == "refused" || f.Error != "" {
		t.Fatalf("the fork %+v", f)
	}
	if !strings.Contains(stdout, "substituted: latest offer") {
		t.Errorf("stdout does not flag the substitution: %s", stdout)
	}
}

// When nothing selected implicitly can be forked, the batch still writes its
// report, naming every offer left out, and exits 8.
func TestFanoutAllSkippedStillReports(t *testing.T) {
	_, cfg, run, ws, state, offers := revisitRun(t, false)
	out := filepath.Join(t.TempDir(), "batch")
	code, stdout, stderr := cliCall(cfg, "dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--out", out, "--parallel", "1", "--timeout", "5s", "--coordinator-cmd", `["true"]`)
	data, err := os.ReadFile(filepath.Join(out, "report.json"))
	if err != nil {
		t.Fatalf("no report (exit %d): %s %s", code, stdout, stderr)
	}
	var report struct {
		Forks   []json.RawMessage
		Skipped []wf.FanoutSkip
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Forks) != 0 || len(report.Skipped) != len(offers) {
		t.Fatalf("report %s %v (offers %v)", data, err, offers)
	}
	if code != 8 {
		t.Errorf("exit %d: %s %s", code, stdout, stderr)
	}
}

type (
	lagStart struct{}
	lagDelay struct{}
	lagPrep  struct{}
	lagWork  struct{}
	lagOK    struct{}
)

// laggedChild is a map item whose Prep edge applies an external effect; item
// 1 takes one extra step first, so its Prep transition's effects are still
// pending when item 2's Work is offered (the fanout sweep's Code3b offer 88:
// TaskPrepare of one task pending while the other's Implement is offered).
func laggedChild() wf.Child[chItem, chRes] {
	d := wf.New("lag-item")
	start := wf.NewCompute[lagStart, int](d, func(c wf.Ctx) (int, error) { return wf.InputOf[chItem](c).N, nil })
	delay := wf.NewCompute[lagDelay, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	prep := wf.NewCompute[lagPrep, bool](d, func(wf.Ctx) (bool, error) { return true, nil })
	work := wf.NewAgent[lagWork, chOut](d)
	ok := wf.NewTerminal[lagOK](d, wf.Success)
	toPrep, toDelay := start.To(prep, "now"), start.To(delay, "later")
	start.Route(func(n int, _ wf.Ctx) wf.Edge[lagStart] {
		if n == 1 {
			return toDelay
		}
		return toPrep
	}, toPrep, toDelay)
	delayed := delay.To(prep, "ready")
	delay.Route(func(bool, wf.Ctx) wf.Edge[lagDelay] { return delayed }, delayed)
	prepared := prep.To(work, "prepared").With(wf.ExternalEffect{Adapter: "git", Op: "merge", Args: map[string]any{"branch": "feat"}, Key: wf.KeyOf("merge", "feat")})
	prep.Route(func(bool, wf.Ctx) wf.Edge[lagPrep] { return prepared }, prepared)
	work.Spec(wf.PromptText("do {{item}}", wf.ChildInput()), wf.Retry[lagWork](wf.Const(1), work.To(ok, "gave up")))
	done := work.To(ok, "ok")
	work.Route(func(chOut, wf.Ctx) wf.Edge[lagWork] { return done }, done)
	wf.Yield(ok, func(wf.Ctx) chRes { return chRes{} })
	return wf.Typed[chItem, chRes](d)
}

// An offer made while a sibling's transition effects are pending is
// forkable and binds: the sibling is outside the until-unit
// boundary and frozen in the fork, so its pending effects no longer refuse
// the bind, and the batch that one such target used to abort runs it.
func TestFanoutBindsOfferWithSiblingEffectsPending(t *testing.T) {
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
		t.Setenv(k, "")
	}
	ws, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "data"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	for _, rt := range []wf.AdapterRuntime{&snapshotRuntime{}, newWorld()} {
		if err := reg.Register(rt); err != nil {
			t.Fatal(err)
		}
	}
	d := wf.New("lagged")
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(1, 2), func(i chItem) string { return fmt.Sprint(i.N) }, laggedChild())
	merged := wf.NewTerminal[chMerged](d, wf.Success)
	partial := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(merged, "met"), m.To(partial, "partial"))
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: ws, StateRoot: state, Snapshots: true, RunID: "lag",
		RunDir: func(r wf.RunID) string { return filepath.Join(state, "runs", string(r)) }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "boss"}); err != nil {
		t.Fatal(err)
	}
	// The fixture's point: item 2's Work offer precedes item 1's Prep effects.
	recs, _ := wf.ReadJournal(state, run)
	var offer, effect int64
	for _, r := range recs {
		if r.Kind == wf.JournalInstructionIssued && strings.HasSuffix(r.Instance, "[2]") && offer == 0 {
			offer = r.Seq
		}
		if r.Kind == wf.JournalEffectIntent && strings.HasSuffix(r.Instance, "[1]") && r.State == "lagPrep" && effect == 0 {
			effect = r.Seq
		}
	}
	prepAccepted := false
	for _, r := range recs[:offer] {
		prepAccepted = prepAccepted || (r.Kind == wf.JournalAccepted && strings.HasSuffix(r.Instance, "[1]") && r.State == "lagPrep")
	}
	if offer == 0 || effect < offer || !prepAccepted {
		for _, r := range recs {
			t.Logf("%d %s %s %s", r.Seq, r.Kind, r.Instance, r.State)
		}
		t.Fatalf("fixture: item 2 offer %d, item 1 Prep effect %d, item 1 Prep accepted before the offer %v", offer, effect, prepAccepted)
	}
	plan, err := e.PlanFanout(run, "all")
	if err != nil {
		t.Fatal(err)
	}
	var target wf.FanoutTarget
	for _, tg := range plan.Targets {
		if tg.IssueSeq == offer {
			target = tg
		}
	}
	if target.IssueSeq == 0 || target.Refused != "" || len(plan.Skipped) != 0 {
		t.Fatalf("plan %+v", plan)
	}
	fork, err := e.ForkSnapshot(run, target.SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	restored := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
	if err := restored.BindUntilUnitName(run, target.Unit); err != nil {
		t.Fatalf("bind %s: %v", target.Unit, err)
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}
	out := filepath.Join(t.TempDir(), "b")
	code, stdout, stderr := cliCall(cfg, "dev", "fanout", string(run), "--state-dir", state, "--workspace", ws, "--out", out, "--parallel", "2", "--timeout", "20s", "--coordinator-cmd", `["true"]`, "--states", "all")
	if strings.Contains(stdout+stderr, "until-unit requires") || strings.Contains(stdout, "refused") {
		t.Fatalf("fanout %d:\n%s\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "report.json")); err != nil {
		t.Fatalf("no report (batch aborted): %v\n%s\n%s", err, stdout, stderr)
	}
}

// A --workspace that does not exist is named as such before any verb runs
// : validate used to run its effects there and report
// "fork/exec /usr/bin/bash: no such file or directory".
func TestMissingWorkspaceIsNamed(t *testing.T) {
	_, d, reg, run, ws, state, first := liveSiblingsRun(t, false)
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}, Adapters: reg}
	missing := filepath.Join(ws, "profiles")
	for _, args := range [][]string{{"validate", string(run), first.Name}, {"status", string(run)}} {
		code, _, stderr := cliCall(cfg, append(args, "--state-dir", state, "--workspace", missing)...)
		if code != 2 || !strings.Contains(stderr, "--workspace "+missing+": no such directory") || strings.Contains(stderr, "fork/exec") {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
	}
}

// forkedEngine is an engine over fork with the source's definition and
// options.
func forkedEngine(d *wf.Def, reg *wf.Adapters, fork wf.SnapshotFork, lostCap int) *wf.Engine {
	return wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, Snapshots: true, LostCap: lostCap,
		RunDir: func(r wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(r)) }}).(*wf.Engine)
}

// forkLatestOffer forks run at its latest offer's checkpoint.
func forkLatestOffer(t *testing.T, e *wf.Engine, run wf.RunID) (wf.FanoutTarget, wf.SnapshotFork) {
	t.Helper()
	plan, err := e.PlanFanout(run, "distinct")
	if err != nil || len(plan.Targets) != 1 || plan.Targets[0].Substituted != "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	fork, err := e.ForkSnapshot(run, plan.Targets[0].SnapshotSeq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	return plan.Targets[0], fork
}

// A fork's orphan is lost with the promised budget (the workflow run review P1 #3):
// a live sibling whose attempt holds an open refusal is not failed as that
// refusal (no retry spent, no route, no halt) on the fork's first cycle.
func TestForkOrphanWithOpenRefusalSpendsNoRetry(t *testing.T) {
	e, d, reg, run, _, _, first := liveSiblingsRunWith(t, liveSiblings{refuseFirst: true})
	target, fork := forkLatestOffer(t, e, run)
	restored := forkedEngine(d, reg, fork, 0)
	if err := restored.BindUntilUnitName(run, target.Unit); err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(fork.StateDir, run)
	var lost []string
	for _, r := range recs[target.SnapshotSeq:] {
		switch r.Kind {
		case wf.JournalRefused, wf.JournalRetried, wf.JournalTransition, wf.JournalBackoffScheduled:
			t.Fatalf("the fork's orphan took its refusal's route: %s %s %s", r.Kind, r.Instance, r.Data)
		case wf.JournalAttemptLost:
			var data wf.AttemptLostData
			if err := json.Unmarshal(r.Data, &data); err != nil {
				t.Fatal(err)
			}
			lost = append(lost, data.Name+" "+data.Cause)
		}
	}
	if len(lost) != 1 || lost[0] != first.Name+" "+wf.LostCauseFork {
		t.Fatalf("lost %v", lost)
	}
}

// A fork's orphan loss does not count toward the lost cap: next to the cap,
// the reissued unit's own first loss is an ordinary one, not the cap.
func TestForkOrphanDoesNotCountTowardLostCap(t *testing.T) {
	e, d, reg, run, _, _, _ := liveSiblingsRunWith(t, liveSiblings{lostCap: 2})
	_, fork := forkLatestOffer(t, e, run)
	restored := forkedEngine(d, reg, fork, 2)
	ins, err := restored.HandOutAs(run, 2, wf.Coordinator{Harness: "codex", Session: "fork-boss"})
	if err != nil {
		t.Fatal(err)
	}
	var again wf.Instruction
	for _, in := range agentsOf(ins) {
		if strings.HasPrefix(in.Name, "chFan/1/") {
			again = in
		}
	}
	if again.Name != "chFan/1/chWork#2" {
		t.Fatalf("the orphan was not offered again: %+v", agentsOf(ins))
	}
	if _, err := restored.PromptFetchAs(run, again.Name, claimToken(t, again), codexChild("fork-boss", "f1")); err != nil {
		t.Fatal(err)
	}
	if err := restored.HeartbeatAs(run, wf.HeartbeatTarget{Name: again.Name}, false, wf.Coordinator{Harness: "codex", Session: "fork-boss"}); err != nil {
		t.Fatal(err)
	}
	recs, _ := wf.ReadJournal(fork.StateDir, run)
	var data wf.AttemptLostData
	for _, r := range recs {
		if r.Kind == wf.JournalAttemptLost {
			data = wf.AttemptLostData{}
			_ = json.Unmarshal(r.Data, &data)
		}
	}
	if data.Name != again.Name || data.Cause != wf.LostCauseDied {
		t.Fatalf("the reissued unit's first loss was %+v, want an ordinary died loss below the cap", data)
	}
}

// A live capture is a consistent cut or none (the workflow run review P1 #1): a
// capture during which the world moved (a child committed or wrote the
// tracker between the adapters' reads) succeeds as a capture but is
// recorded unavailable, never as a checkpoint no crash could leave.
func TestLiveCaptureOfAMovingWorldIsUnavailable(t *testing.T) {
	e, _, _, run, _, state, _ := liveSiblingsRunWith(t, liveSiblings{moveDuring: true})
	recs, _ := wf.ReadJournal(state, run)
	var last wf.WorldSnapshotData
	for _, r := range recs {
		if r.Kind == wf.JournalWorldSnapshot {
			last = wf.WorldSnapshotData{}
			if err := json.Unmarshal(r.Data, &last); err != nil {
				t.Fatal(err)
			}
		}
	}
	if last.Unavailable != "world changed during capture: world" {
		t.Fatalf("last checkpoint %+v", last)
	}
	plan, err := e.PlanFanout(run, "distinct")
	if err != nil || len(plan.Skipped) != 1 || plan.Skipped[0].Reason != "world changed during capture: world" {
		t.Fatalf("plan %+v %v", plan, err)
	}
}
