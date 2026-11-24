package wf_test

// The integration sweep of the subagent driver: each main flow runs through
// wf.CLI in single-registration mode over a FileJournal, the way ./super is
// driven. Flows that need the production binary, git and bd are covered by the
// client's own tests; these tests need none of them.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// drv is one single-registration binary over its own state directory.
type drv struct {
	t     *testing.T
	cfg   wf.MainConfig
	state string
}

func newDrv(t *testing.T, def *wf.Def) *drv {
	t.Helper()
	cfg := wf.MainConfig{Adapters: clConfig().Adapters, Defs: []wf.Registration{{Def: def}}, Binary: "super"}
	return &drv{t: t, cfg: cfg, state: filepath.Join(t.TempDir(), "state")}
}

func (d *drv) cli(args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := fixtureCLI(d.cfg, args, &o, &er)
	return code, o.String(), er.String()
}

// wf runs `wf <verb> <args>` against the state directory.
func (d *drv) wf(args ...string) (int, string, string) {
	return d.cli(append(append([]string{"wf"}, args...), "--state-dir", d.state)...)
}

func (d *drv) must(args ...string) string {
	d.t.Helper()
	code, out, er := d.wf(args...)
	if code != 0 {
		d.t.Fatalf("wf %v exited %d: %s", args, code, er)
	}
	return out
}

func (d *drv) start(id string) {
	d.t.Helper()
	code, out, er := d.cli("run", "--state-dir", d.state, "--workspace", d.t.TempDir(), "--run-id", id)
	if code != 0 || strings.TrimSpace(out) != id {
		d.t.Fatalf("run = %d %q %s, want the id %s", code, out, er, id)
	}
}

func (d *drv) handOut(n string) []wf.Instruction {
	d.t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(d.must("next", n, "--json")), &ins); err != nil {
		d.t.Fatal(err)
	}
	return ins
}

func (d *drv) status() wf.RunStatus {
	d.t.Helper()
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(d.must("status", "--json")), &st); err != nil {
		d.t.Fatal(err)
	}
	return st
}

func (d *drv) outFile(content string) string {
	d.t.Helper()
	f := filepath.Join(d.t.TempDir(), "out.json")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		d.t.Fatal(err)
	}
	return f
}

// kinds lists the journal's record kinds.
func (d *drv) kinds() []string {
	d.t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(d.must("journal")), "\n") {
		var rec wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			d.t.Fatalf("journal line %q: %v", line, err)
		}
		out = append(out, rec.Kind.String())
	}
	return out
}

// reapDetached kills the process group of every detached effect the run started.
func (d *drv) reapDetached() {
	_, out, _ := d.wf("journal")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec wf.JournalRecord
		var s wf.EffectStartedData
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Kind == wf.JournalEffectStarted && json.Unmarshal(rec.Data, &s) == nil && s.Pgid > 0 {
			syscall.Kill(-s.Pgid, syscall.SIGKILL)
		}
	}
}

func unitNames(ins []wf.Instruction) []string {
	var out []string
	for _, in := range ins {
		out = append(out, in.Name)
	}
	return out
}

// TestPagedRunEndToEnd: next <n>, started, prompt, done, next --wait and status, to
// the end of a three-unit run that only two units at a time may occupy.
func TestPagedRunEndToEnd(t *testing.T) {
	d := newDrv(t, fanDef("paged-e2e", chItems(1, 2, 3), 2, wf.AllSettled()))
	d.start("pg1")

	first := d.handOut("2")
	if len(first) != 2 || first[0].Name == "" || first[0].Prompt != "" || first[0].Token != "" {
		t.Fatalf("wf next 2 = %+v, want two paged units with a name and no prompt or token", first)
	}
	if again := d.handOut("1"); len(again) != 0 {
		t.Fatalf("wf next 1 with Parallel(2) full = %v, want none", unitNames(again))
	}

	for i, in := range first {
		if i == 0 {
			d.must("prompt", in.Name) // The worker may fetch before its coordinator attaches metadata.
		}
		d.must("dev", "started", in.Name, "--session", "s1", "--agent", "a"+in.Name, "--harness", "claude")
		prompt := withBody(t, d.must("prompt", in.Name))
		if !strings.Contains(prompt, "do ") || !strings.Contains(prompt, " wf done --state-dir "+d.state+" "+in.Name) {
			t.Fatalf("prompt of unit %d lacks the task or the done line:\n%s", i, prompt)
		}
	}
	st := d.status()
	if len(st.Units) != 3 || st.Phase != wf.RunRunning {
		t.Fatalf("status mid-run = %s with %d units, want running with the 2 handed units and the 1 item held by Parallel in view", st.Phase, len(st.Units))
	}
	if heldRows(st, "held") == nil || len(heldRows(st, "held")) != 1 {
		t.Errorf("status mid-run units = %+v, want one waiting unit noted held", st.Units)
	}
	if txt := d.must("status", "--full"); !strings.Contains(txt, first[0].Name) {
		t.Errorf("the status line view does not list %s:\n%s", first[0].Name, txt)
	}

	d.must("done", first[0].Name, "--output", writeAllocated(t, first[0].Output, `{"OK":true}`), "--usage", "tokens=7")
	d.must("done", first[1].Name, "--output", writeAllocated(t, first[1].Output, `{"OK":true}`))
	// next --wait hands out the third unit, which the two dones freed a slot
	// for, and prints its spawn line; its child fetches the prompt by claim.
	waited := strings.TrimSpace(d.must("next", "1", "--timeout", "5s"))
	name, _, ok := strings.Cut(strings.TrimPrefix(waited, "start a child agent for "), " with ")
	if !strings.HasPrefix(waited, "start a child agent for ") || !ok || strings.Count(waited, "start a child agent for ") != 1 || slices.Contains(unitNames(first), name) {
		t.Fatalf("next 1 --wait after two dones =\n%s\nwant one spawn line for the third unit", waited)
	}
	prompt := withBody(t, d.must("prompt", "--claim", claimToken(t, wf.Instruction{Brief: waited})))
	_, output, _ := strings.Cut(prompt, "allocated output file: ")
	output, _, _ = strings.Cut(output, "\n")
	d.must("dev", "started", name, "--session", "s1", "--agent", "a3")
	d.must("done", name, "--output", writeAllocated(t, strings.TrimSpace(output), `{"OK":true}`))

	st = d.status()
	if st.Phase != wf.RunEnded || st.Terminal != "chMerged" || st.Outcome != wf.Success {
		t.Fatalf("final status = %s/%s/%s, want ended at chMerged with success", st.Phase, st.Terminal, st.Outcome)
	}
	if st.Progress.Done == 0 {
		t.Errorf("progress = %+v, want settled units counted", st.Progress)
	}
	for _, k := range []string{"run_started", "handed_out", "attempt_started", "prompt_fetched", "submitted", "run_ended"} {
		if !slices.Contains(d.kinds(), k) {
			t.Errorf("journal lacks a %s record", k)
		}
	}
}

