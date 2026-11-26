package shell

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/internal/testgit"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// TestMain makes the test binary its own detached launcher: DetachedRunner
// re-executes it as the hidden verb. The stage hooks below let a test slow or
// fail either stage of a launch.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == DetachedVerb {
		stageHooks(os.Args[2:])
		os.Exit(RunDetached(os.Args[2:], os.Stdout, os.Stderr))
	}
	testgit.Isolate()
	os.Exit(m.Run())
}

// Environment hooks for the two stages of a launch, both of which run as this
// test binary. The --spawn intermediate sleeps envSpawnDelay after starting the
// runner and before printing its pid. The runner writes its pid to
// envRunnerPidfile, then exits 3 when envRunnerExit is set, else sleeps
// envRunnerDelay, all before its proc record.
const (
	envSpawnDelay    = "WF_TEST_SPAWN_DELAY"
	envRunnerPidfile = "WF_TEST_RUNNER_PIDFILE"
	envRunnerExit    = "WF_TEST_RUNNER_EXIT"
	envRunnerDelay   = "WF_TEST_RUNNER_DELAY"
)

func stageHooks(args []string) {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--spawn" {
			if d, err := time.ParseDuration(os.Getenv(envSpawnDelay)); err == nil {
				spawnHook = func() { time.Sleep(d) }
			}
			return
		}
	}
	if f := os.Getenv(envRunnerPidfile); f != "" {
		_ = os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o644)
	}
	if os.Getenv(envRunnerExit) != "" {
		os.Exit(3)
	}
	if d, err := time.ParseDuration(os.Getenv(envRunnerDelay)); err == nil {
		time.Sleep(d)
	}
}

func testBin(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func reqIn(t *testing.T, dir string, argv ...string) wf.DetachedRequest {
	t.Helper()
	return wf.DetachedRequest{
		Argv: argv, Dir: t.TempDir(),
		ResultPath: filepath.Join(dir, "effects", "7.json"),
		ProcPath:   filepath.Join(dir, "effects", "7.proc"),
		LogPath:    filepath.Join(dir, "effects", "7.log"),
	}
}

func startSh(t *testing.T, script string) (wf.EffectRunner, wf.DetachedProcess, wf.DetachedRequest) {
	t.Helper()
	r := DetachedRunner(testBin(t))
	req := reqIn(t, t.TempDir(), "sh", "-c", script)
	p, err := r.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-p.Pgid, syscall.SIGKILL) })
	return r, p, req
}

func waitReport(t *testing.T, r wf.EffectRunner, p wf.DetachedProcess, req wf.DetachedRequest, want wf.DetachedStatus) wf.DetachedReport {
	t.Helper()
	var rep wf.DetachedReport
	for i := 0; i < 500; i++ {
		var err error
		rep, err = r.Status(p, req)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Status == want {
			return rep
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status = %v after 10s, want %v", rep.Status, want)
	return rep
}

func waitStatus(t *testing.T, r wf.EffectRunner, p wf.DetachedProcess, req wf.DetachedRequest, want wf.DetachedStatus) wf.RunResult {
	t.Helper()
	return waitReport(t, r, p, req, want).Result
}

func TestDetachedRunnerWritesResultAtomically(t *testing.T) {
	r, p, req := startSh(t, "echo hi; exit 3")
	got := waitStatus(t, r, p, req, wf.DetachedPresent)
	if got.Exit != 3 || got.Stdout != "hi\n" {
		t.Fatalf("result = %+v, want exit 3 and stdout hi", got)
	}
	if _, err := os.Stat(req.ResultPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file remains (stat err %v)", err)
	}
}

func TestDetachedStartFailureIsExit127(t *testing.T) {
	r := DetachedRunner(testBin(t))
	req := reqIn(t, t.TempDir(), "/no/such/binary")
	p, err := r.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, r, p, req, wf.DetachedPresent)
	if got.Exit != 127 || got.Stderr == "" {
		t.Fatalf("result = %+v, want exit 127 with the start error", got)
	}
}

func TestRunDetachedUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := RunDetached([]string{"--result", "x"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("code %d stderr %q, want 2 and usage", code, errb.String())
	}
}

func TestDetachedSurvivesParentReturn(t *testing.T) {
	r, p, req := startSh(t, "sleep 1")
	// Start returned without waiting for the command.
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("status = %v, %v right after Start, want running", rep.Status, err)
	}
	waitStatus(t, r, p, req, wf.DetachedPresent)
}

func TestGroupEmptyAfterExit(t *testing.T) {
	r, p, req := startSh(t, "true")
	waitStatus(t, r, p, req, wf.DetachedPresent)
	for i := 0; i < 250; i++ {
		if empty, err := r.GroupEmpty(p, req); err != nil {
			t.Fatal(err)
		} else if empty {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("process group still not empty 5s after the command exited")
}

func TestInlineChildGetsPdeathsig(t *testing.T) {
	attr := procAttr()
	if !attr.Setpgid {
		t.Fatal("inline command lost its own process group")
	}
	checkPdeathsig(t, attr)
}

type (
	cRun  struct{}
	cOK   struct{}
	cFail struct{}
)

type cExit int

func detachedDef(max time.Duration, argv ...string) *wf.Def {
	d := wf.New("detachedshell")
	exits := wf.NewEnum(d, "c-exit", cExit(0), cExit(1), cExit(124))
	cmd := wf.NewCommand[cRun](d, argv...)
	ok := wf.NewTerminal[cOK](d, wf.Success)
	fail := wf.NewTerminal[cFail](d, wf.Stopped)
	cmd.Key("detached", "shell").Idempotent().Detached(max)
	cmd.RouteCases(wf.Cases[cRun, wf.RunResult](wf.ExitAs[cExit], exits,
		wf.On(cExit(0), cmd.To(ok, "passes")),
		wf.On(cExit(1), cmd.To(fail, "fails")),
		wf.On(cExit(124), cmd.To(fail, "timed out"))))
	return d
}

// engineOver is a fresh Engine over the file journal in stateDir, as one CLI
// call builds it.
func engineOver(t *testing.T, stateDir string, d *wf.Def, r wf.EffectRunner) wf.Executor {
	t.Helper()
	return engineOverWith(t, stateDir, d, r, nil)
}

// engineOverWith is engineOver with the options adjusted by opt (nil: none).
func engineOverWith(t *testing.T, stateDir string, d *wf.Def, r wf.EffectRunner, opt func(*wf.EngineOptions)) wf.Executor {
	t.Helper()
	reg := wf.NewAdapters()
	if err := reg.Register(Runtime(Config{})); err != nil {
		t.Fatal(err)
	}
	o := wf.EngineOptions{
		Defs:         []*wf.Def{d},
		EffectRunner: r,
		RunDir:       func(run wf.RunID) string { return filepath.Join(stateDir, "runs", string(run)) },
	}
	if opt != nil {
		opt(&o)
	}
	return wf.NewEngine(wf.FileJournal(stateDir), reg, o)
}

func kinds(t *testing.T, stateDir string, run wf.RunID, k wf.JournalKind) []wf.JournalRecord {
	t.Helper()
	j := wf.FileJournal(stateDir)
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	recs, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.JournalRecord
	for _, r := range recs {
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

func TestDetachedRecoveredAfterCLIKill(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "sleep 1; echo done")
	r := DetachedRunner(testBin(t))

	first := engineOver(t, stateDir, d, r)
	run, err := first.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := first.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionWait {
		t.Fatalf("first Next = %+v, %v, want one wait", ins, err)
	}
	started := kinds(t, stateDir, run, wf.JournalEffectStarted)
	if len(started) != 1 {
		t.Fatalf("effect_started records = %d, want 1", len(started))
	}
	t.Cleanup(func() {
		var s wf.EffectStartedData
		if json.Unmarshal(started[0].Data, &s) == nil {
			syscall.Kill(-s.Pgid, syscall.SIGKILL)
		}
	})
	first = nil // the CLI call that launched it is gone

	second := engineOver(t, stateDir, d, r)
	ins, err = second.Next(run, "")
	if err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionWait {
		t.Fatalf("second Next = %+v, %v, want wait while the command runs", ins, err)
	}
	var st wf.RunStatus
	for i := 0; i < 500; i++ {
		if _, err := second.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, err = second.Status(run); err != nil {
			t.Fatal(err)
		}
		if st.Phase == wf.RunEnded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cOK" {
		t.Fatalf("status = %s/%s, want ended/cOK routed from exit 0", st.Phase, st.Terminal)
	}
	res := kinds(t, stateDir, run, wf.JournalEffectResult)
	var data wf.EffectResultData
	var rr wf.RunResult
	if len(res) != 1 || json.Unmarshal(res[0].Data, &data) != nil || json.Unmarshal(data.Result, &rr) != nil || rr.Stdout != "done\n" {
		t.Fatalf("effect_result = %v (%+v), want one carrying the command's stdout", res, rr)
	}
	if n := len(kinds(t, stateDir, run, wf.JournalEffectStarted)); n != 1 {
		t.Fatalf("effect_started records = %d, want exactly 1: recovery reads the file", n)
	}
}

func TestDetachedMaxDurationSettlesRed(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(500*time.Millisecond, "sleep", "30")
	r := DetachedRunner(testBin(t))
	e := engineOver(t, stateDir, d, r)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	started := kinds(t, stateDir, run, wf.JournalEffectStarted)
	if len(started) != 1 {
		t.Fatalf("effect_started records = %d, want 1", len(started))
	}
	var s wf.EffectStartedData
	if err := json.Unmarshal(started[0].Data, &s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-s.Pgid, syscall.SIGKILL) })

	var st wf.RunStatus
	for i := 0; i < 250; i++ {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if in.Kind != wf.InstructionWait && in.Kind != wf.InstructionEnded {
				t.Fatalf("instruction %+v: a timed-out detached effect gets no manual verification", in)
			}
		}
		if st, err = e.Status(run); err != nil {
			t.Fatal(err)
		}
		if st.Phase == wf.RunEnded {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cFail" {
		t.Fatalf("status = %s/%s, want ended/cFail via the exit-124 case", st.Phase, st.Terminal)
	}
	res := kinds(t, stateDir, run, wf.JournalEffectResult)
	var data wf.EffectResultData
	var rr wf.RunResult
	if len(res) != 1 || json.Unmarshal(res[0].Data, &data) != nil || data.Outcome != "timed out" ||
		json.Unmarshal(data.Result, &rr) != nil || rr.Exit != 124 {
		t.Fatalf("effect_result = %v, want one timed-out exit-124 result", res)
	}
	if empty, err := r.GroupEmpty(wf.DetachedProcess{Pid: s.Pid, Pgid: s.Pgid, StartTicks: s.StartTicks}, wf.DetachedRequest{}); err != nil || !empty {
		t.Fatalf("GroupEmpty = %v, %v after the maximum duration, want true", empty, err)
	}
}

func TestMainWithPutsResultsUnderRunDir(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "echo main")
	cfg := wf.MainConfig{
		Defs:         []wf.Registration{{Def: d}},
		Adapters:     wf.NewAdapters(),
		EffectRunner: DetachedRunner(testBin(t)),
	}
	var out, errb bytes.Buffer
	if code := wf.CLI(cfg, []string{"run", "detachedshell", "--run-id", "r1", "--state-dir", stateDir}, &out, &errb); code != 0 {
		t.Fatalf("run exit %d: %s%s", code, out.String(), errb.String())
	}
	out.Reset()
	if code := wf.CLI(cfg, []string{"next", "r1", "all", "--state-dir", stateDir, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("next exit %d: %s%s", code, out.String(), errb.String())
	}
	var files []string
	for i := 0; i < 250 && len(files) == 0; i++ {
		files, _ = filepath.Glob(filepath.Join(stateDir, "runs", "r1", "effects", "*.json"))
		time.Sleep(20 * time.Millisecond)
	}
	if len(files) != 1 {
		t.Fatalf("result files = %v, want one under <state-dir>/runs/r1/effects", files)
	}
}

func TestDetachedProcRecordBeforeCommand(t *testing.T) {
	copyTo := filepath.Join(t.TempDir(), "copy")
	r := DetachedRunner(testBin(t))
	req := reqIn(t, t.TempDir(), "sh", "-c", "cat \"$0\" > "+copyTo)
	req.Argv = []string{"sh", "-c", "cat " + req.ProcPath + " > " + copyTo}
	p, err := r.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, p, req, wf.DetachedPresent)
	b, err := os.ReadFile(copyTo)
	if err != nil {
		t.Fatal(err)
	}
	var rec procRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("copy %q: %v", b, err)
	}
	if rec.Pid != p.Pid || rec.Pgid != p.Pgid || rec.StartTicks != p.StartTicks {
		t.Fatalf("record %+v seen by the command, Start returned %+v", rec, p)
	}
	if p.Pid != p.Pgid || p.StartTicks == 0 {
		t.Fatalf("process %+v, want Pid == Pgid and start ticks > 0", p)
	}
	if got, ok, err := r.Launched(req); err != nil || !ok || got != p {
		t.Fatalf("Launched = %+v, %v, %v, want %+v", got, ok, err, p)
	}
}