// TestCoordinatorReplacedMidRun: session s1 hands out and claims two units,
// finishes one and dies; s2 resumes by run id, declares s1 dead and finishes
// the reissued unit. The finished unit is not redone and the orphan's late done
// is rejected.
func TestCoordinatorReplacedMidRun(t *testing.T) {
	d := newDrv(t, fanDef("replaced-e2e", chItems(1, 2), 2, wf.AllSettled()))
	d.start("rp1")

	ins := d.handOut("2")
	if len(ins) != 2 {
		t.Fatalf("wf next 2 = %v, want two units", unitNames(ins))
	}
	d.must("dev", "started", ins[0].Name, "--session", "s1", "--agent", "a1")
	d.must("dev", "started", ins[1].Name, "--session", "s1", "--agent", "a2")
	d.must("done", ins[0].Name, "--output", writeAllocated(t, ins[0].Output, `{"OK":true}`))
	// s1 dies here, with ins[1] claimed and running.

	boot := d.cli
	code, out, er := boot("next", "--state-dir", d.state, "--run-id", "rp1")
	// The bootstrap names no earlier session: a new session's next clears
	// what s1 left (B4); heartbeat died --session stays a manual override.
	if code != 0 || !strings.Contains(out, "run: rp1") || strings.Contains(out, "session s1") {
		t.Fatalf("the replacement's bootstrap = %d %s\n%s, want run rp1 and no session list", code, er, out)
	}
	d.must("heartbeat", "died", "--session", "s1")
	if code, _, _ := d.wf("done", ins[1].Name, "--output", d.outFile(`{"OK":true}`)); code != 4 {
		t.Fatalf("the orphan's late done exited %d, want 4", code)
	}
	if !slices.Contains(d.kinds(), "attempt_lost") {
		t.Fatalf("heartbeat died --session s1 journaled no attempt_lost: %v", d.kinds())
	}

	re := d.handOut("2")
	if len(re) != 1 || re[0].Name == ins[1].Name || re[0].Attempt != 2 {
		t.Fatalf("wf next 2 after the loss = %+v, want one reissue at attempt 2 and not the finished unit", re)
	}
	d.must("dev", "started", re[0].Name, "--session", "s2", "--agent", "b1")
	if st := d.status(); st.Phase == wf.RunEnded {
		t.Fatalf("the run ended before the reissued unit finished: %+v", st)
	}
	d.must("done", re[0].Name, "--output", writeAllocated(t, re[0].Output, `{"OK":true}`))
	if st := d.status(); st.Phase != wf.RunEnded || st.Terminal != "chMerged" {
		t.Fatalf("final status = %s/%s, want ended at chMerged", st.Phase, st.Terminal)
	}
}

type (
	dsSuite struct{}
	dsOK    struct{}
	dsFail  struct{}
	dsExit  int
)