func TestLaunchedAbsentIsNotOK(t *testing.T) {
	req := reqIn(t, t.TempDir(), "true")
	if _, ok, err := DetachedRunner(testBin(t)).Launched(req); err != nil || ok {
		t.Fatalf("Launched = %v, %v, want absent", ok, err)
	}
}

func TestDeadLeaderLiveChildIsDeadLive(t *testing.T) {
	r, p, req := startSh(t, "sleep 30 & wait")
	if err := syscall.Kill(p.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitReport(t, r, p, req, wf.DetachedDeadLive)
	if empty, err := r.GroupEmpty(p, req); err != nil || empty {
		t.Fatalf("GroupEmpty = %v, %v with a live child, want false", empty, err)
	}
	if err := r.Terminate(p, req); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		if empty, _ := r.GroupEmpty(p, req); empty {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("group not empty after Terminate through -pgid with the leader gone")
}

func TestRecycledPgidIsEmpty(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	ticks, _, ok := procStat(cmd.Process.Pid)
	if !ok {
		t.Skip("no /proc")
	}
	p := wf.DetachedProcess{Pid: cmd.Process.Pid, Pgid: cmd.Process.Pid, StartTicks: ticks + 1}
	r := DetachedRunner(testBin(t))
	if empty, err := r.GroupEmpty(p, wf.DetachedRequest{}); err != nil || !empty {
		t.Fatalf("GroupEmpty = %v, %v for another launch's group, want true", empty, err)
	}
	if err := r.Terminate(p, wf.DetachedRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, state, ok := procStat(p.Pid); !ok || state == 'Z' {
		t.Fatalf("sleep state %q (ok %v) after Terminate of a recycled pgid, want untouched", state, ok)
	}
	rep, err := r.Status(p, wf.DetachedRequest{ResultPath: filepath.Join(t.TempDir(), "none")})
	if err != nil || rep.Status != wf.DetachedDead {
		t.Fatalf("status = %v, %v, want dead", rep.Status, err)
	}
}

func TestStatusRereadsResultAfterNegativeProbe(t *testing.T) {
	req := reqIn(t, t.TempDir(), "true")
	gone := wf.DetachedProcess{Pid: 1 << 30, Pgid: 1 << 30, StartTicks: 5}
	probeHook = func() {
		if err := writeJSON(req.ResultPath, wf.RunResult{Exit: 4}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { probeHook = nil })
	rep, err := DetachedRunner(testBin(t)).Status(gone, req)
	if err != nil || rep.Status != wf.DetachedPresent || rep.Result.Exit != 4 {
		t.Fatalf("report = %+v, %v, want the result that landed after the probe", rep, err)
	}
}

// unwritableResult is a request whose runner cannot write its result file.
func unwritableResult(t *testing.T) wf.DetachedRequest {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	req := reqIn(t, dir, "true")
	req.ResultPath = filepath.Join(blocker, "7.json")
	return req
}

// The runner is never this process's child, so the test makes its own zombie:
// a session leader that exited and is not yet reaped is not running.
func TestZombieLeaderIsGone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	ticks, _, _ := procStat(pid)
	zombie := false
	for i := 0; i < 500 && !zombie; i++ {
		_, state, ok := procStat(pid)
		zombie = ok && state == 'Z'
		time.Sleep(10 * time.Millisecond)
	}
	if !zombie {
		t.Fatal("the unreaped leader never became a zombie")
	}
	r := DetachedRunner(testBin(t))
	req := reqIn(t, t.TempDir(), "true")
	p := wf.DetachedProcess{Pid: pid, Pgid: pid, StartTicks: ticks}
	rep, err := r.Status(p, req)
	if err != nil || rep.Status == wf.DetachedRunning {
		t.Fatalf("status = %v, %v for a state-Z leader, want not running", rep.Status, err)
	}
	cmd.Wait()
	waited = true
	waitReport(t, r, p, req, wf.DetachedDead)
}

func TestRunnerFailureGoesToLog(t *testing.T) {
	var out, errb bytes.Buffer
	req := unwritableResult(t)
	args := []string{"--result", req.ResultPath, "--dir", req.Dir, "--", "true"}
	if code := RunDetached(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "not a directory") {
		t.Fatalf("code %d stderr %q, want 2 and the write error", code, errb.String())
	}
	for _, c := range []struct {
		name    string
		systemd bool
	}{{"fork", false}, {"systemd", true}} {
		t.Run(c.name, func(t *testing.T) {
			if c.systemd {
				skipWithoutSystemd(t)
			}
			withLauncher(t, c.systemd)
			req := unwritableResult(t)
			r := DetachedRunner(testBin(t))
			p, err := r.Start(req)
			if err != nil {
				t.Fatal(err)
			}
			rep := waitReport(t, r, p, req, wf.DetachedDead)
			if !strings.Contains(rep.Reason, "not a directory") || !strings.Contains(rep.Reason, "runner exited") {
				t.Fatalf("reason = %q, want the runner's own failure and its exit", rep.Reason)
			}
		})
	}
}

func TestStartFailsWhenRunnerCannotWriteProc(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	for _, c := range []struct {
		name    string
		systemd bool
	}{{"fork", false}, {"systemd", true}} {
		t.Run(c.name, func(t *testing.T) {
			if c.systemd {
				skipWithoutSystemd(t)
				withStartWait(t, 3*time.Second)
			}
			withLauncher(t, c.systemd)
			req := reqIn(t, t.TempDir(), "true")
			r := DetachedRunner("/bin/false")
			if _, err := r.Start(req); !errors.Is(err, wf.ErrNotLaunched) {
				t.Fatalf("Start = %v, want not launched (the engine steps down)", err)
			}
		})
	}
}

// The runner exits before its proc record: the procGone branch, noticed well
// before startWait.
func TestStartFailsWhenRunnerExitsBeforeProc(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	withLauncher(t, false)
	t.Setenv(envRunnerExit, "1")
	begin := time.Now()
	_, err := DetachedRunner(testBin(t)).Start(reqIn(t, t.TempDir(), "true"))
	if err == nil || !strings.Contains(err.Error(), "exited before its proc record") {
		t.Fatalf("Start error = %v, want the exited-before-record error", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("Start took %v, want the exit noticed well before startWait", d)
	}
}

func TestDetachedOutputTailed(t *testing.T) {
	r, p, req := startSh(t, "head -c 40960 /dev/zero | tr '\\0' a; head -c 61440 /dev/zero | tr '\\0' b; echo; echo 'FAIL: TestLast'; exit 1")
	got := waitStatus(t, r, p, req, wf.DetachedPresent)
	marker := wf.ElisionLine(40960 + 61440 + len("\nFAIL: TestLast\n") - wf.OutputTail)
	if !strings.HasPrefix(got.Stdout, marker) || len(got.Stdout) != len(marker)+wf.OutputTail || !strings.HasSuffix(got.Stdout, "FAIL: TestLast\n") {
		t.Fatalf("stdout len %d starts %q ends %q, want the marked tail with the failure at its end", len(got.Stdout), got.Stdout[:40], got.Stdout[len(got.Stdout)-20:])
	}
	full, err := os.ReadFile(got.Output)
	if err != nil || !strings.Contains(string(full), strings.Repeat("a", 40960)) || !strings.Contains(string(full), "FAIL: TestLast") {
		t.Fatalf("full output %q (%v): want every byte, the head included", got.Output, err)
	}
}

// A short output is neither cut nor copied.
func TestDetachedShortOutputIsWhole(t *testing.T) {
	r, p, req := startSh(t, "echo short")
	if got := waitStatus(t, r, p, req, wf.DetachedPresent); got.Stdout != "short\n" || got.Output != "" {
		t.Fatalf("result %+v, want the whole stdout and no output file", got)
	}
}

// countingRunner counts Start calls of the wrapped runner.
type countingRunner struct {
	wf.EffectRunner
	starts int
}

func (c *countingRunner) Start(r wf.DetachedRequest) (wf.DetachedProcess, error) {
	c.starts++
	return c.EffectRunner.Start(r)
}

// The runner is killed while the process that started it lives: its reaper
// runs, the probe sees the leader gone, and the launch is classed dead and run
// again, never reported running.
func TestDetachedRunnerCrashWhileStarterLives(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sleep", "30")
	r := &countingRunner{EffectRunner: DetachedRunner(testBin(t))}
	e := engineOver(t, stateDir, d, r)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		for _, rec := range kinds(t, stateDir, run, wf.JournalEffectStarted) {
			var s wf.EffectStartedData
			if json.Unmarshal(rec.Data, &s) == nil {
				syscall.Kill(-s.Pgid, syscall.SIGKILL)
			}
		}
	}
	t.Cleanup(cleanup)
	var first wf.EffectStartedData
	if recs := kinds(t, stateDir, run, wf.JournalEffectStarted); len(recs) != 1 || json.Unmarshal(recs[0].Data, &first) != nil {
		t.Fatalf("effect_started records = %v, want 1", recs)
	}
	if err := syscall.Kill(first.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && len(kinds(t, stateDir, run, wf.JournalEffectStarted)) < 2; i++ {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if in.Kind != wf.InstructionWait {
				t.Fatalf("instruction %+v, want only waits", in)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(kinds(t, stateDir, run, wf.JournalEffectStarted)); n != 2 || r.starts != 2 {
		t.Fatalf("effect_started records %d, Start calls %d, want 2 and 2: the dead launch is run again", n, r.starts)
	}
	if res := kinds(t, stateDir, run, wf.JournalEffectResult); len(res) != 0 {
		t.Fatalf("effect_result records = %d, want none while the second launch runs", len(res))
	}
}

// recordingRunner remembers each Start call of the wrapped runner and what it
// returned.
type recordingRunner struct {
	wf.EffectRunner
	reqs  []wf.DetachedRequest
	procs []wf.DetachedProcess
}

func (c *recordingRunner) Start(r wf.DetachedRequest) (wf.DetachedProcess, error) {
	p, err := c.EffectRunner.Start(r)
	if err == nil {
		c.reqs, c.procs = append(c.reqs, r), append(c.procs, p)
	}
	return p, err
}

// killGroupsOf kills every launch's process group recorded in the run's
// effect_started records, at test cleanup.
func killGroupsOf(t *testing.T, stateDir string, run wf.RunID) {
	t.Cleanup(func() {
		for _, rec := range kinds(t, stateDir, run, wf.JournalEffectStarted) {
			var s wf.EffectStartedData
			if json.Unmarshal(rec.Data, &s) == nil {
				syscall.Kill(-s.Pgid, syscall.SIGKILL)
			}
		}
	})
}

// nextUntil calls Next (waits only) until done reports true.
func nextUntil(t *testing.T, e wf.Executor, run wf.RunID, done func() bool) {
	t.Helper()
	for i := 0; i < 500 && !done(); i++ {
		ins, err := e.Next(run, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range ins {
			if in.Kind != wf.InstructionWait {
				t.Fatalf("instruction %+v, want only waits", in)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !done() {
		t.Fatal("the condition never held within the polling bound")
	}
}

func startedData(t *testing.T, stateDir string, run wf.RunID) []wf.EffectStartedData {
	t.Helper()
	var out []wf.EffectStartedData
	for _, rec := range kinds(t, stateDir, run, wf.JournalEffectStarted) {
		var s wf.EffectStartedData
		if err := json.Unmarshal(rec.Data, &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A dead leader with a live child and no result file is a dead launch, not a
// running one: the group is terminated through -pgid, the re-run waits for it to
// empty and then runs under a fresh intent. It never settles timed out.
func TestDetachedDeadLeaderLiveChildRerun(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sh", "-c", "sleep 30 & wait")
	r := DetachedRunner(testBin(t))
	e := engineOver(t, stateDir, d, r)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	killGroupsOf(t, stateDir, run)
	first := startedData(t, stateDir, run)
	if len(first) != 1 {
		t.Fatalf("effect_started records = %d, want 1", len(first))
	}
	// Only the leader dies; the shell and its sleep live on in the group.
	if err := syscall.Kill(first[0].Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextUntil(t, e, run, func() bool { return len(startedData(t, stateDir, run)) >= 2 })
	p := wf.DetachedProcess{Pid: first[0].Pid, Pgid: first[0].Pgid, StartTicks: first[0].StartTicks}
	if empty, err := r.GroupEmpty(p, wf.DetachedRequest{}); err != nil || !empty {
		t.Fatalf("GroupEmpty of the first launch = %v, %v, want true: the re-run waits for it", empty, err)
	}
	var intents []wf.EffectIntentData
	for _, rec := range kinds(t, stateDir, run, wf.JournalEffectIntent) {
		var in wf.EffectIntentData
		if err := json.Unmarshal(rec.Data, &in); err != nil {
			t.Fatal(err)
		}
		intents = append(intents, in)
	}
	if len(intents) != 2 || intents[1].Rerun == nil || intents[1].Rerun.Of != first[0].Intent || intents[1].Rerun.Pgid != first[0].Pgid {
		t.Fatalf("effect_intent records = %+v, want a fresh second intent re-running intent %d", intents, first[0].Intent)
	}
	if res := kinds(t, stateDir, run, wf.JournalEffectResult); len(res) != 0 {
		t.Fatalf("effect_result records = %s, want none: a dead launch never settles timed out", res[0].Data)
	}
}

// A lost effect_started does not relaunch while the first launch's group lives:
// the proc record says it started.
func TestDetachedLostEffectStartedNoRelaunch(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sleep", "30")
	r := &countingRunner{EffectRunner: DetachedRunner(testBin(t))}
	reg := wf.NewAdapters()
	if err := reg.Register(Runtime(Config{})); err != nil {
		t.Fatal(err)
	}
	j := &dropStartedJournal{Journal: wf.FileJournal(stateDir), drop: 1}
	e := wf.NewEngine(j, reg, wf.EngineOptions{Defs: []*wf.Def{d}, EffectRunner: r,
		RunDir: func(run wf.RunID) string { return filepath.Join(stateDir, "runs", string(run)) }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err == nil {
		t.Fatal("Next succeeded, want the lost effect_started append to fail it")
	}
	intents := kinds(t, stateDir, run, wf.JournalEffectIntent)
	if len(intents) != 1 || r.starts != 1 || len(startedData(t, stateDir, run)) != 0 {
		t.Fatalf("intents %d, Start calls %d, effect_started %d, want 1, 1 and 0", len(intents), r.starts, len(startedData(t, stateDir, run)))
	}
	proc, ok, err := r.Launched(wf.DetachedRequest{ProcPath: filepath.Join(stateDir, "runs", string(run), "effects", fmt.Sprint(intents[0].Seq)+".proc")})
	if err != nil || !ok {
		t.Fatalf("Launched = %v, %v, want the proc record of the first launch", ok, err)
	}
	t.Cleanup(func() { syscall.Kill(-proc.Pgid, syscall.SIGKILL) })
	for i := 0; i < 3; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
	}
	st := startedData(t, stateDir, run)
	if r.starts != 1 || len(st) != 1 || st[0].Pid != proc.Pid || st[0].StartTicks != proc.StartTicks {
		t.Fatalf("Start calls %d, effect_started %+v, want 1 and the proc record's launch journaled", r.starts, st)
	}
}

// dropStartedJournal fails the first drop effect_started appends, as a crash
// between Start and the journal write does.
type dropStartedJournal struct {
	wf.Journal
	drop int
}

func (j *dropStartedJournal) Append(r wf.JournalRecord) error {
	if r.Kind == wf.JournalEffectStarted && j.drop > 0 {
		j.drop--
		return errors.New("crash before effect_started")
	}
	return j.Journal.Append(r)
}

// A re-run after a dead launch reads its own proc record: the second
// effect_started carries the new launch's group and ticks, and its .proc path is
// effects/<new seq>.proc, never the earlier launch's.
func TestDetachedRerunUsesFreshProcRecord(t *testing.T) {
	stateDir := t.TempDir()
	d := detachedDef(time.Minute, "sleep", "30")
	r := &recordingRunner{EffectRunner: DetachedRunner(testBin(t))}
	e := engineOver(t, stateDir, d, r)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	killGroupsOf(t, stateDir, run)
	first := startedData(t, stateDir, run)
	if len(first) != 1 {
		t.Fatalf("effect_started records = %d, want 1", len(first))
	}
	if err := syscall.Kill(first[0].Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	nextUntil(t, e, run, func() bool { return len(startedData(t, stateDir, run)) >= 2 })
	second := startedData(t, stateDir, run)[1]
	if len(r.reqs) != 2 {
		t.Fatalf("Start calls = %d, want 2", len(r.reqs))
	}
	want := filepath.Join(stateDir, "runs", string(run), "effects", fmt.Sprint(second.Intent)+".proc")
	if second.Intent == first[0].Intent || r.reqs[1].ProcPath != want || r.reqs[0].ProcPath == want {
		t.Fatalf("second intent %d (first %d), proc paths %q then %q, want effects/<new seq>.proc = %q", second.Intent, first[0].Intent, r.reqs[0].ProcPath, r.reqs[1].ProcPath, want)
	}
	if second.Pgid == first[0].Pgid || second.StartTicks == first[0].StartTicks || second.StartTicks == 0 {
		t.Fatalf("second launch %+v, first %+v, want a new group and new ticks", second, first[0])
	}
	if second.Pid != r.procs[1].Pid || second.Pgid != r.procs[1].Pgid || second.StartTicks != r.procs[1].StartTicks {
		t.Fatalf("effect_started %+v, want the process Start returned: %+v", second, r.procs[1])
	}
	rec, ok, err := r.Launched(r.reqs[1])
	if err != nil || !ok || rec.Pgid != second.Pgid || rec.StartTicks != second.StartTicks {
		t.Fatalf("proc record %+v (%v, %v), want the second launch's", rec, ok, err)
	}
}

// startNoRecord starts a command whose runner stalls before its proc record
// and asserts the launch times out, returns in time, leaves no runner alive and
// never runs the command.
func startNoRecord(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	marker := filepath.Join(dir, "ran")
	t.Setenv(envRunnerPidfile, pidfile)
	t.Setenv(envRunnerDelay, "30s")
	req := reqIn(t, dir, "sh", "-c", "touch '"+marker+"'")
	begin := time.Now()
	_, err := DetachedRunner(testBin(t)).Start(req)
	if err == nil || !strings.Contains(err.Error(), "wrote no proc record") {
		t.Fatalf("Start error = %v, want the no-proc-record timeout", err)
	}
	if d := time.Since(begin); d > startWait+3*time.Second {
		t.Fatalf("Start took %v, want it bounded by startWait %v", d, startWait)
	}
	waitGone(t, runnerPid(t, pidfile), 2*time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran")
	}
}

// A Start that times out reports "nothing ran" to the engine, so it must leave
// no runner in the launch's group.
func TestStartTimeoutKillsTheRunnerGroup(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	t.Run("fork", func(t *testing.T) {
		withLauncher(t, false)
		withStartWait(t, 200*time.Millisecond)
		startNoRecord(t)
	})
	t.Run("systemd", func(t *testing.T) {
		skipWithoutSystemd(t)
		withLauncher(t, true)
		withStartWait(t, 3*time.Second)
		startNoRecord(t)
	})
}

// A startWait that passes before the --spawn intermediate exits must still kill
// the runner it already forked into a session of its own.
func TestStartWaitExpiryBeforeIntermediateExitsKillsRunner(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	withLauncher(t, false)
	withStartWait(t, 200*time.Millisecond)
	t.Setenv(envSpawnDelay, "600ms")
	startNoRecord(t)
}

// secretEnv sets a sentinel environment variable and returns its value.
func secretEnv(t *testing.T) string {
	t.Helper()
	secret := fmt.Sprintf("sentinel-%d", time.Now().UnixNano())
	t.Setenv("WF_TEST_SECRET", secret)
	return secret
}

// noSecret fails if secret appears in the file at path, or in text.
func noSecret(t *testing.T, what, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("%s carries an environment value: %q", what, text)
	}
}

// A systemd launch whose systemd-run refuses launched nothing and says so: no
// silent double fork, the engine steps down.
func TestSystemdRunFailureLaunchesNothingWithoutEnvValues(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	secret := secretEnv(t)
	argv := stubSystemdRun(t, "echo 'Failed to connect to bus: No such file or directory' >&2\nexit 1\n")
	marker := filepath.Join(t.TempDir(), "ran")
	req := reqIn(t, t.TempDir(), "touch", marker)
	req.Mode = wf.DetachSystemd
	_, err := DetachedRunner(testBin(t)).Start(req)
	if !errors.Is(err, wf.ErrNotLaunched) || !strings.Contains(err.Error(), "systemd-run exit 1: Failed to connect to bus") {
		t.Fatalf("Start error = %v, want not launched with the exit and first stderr line", err)
	}
	noSecret(t, "Start error", err.Error(), secret)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran: Start fell back to a double fork")
	}
	b, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains("\n"+string(b), "\n--setenv=WF_TEST_SECRET\n") {
		t.Fatalf("systemd-run argv lacks --setenv=WF_TEST_SECRET: %q", b)
	}
	noSecret(t, "systemd-run argv", string(b), secret)
	log, _ := os.ReadFile(req.LogPath)
	if !strings.Contains(string(log), "systemd-run unavailable: exit 1: Failed to connect to bus") {
		t.Fatalf("log = %q, want the exit and first stderr line", log)
	}
	noSecret(t, "launch log", string(log), secret)
	noSecret(t, "Status reason", reasonOf(t, req), secret)
}

// A systemd-run that hangs is stopped and its paths fenced: the unit's runner,
// started late by the user manager, finds the fence and never runs the
// command, so the engine may step down at once.
func TestSystemdRunTimeoutNeverRunsTwice(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not found")
	}
	withStartWait(t, 300*time.Millisecond)
	secret := secretEnv(t)
	// The stub starts the unit's command late, in a session of its own that
	// RunTool's timeout kill of the stub's group does not reach, the way a user
	// manager would after systemd-run gave up.
	argv := stubSystemdRun(t, `while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
setsid sh -c 'sleep 0.6; exec "$@"' late "$@" </dev/null >/dev/null 2>&1 &
exec sleep 30
`)
	count := filepath.Join(t.TempDir(), "count")
	req := reqIn(t, t.TempDir(), "sh", "-c", "echo ran >> '"+count+"'")
	req.Mode = wf.DetachSystemd
	t.Cleanup(func() {
		if p, ok, _ := readProc(req.ProcPath); ok {
			_ = syscall.Kill(-p.Pgid, syscall.SIGKILL)
		}
	})
	begin := time.Now()
	_, err := DetachedRunner(testBin(t)).Start(req)
	if !errors.Is(err, wf.ErrNotLaunched) || !strings.Contains(err.Error(), "did not return within") {
		t.Fatalf("Start error = %v, want not launched: systemd-run did not return, the paths are fenced", err)
	}
	noSecret(t, "Start error", err.Error(), secret)
	if d := time.Since(begin); d > 8*time.Second {
		t.Fatalf("Start took %v", d)
	}
	// The late unit's runner starts after the fence: it writes its record,
	// finds the stop request and exits without running the command.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok, _ := readProc(req.ProcPath); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late unit's runner never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if b, err := os.ReadFile(count); err == nil {
		t.Fatalf("the fenced late runner ran the command: %q", b)
	}
	if _, err := os.Stat(req.ResultPath); err == nil {
		t.Fatal("the fenced late runner wrote a result")
	}
	b, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains("\n"+string(b), "\n--setenv=WF_TEST_SECRET\n") {
		t.Fatalf("systemd-run argv lacks --setenv=WF_TEST_SECRET: %q", b)
	}
	noSecret(t, "systemd-run argv", string(b), secret)
	log, _ := os.ReadFile(req.LogPath)
	noSecret(t, "launch log", string(log), secret)
	noSecret(t, "Status reason", reasonOf(t, req), secret)
}

// ppidOf is the parent pid of pid: field 4 of /proc/<pid>/stat.
func ppidOf(t *testing.T, pid int) int {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	var ppid int
	if _, err := fmt.Sscan(f[1], &ppid); err != nil {
		t.Fatalf("stat %q: %v", b, err)
	}
	return ppid
}

// withLauncher selects the launch path of a request with no mode: systemd-run,
// or (false) the double fork.
func withLauncher(t *testing.T, systemd bool) {
	t.Helper()
	old := systemdRun
	if systemd {
		systemdRun = "systemd-run"
	} else {
		systemdRun = ""
	}
	resetDetachProbe(t)
	t.Cleanup(func() { systemdRun = old })
}

// resetDetachProbe makes the next DetachMode probe again, now and after the
// test, so a probe of a stubbed systemd-run stays in its test.
func resetDetachProbe(t *testing.T) {
	t.Helper()
	detachOnce = sync.Once{}
	t.Cleanup(func() { detachOnce = sync.Once{} })
}

// withStartWait sets startWait for the test.
func withStartWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := startWait
	startWait = d
	t.Cleanup(func() { startWait = old })
}

// stubSystemdRun points the systemd-run path at a shell script that first
// records its arguments, one per line, in the returned file, then runs body.
func stubSystemdRun(t *testing.T, body string) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "systemd-run")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\n" + body
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemdRun
	systemdRun = stub
	resetDetachProbe(t)
	t.Cleanup(func() { systemdRun = old })
	return argvFile
}

// runnerPid reads the pid a runner wrote to pidfile, waiting up to 5s for it.
func runnerPid(t *testing.T, pidfile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pidfile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = syscall.Kill(pid, syscall.SIGKILL) })
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no runner pid in %s", pidfile)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitGone fails unless pid has exited (gone or a zombie) within d.
func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, state, ok := procStat(pid); !ok || state == 'Z' {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reasonOf is the Status reason a dead launch of req would report: its log tail.
func reasonOf(t *testing.T, req wf.DetachedRequest) string {
	t.Helper()
	req.ResultPath = filepath.Join(t.TempDir(), "absent.json")
	rep, err := DetachedRunner(testBin(t)).Status(wf.DetachedProcess{}, req)
	if err != nil {
		t.Fatal(err)
	}
	return rep.Reason
}

func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func skipWithoutSystemd(t *testing.T) {
	t.Helper()
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	out, err := exec.Command("systemd-run", "--user", "--collect", "--quiet", "--wait", "true").CombinedOutput()
	if err != nil {
		t.Skipf("systemd-run --user unavailable: %v: %s", err, out)
	}
}

func TestStartViaDoubleForkLeavesNoDescendant(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	withLauncher(t, false)
	r, p, req := startSh(t, "sleep 1")
	if pp := ppidOf(t, p.Pid); pp == os.Getpid() {
		t.Fatalf("runner %d is a child of the caller", p.Pid)
	}
	if cg := cgroupOf(t, p.Pid); strings.Contains(cg, "/wf-") {
		t.Fatalf("double-fork runner is in a transient unit: %s", cg)
	}
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("status = %v, %v right after Start, want running", rep.Status, err)
	}
	waitStatus(t, r, p, req, wf.DetachedPresent)
}

func TestStartViaSystemdRunLeavesNoDescendant(t *testing.T) {
	skipWithoutSystemd(t)
	withLauncher(t, true)
	r, p, req := startSh(t, "sleep 1")
	if pp := ppidOf(t, p.Pid); pp == os.Getpid() {
		t.Fatalf("runner %d is a child of the caller", p.Pid)
	}
	if cg := cgroupOf(t, p.Pid); !strings.Contains(cg, "/wf-") || !strings.Contains(cg, ".service") {
		t.Fatalf("runner cgroup = %q, want a transient wf-*.service unit", cg)
	}
	if p.Pid != p.Pgid {
		t.Fatalf("pid %d != pgid %d", p.Pid, p.Pgid)
	}
	if rep, err := r.Status(p, req); err != nil || rep.Status != wf.DetachedRunning {
		t.Fatalf("status = %v, %v right after Start, want running", rep.Status, err)
	}
	waitStatus(t, r, p, req, wf.DetachedPresent)
}

func TestSystemdRunKeepsDollarAndEnv(t *testing.T) {
	skipWithoutSystemd(t)
	withLauncher(t, true)
	t.Setenv("WF_TEST_MARK", "x1")
	r, p, req := startSh(t, `echo "$WF_TEST_MARK" '$HOME'`)
	got := waitStatus(t, r, p, req, wf.DetachedPresent)
	if got.Stdout != "x1 $HOME\n" {
		t.Fatalf("stdout = %q, want the caller's env and no $ expansion", got.Stdout)
	}
}

func TestNestedPidNS(t *testing.T) {
	for _, c := range []struct {
		status string
		want   bool
	}{
		{"Name:\tx\nNSpid:\t123\n", false},
		{"Name:\tx\nNSpid:\t123\t7\n", true},
		{"Name:\tx\n", false},
	} {
		if got := nestedPidNS([]byte(c.status)); got != c.want {
			t.Errorf("nestedPidNS(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestSystemdUnitNameIsSafe(t *testing.T) {
	name := unitName(filepath.Join("/x", "run id/é", "effects", "7.proc"))
	if !strings.HasPrefix(name, "wf-__-7-") || len(name) > 200 {
		t.Fatalf("unit name = %q", name)
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			t.Fatalf("unit name %q has byte %q", name, c)
		}
	}
	if other := unitName(filepath.Join("/x", "run id/é", "effects", "7.proc")); other == name {
		t.Fatal("two unit names for the same intent collide")
	}
}

// modeRunner records the mode of every Start call of the wrapped runner,
// failed ones included, and keeps its detach probe.
type modeRunner struct {
	wf.EffectRunner
	modes []string
}

func (m *modeRunner) DetachMode() wf.DetachFact { return m.EffectRunner.(wf.DetachProber).DetachMode() }

func (m *modeRunner) Start(r wf.DetachedRequest) (wf.DetachedProcess, error) {
	m.modes = append(m.modes, r.Mode)
	return m.EffectRunner.Start(r)
}

// Item 98 under the real runner: a systemd-run that refuses (the Codex
// sandbox's user bus) launched nothing, so the same call launches on process,
// never a silent double fork of the systemd launch. When that runner dies too,
// the re-run lands inline with no further launch and no runner-failed exit,
// each step-down is a degraded record, and the dead runner's reason is its own
// launch's, not an earlier systemd-run line.
func TestDetachLadderUnderARefusingSystemd(t *testing.T) {
	if !detachedSupported {
		t.Skip("the detached runner needs Linux")
	}
	t.Setenv(detachModeEnv, wf.DetachSystemd)
	stubSystemdRun(t, "echo 'Failed to connect to user scope bus via local transport: Operation not permitted' >&2\nexit 1\n")
	stateDir := t.TempDir()
	count := filepath.Join(t.TempDir(), "count")
	// Each run appends a line; the first sleeps so its runner can be killed.
	d := detachedDef(time.Minute, "sh", "-c", `echo x >> "$0"; [ "$(wc -l < "$0")" -ge 2 ] || sleep 30`, count)
	r := &modeRunner{EffectRunner: DetachedRunner(testBin(t))}
	e := engineOver(t, stateDir, d, r)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	killGroupsOf(t, stateDir, run)
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{wf.DetachSystemd, wf.DetachProcess}; !slices.Equal(r.modes, want) {
		t.Fatalf("Start modes = %v, want %v within the first call", r.modes, want)
	}
	started := startedData(t, stateDir, run)
	if len(started) != 1 {
		t.Fatalf("effect_started = %+v, want the process launch", started)
	}
	if err := syscall.Kill(started[0].Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var st wf.RunStatus
	for i := 0; i < 500 && st.Phase != wf.RunEnded; i++ {
		if _, err := e.Next(run, ""); err != nil {
			t.Fatal(err)
		}
		if st, err = e.Status(run); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Phase != wf.RunEnded || st.Terminal != "cOK" || st.Detach != wf.DetachNone {
		t.Fatalf("status = %s/%s detach %q, want ended/cOK at none", st.Phase, st.Terminal, st.Detach)
	}
	if want := []string{wf.DetachSystemd, wf.DetachProcess}; !slices.Equal(r.modes, want) || len(startedData(t, stateDir, run)) != 1 {
		t.Fatalf("Start modes = %v, effect_started %d, want %v and 1: inline launches nothing", r.modes, len(startedData(t, stateDir, run)), want)
	}
	if b, _ := os.ReadFile(count); string(b) != "x\nx\n" {
		t.Fatalf("command ran %q, want twice: on process and inline", b)
	}
	if len(st.Degraded) != 2 || st.Degraded[0].Kind != wf.DegradedDetach || st.Degraded[0].Action != "stepped down systemd -> process" ||
		!strings.Contains(st.Degraded[0].Cause, "Failed to connect to user scope bus") ||
		st.Degraded[1].Action != "stepped down process -> none" || st.Degraded[1].Cause != "runner died with no result (mode process)" {
		t.Fatalf("degraded = %+v, want systemd -> process (the refusal), then process -> none (the dead runner)", st.Degraded)
	}
}

// The runner's half of the fence: a stop request already beside the result
// when its record is written means the launch was given up, so the command
// never runs.
func TestFencedRunnerDoesNotRun(t *testing.T) {
	req := reqIn(t, t.TempDir(), "x")
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.MkdirAll(filepath.Dir(req.ResultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stopPath(req.ResultPath), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	args := []string{"--result", req.ResultPath, "--proc", req.ProcPath, "--dir", req.Dir, "--", "touch", marker}
	if code := RunDetached(args, &out, &errb); code != 0 || !strings.Contains(errb.String(), "not run") {
		t.Fatalf("code %d stderr %q, want 0 and the fence noted", code, errb.String())
	}
	if _, ok, _ := readProc(req.ProcPath); !ok {
		t.Fatal("no proc record: the record comes before the fence check")
	}
	for _, p := range []string{marker, req.ResultPath} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists: the fenced command ran", p)
		}
	}
}

// The starter's half: fenceLate leaves the fence and reports nothing launched
// when no record is there, and adopts (withdrawing the fence) a record that
// came first.
func TestFenceLateAdoptsARecordThatCameFirst(t *testing.T) {
	req := reqIn(t, t.TempDir(), "x")
	if err := os.MkdirAll(filepath.Dir(req.ResultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	why := errors.New("systemd-run did not return")
	if _, err := fenceLate(req, why); !errors.Is(err, wf.ErrNotLaunched) {
		t.Fatalf("fenceLate = %v, want not launched", err)
	}
	if _, err := os.Stat(stopPath(req.ResultPath)); err != nil {
		t.Fatalf("no fence after fenceLate: %v", err)
	}
	if err := writeJSON(req.ProcPath, procRecord{Pid: 4242, Pgid: 4242, StartTicks: 7}); err != nil {
		t.Fatal(err)
	}
	p, err := fenceLate(req, why)
	if err != nil || p.Pid != 4242 {
		t.Fatalf("fenceLate = %+v, %v, want the record's launch", p, err)
	}
	if _, err := os.Stat(stopPath(req.ResultPath)); err == nil {
		t.Fatal("the fence stays on an adopted launch")
	}
}

// The engine's fence of a launch whose proc record is already there adopts
// that launch, so the fence is withdrawn: the runner keeps running past its
// heartbeats and its result file settles the effect.
func TestFenceOfARecordedLaunchIsWithdrawn(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 50 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })

	r, p, req := startSh(t, "sleep 0.5; echo done")
	waitAlive(t, req)
	if err := r.Terminate(wf.DetachedProcess{}, req); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stopPath(req.ResultPath)); err == nil {
		t.Fatal("the fence stays on a launch whose record came first")
	}
	if res := waitStatus(t, r, p, req, wf.DetachedPresent); res.Exit != 0 || res.Stdout != "done\n" {
		t.Fatalf("result %+v, want the adopted launch's exit 0 and stdout done", res)
	}
}