// dsDef is one keyed, idempotent shell command that runs detached.
func dsDef(argv ...string) *wf.Def {
	d := wf.New("detached-e2e")
	exits := wf.NewEnum(d, "ds-exit", dsExit(0), dsExit(1))
	cmd := wf.NewCommand[dsSuite](d, argv...)
	ok := wf.NewTerminal[dsOK](d, wf.Success)
	fail := wf.NewTerminal[dsFail](d, wf.Stopped)
	cmd.Key("detached", "e2e").Idempotent().Detached(time.Minute)
	cmd.RouteCases(wf.Cases[dsSuite, wf.RunResult](wf.ExitAs[dsExit], exits,
		wf.On(dsExit(0), cmd.To(ok, "passes")),
		wf.On(dsExit(1), cmd.To(fail, "fails"))))
	return d
}

// TestDetachedSuiteStepEndToEnd: a detached suite step runs through the real
// shell runner and the binary's hidden verb: next starts it and answers wait,
// next --wait returns when it settles, and the result file stays under the run.
func TestDetachedSuiteStepEndToEnd(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reg := wf.NewAdapters()
	if err := reg.Register(shell.Runtime(shell.Config{})); err != nil {
		t.Fatal(err)
	}
	d := newDrv(t, dsDef("sh", "-c", "sleep 0.3; echo suite-output"))
	d.cfg.Adapters = reg
	d.cfg.EffectRunner = shell.DetachedRunner(bin)
	d.cfg.Extra = map[string]func([]string, io.Writer, io.Writer) int{shell.DetachedVerb: shell.RunDetached}
	d.start("ds1")

	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(d.must("next", "all", "--json")), &ins); err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionWait {
		t.Fatalf("wf next = %+v (%v), want one wait while the suite runs", ins, err)
	}
	started := d.kinds()
	if !slices.Contains(started, "effect_started") {
		t.Fatalf("journal %v lacks effect_started: the command did not detach", started)
	}
	t.Cleanup(func() { d.reapDetached() })

	deadline := time.Now().Add(30 * time.Second)
	for d.status().Phase != wf.RunEnded {
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end; status %+v", d.status())
		}
		if code, _, er := d.wf("next", "all", "--timeout", "2s"); code != 0 && code != 7 {
			t.Fatalf("wf next all --wait exited %d: %s", code, er)
		}
		if code, _, er := d.wf("next", "all", "--json"); code != 0 && code != 7 {
			t.Fatalf("wf next exited %d: %s", code, er)
		}
	}
	if code, _, er := d.wf("next", "all", "--json"); code != 7 {
		t.Fatalf("wf next on the ended run exited %d (%s), want 7", code, er)
	}
	if st := d.status(); st.Terminal != "dsOK" || st.Outcome != wf.Success {
		t.Fatalf("final status = %s/%s, want ended at dsOK from exit 0", st.Phase, st.Terminal)
	}
	files, _ := filepath.Glob(filepath.Join(d.state, "runs", "ds1", "effects", "*.json"))
	if len(files) != 1 {
		t.Fatalf("result files = %v, want one under <state-dir>/runs/ds1/effects", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || !strings.Contains(string(raw), "suite-output") {
		t.Fatalf("result file = %q, %v, want the suite's stdout", raw, err)
	}
	if n := strings.Count(strings.Join(d.kinds(), " "), "effect_started"); n != 1 {
		t.Errorf("effect_started records = %d, want exactly 1", n)
	}
}

type (
	bwMake struct{}
	bwOK   struct{}
	bwExit int
)

// blkWaitDef is one inline command whose only routed exit is 0; the command
// exits 3, so the step halts with no unit (route-off) and the run blocks.
func blkWaitDef() *wf.Def {
	d := wf.New("blocked-wait")
	exits := wf.NewEnum(d, "bw-exit", bwExit(0))
	cmd := wf.NewCommand[bwMake](d, "sh", "-c", "exit 3")
	ok := wf.NewTerminal[bwOK](d, wf.Success)
	cmd.RouteCases(wf.Cases[bwMake, wf.RunResult](wf.ExitAs[bwExit], exits,
		wf.On(bwExit(0), cmd.To(ok, "passes"))))
	return d
}

// TestBlockedReachesWaitEndToEnd: a halted command step reaches the
// coordinator through the CLI: wf next --wait returns a "blocked <state>" line.
func TestBlockedReachesWaitEndToEnd(t *testing.T) {
	reg := wf.NewAdapters()
	if err := reg.Register(shell.Runtime(shell.Config{})); err != nil {
		t.Fatal(err)
	}
	d := newDrv(t, blkWaitDef())
	d.cfg.Adapters = reg
	d.start("bw1")

	began := time.Now()
	out := d.must("next", "all", "--run-id", "bw1", "--timeout", "10s")
	if time.Since(began) >= 10*time.Second {
		t.Errorf("wait took %s: it returned on its timeout, not on the blocked event", time.Since(began))
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "blocked ") {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "bwMake") {
		t.Fatalf("wait output has no blocked line naming bwMake:\n%s", out)
	}
	st := d.status()
	if st.Phase == wf.RunEnded {
		t.Errorf("status phase = %s, want the run not ended", st.Phase)
	}
	if len(st.Instances) != 1 || st.Instances[0].Phase != "blocked" || st.Instances[0].Reason == "" {
		t.Errorf("instances = %+v, want one halted instance with a reason", st.Instances)
	}
}
