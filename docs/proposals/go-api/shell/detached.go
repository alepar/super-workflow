package shell

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// DetachedVerb is the hidden subcommand a detached launch re-executes the
// binary as. It is not listed in usage.
const DetachedVerb = "__detached"

// reasonTail is how much of the log Status reports as the reason of a dead
// launch.
const reasonTail = 4 << 10

// startWait is how long Start waits for the proc record: a var, a test seam
// like probeHook.
var startWait = 5 * time.Second

// probeHook is a test seam, nil in production: it runs between Status's
// negative probe and its second read of the result file.
var probeHook func()

// spawnHook is a test seam, nil in production: the --spawn intermediate runs it
// after starting the runner and before printing the runner's pid.
var spawnHook func()

// systemdRun is the program a systemd launch and the systemd probe run; ""
// disables that mode (a test seam).
var systemdRun = "systemd-run"

// terminateGrace is how long Terminate waits after SIGTERM before SIGKILL.
const terminateGrace = 2 * time.Second

// heartbeatEvery is how often the runner touches its heartbeat file while the
// command runs (a var: a test seam). Start hands it to the runner, so the
// starter and the runner agree. A heartbeat older than staleBeats intervals
// with no result file is a dead runner (escalation-and-recovery G2).
var heartbeatEvery = 5 * time.Second

const staleBeats = 4

// alivePath is the heartbeat file of the launch whose result file is result.
// It lives beside the result file, so any process that shares the filesystem
// can judge the runner's liveness, in whatever PID namespace it runs.
func alivePath(result string) string { return result + ".alive" }

// stopPath is the stop request of the launch whose result file is result: a
// Terminate that cannot signal the runner (it is in another PID namespace)
// writes it, and the runner, which checks it on every heartbeat, stops its own
// process group.
func stopPath(result string) string { return result + ".stop" }

// procRecord is the pre-launch proc record the runner writes to --proc before
// the command runs.
type procRecord struct {
	Pid, Pgid  int
	StartTicks uint64
	// PidNS is the runner's PID namespace (/proc/self/ns/pid). A reader in
	// the same namespace can trust the pid probe; any other reader judges the
	// runner by its heartbeat.
	PidNS string `json:",omitempty"`
}

// pidNS is this process's PID namespace, "" where /proc does not say.
func pidNS() string {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return ns
}

// samePidNS reports whether the runner recorded at procPath shares this
// process's PID namespace, so its pid means the same here. A record without
// a namespace (a runner from before G2) is trusted, as before.
func samePidNS(procPath string) bool {
	b, err := os.ReadFile(procPath)
	if err != nil {
		return true
	}
	var rec procRecord
	if json.Unmarshal(b, &rec) != nil || rec.PidNS == "" {
		return true
	}
	return rec.PidNS == pidNS()
}

// RunDetached is the hidden verb: it runs `-- argv...` in --dir, captures
// stdout and stderr (the last wf.OutputTail bytes of each, marked when cut,
// with the full output in <result>.out named by the result), and writes the
// wf.RunResult to --result atomically (temporary file, fsync, rename). Before
// the command runs it writes its own pid, pgid and start ticks to --proc the
// same way. The command's exit code is data; a command
// that cannot start is exit 127 with the error as stderr. The verb itself exits
// 0 once the file is in place, and 2 on a usage or write error; a non-zero exit
// is first reported on stderr (the launch's log) as "runner exited".
//
// With --spawn first the verb is the double fork's intermediate instead: it
// starts the runner (the same arguments without --spawn) in a new session,
// prints its pid on stdout and exits without waiting, so the runner is
// reparented before the starter returns.
func RunDetached(args []string, stdout, stderr io.Writer) int {
	for i, a := range args {
		if a == "--" {
			break
		}
		if a == "--spawn" {
			return spawnRunner(append(append([]string{}, args[:i]...), args[i+1:]...), stdout, stderr)
		}
	}
	code := runDetached(args, stderr)
	if code != 0 {
		fmt.Fprintf(stderr, "runner exited: exit status %d\n", code)
	}
	return code
}

// spawnRunner starts this binary as the runner in its own session. Its output
// goes to os.Stderr itself (the log file): a writer that is not an *os.File
// would make Go copy through a pipe that dies with this process.
func spawnRunner(args []string, stdout, stderr io.Writer) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", DetachedVerb, err)
		return 2
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", DetachedVerb, err)
		return 2
	}
	defer null.Close()
	cmd := exec.Command(exe, append([]string{DetachedVerb}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, os.Stderr, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", DetachedVerb, err)
		return 2
	}
	if spawnHook != nil {
		spawnHook()
	}
	fmt.Fprintln(stdout, cmd.Process.Pid)
	return 0
}

func runDetached(args []string, stderr io.Writer) int {
	var result, dir, proc, sandboxBin string
	var argv []string
	var beat time.Duration
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			argv = args[i+1:]
			i = len(args)
		case args[i] == "--result" && i+1 < len(args):
			i++
			result = args[i]
		case args[i] == "--proc" && i+1 < len(args):
			i++
			proc = args[i]
		case args[i] == "--dir" && i+1 < len(args):
			i++
			dir = args[i]
		case args[i] == "--sandbox-bin" && i+1 < len(args):
			i++
			sandboxBin = args[i]
		case args[i] == "--heartbeat" && i+1 < len(args):
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d <= 0 {
				fmt.Fprintf(stderr, "%s: --heartbeat %q: want a positive duration\n", DetachedVerb, args[i])
				return 2
			}
			beat = d
		default:
			fmt.Fprintf(stderr, "%s: unexpected argument %q\n", DetachedVerb, args[i])
			return 2
		}
	}
	if result == "" || len(argv) == 0 {
		fmt.Fprintf(stderr, "usage: %s --result <path> [--proc <path>] --dir <dir> -- argv...\n", DetachedVerb)
		return 2
	}
	if beat > 0 {
		// The heartbeat starts before the proc record, so a launch with a
		// record and no heartbeat file is a runner from before heartbeats.
		stop := startHeartbeat(alivePath(result), stopPath(result), beat)
		defer stop()
	}
	if proc != "" {
		// Both launch paths make the runner a group leader; this keeps pid == pgid
		// if a launcher ever does not.
		if syscall.Getpgrp() != os.Getpid() {
			_, _ = syscall.Setsid()
		}
		ticks, _, _ := procStat(os.Getpid())
		if err := writeJSON(proc, procRecord{Pid: os.Getpid(), Pgid: syscall.Getpgrp(), StartTicks: ticks, PidNS: pidNS()}); err != nil {
			fmt.Fprintf(stderr, "%s: proc record: %v\n", DetachedVerb, err)
			return 2
		}
	}
	// The fence: a stop request already there when the record is written means
	// the starter gave this launch up (see fenceLate). Record, then read.
	if _, err := os.Stat(stopPath(result)); err == nil {
		fmt.Fprintf(stderr, "%s: stop request before the command started; not run\n", DetachedVerb)
		return 0
	}
	res := runArgv(argv, dir, result+".out", sandboxBin)
	if err := writeJSON(result, res); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", DetachedVerb, err)
		return 2
	}
	return 0
}

// startHeartbeat touches path now and every interval until stop is called;
// stop removes it. The result file is written before stop, so a reader never
// sees neither.
func startHeartbeat(path, stopReq string, every time.Duration) (stop func()) {
	touch := func() {
		now := time.Now()
		if err := os.Chtimes(path, now, now); err != nil {
			_ = os.WriteFile(path, nil, 0o644)
		}
	}
	touch()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, err := os.Stat(stopReq); err == nil {
					stopOwnGroup(path)
				}
				touch()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		_ = os.Remove(path)
	}
}

// stopOwnGroup answers a stop request: SIGTERM to the runner's own process
// group (the runner and its command), then SIGKILL after terminateGrace, as
// Terminate does from the runner's own namespace. The runner ignores the
// SIGTERM itself, so it lives to send the SIGKILL to a command that traps
// SIGTERM. Just before the SIGKILL it removes its heartbeat, which tells the
// waiting Terminate the group is going. The runner dies with its group and
// writes no result file: the engine has already settled the effect.
func stopOwnGroup(alive string) {
	signal.Ignore(syscall.SIGTERM)
	_ = syscall.Kill(0, syscall.SIGTERM)
	time.Sleep(terminateGrace)
	_ = os.Remove(alive)
	_ = syscall.Kill(0, syscall.SIGKILL)
}

// foreignAlive judges a runner in another PID namespace by its heartbeat. A
// stale one is looked at again a beat later before it counts as dead: the
// runner's ticker does not advance while the host is suspended, so just after
// a resume a live runner's heartbeat can be stale for up to one interval.
func foreignAlive(result string) bool {
	if present, fresh := heartbeat(result); fresh || !present {
		return fresh
	}
	time.Sleep(heartbeatEvery + heartbeatEvery/2)
	_, fresh := heartbeat(result)
	return fresh
}

// heartbeat reads the launch's heartbeat file: present is false when there is
// none (a runner from before heartbeats, or not started yet); fresh is true
// when it was touched within staleBeats intervals.
func heartbeat(result string) (present, fresh bool) {
	fi, err := os.Stat(alivePath(result))
	if err != nil {
		return false, false
	}
	return true, time.Since(fi.ModTime()) < staleBeats*heartbeatEvery
}

// A sandboxed run's command (sandboxBin set) runs with the run's bd shim
// first on PATH, as an inline one does.
func runArgv(argv []string, dir, fullPath, sandboxBin string) wf.RunResult {
	var stdout, stderr bytes.Buffer
	argv0 := argv[0]
	if sandboxBin != "" {
		argv0 = wf.SandboxArgv0(argv0, sandboxBin)
	}
	cmd := exec.Command(argv0, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = commandEnv(sandboxBin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A descendant holding the pipes past the command's exit does not hold the
	// result back.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	res := wf.RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	// A cut stream keeps its tail, marked; the full output goes beside the
	// result file and the result names it (artifact-identity C).
	out, cutOut := wf.CutStream(res.Stdout)
	errs, cutErr := wf.CutStream(res.Stderr)
	if cutOut || cutErr {
		if err := os.WriteFile(fullPath, wf.FullOutput(res.Stdout, res.Stderr), 0o644); err == nil {
			res.Output = fullPath
		}
		res.Stdout, res.Stderr = out, errs
	}
	var ee *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay):
	case errors.As(err, &ee):
		res.Exit = ee.ExitCode()
	default:
		res.Exit, res.Stderr = 127, err.Error()
	}
	return res
}

// lastBytes is the last n bytes of b.
func lastBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// writeJSON writes v to path atomically (temporary file, fsync, rename).
func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// DetachedRunner returns the wf.EffectRunner that re-executes bin (the running
// executable's absolute path) as the hidden verb, launched outside the calling
// process's tree so the command outlives the CLI call that started it, and a
// tool-call kill (TaskStop kills the descendant tree) does not reach it: on
// the request's mode, as a transient `systemd-run --user` unit (a child of the
// user manager, outside the tool call's process tree and the login scope), or
// by a double fork (setsid in a grandchild, the intermediate exits). It never
// falls back from one to the other: a launch that left nothing running (a
// systemd-run that is missing or refuses, a runner that never wrote its proc
// record) is wf.ErrNotLaunched, and a systemd-run that does not answer within
// startWait makes the launch unavailable; the engine steps down. The
// environment is forwarded by name, never by value.
//
// A launch is identified by its process group, the pre-launch proc record and
// the leader's /proc start ticks, which exist only on Linux: elsewhere Start
// returns an error wrapping wf.ErrUnavailable and saying so.
//
// Limitation: under Codex's workspace-write sandbox every command runs in its
// own PID namespace, so a runner dies when the coordinator session ends; Start
// notes this in the launch's log when it detects it, and the engine's re-run of
// a dead launch one mode down is the recovery.
func DetachedRunner(bin string) wf.EffectRunner { return detached{bin: bin} }

type detached struct{ bin string }

// DetachMode is this host's detach mode, probed once per process.
// WF_DETACH_MODE (systemd, process or none) states the mode instead, for a
// host the probe misreads; it is read on every call.
func (detached) DetachMode() wf.DetachFact {
	if m := os.Getenv(detachModeEnv); m == wf.DetachSystemd || m == wf.DetachProcess || m == wf.DetachNone {
		return wf.DetachFact{Mode: m, Systemd: wf.DetachSetBy + detachModeEnv}
	}
	detachOnce.Do(func() {
		status, _ := os.ReadFile("/proc/self/status")
		pid1, _ := os.ReadFile("/proc/1/comm")
		detachProbed = probeDetach(status, strings.TrimSpace(string(pid1)), systemdAnswer)
	})
	return detachProbed
}

const detachModeEnv = "WF_DETACH_MODE"

var (
	detachOnce   sync.Once
	detachProbed wf.DetachFact
)

// perCommandInit are the pid 1 programs of a sandbox that gives each command
// its own PID namespace, where a runner dies with the command that started it.
// Codex's sandbox shows pid 1 "codex" with a one-field NSpid line. A
// container's nested namespace (docker, podman toolbox, a k8s pod) keeps
// processes past their command, so nesting alone is not none.
var perCommandInit = wf.PerCommandSandboxInit

// detachModeOf decides the mode from its inputs: a PID namespace whose pid 1 is
// a per-command sandbox is none, however many NSpid fields it shows; a user bus
// that ran the probe's unit is systemd; anything else is process.
func detachModeOf(status []byte, pid1, systemd string) wf.DetachFact {
	f := wf.DetachFact{Mode: wf.DetachProcess, Systemd: systemd, Pid1: pid1}
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			f.NSpidFields = len(strings.Fields(rest))
		}
	}
	switch {
	case slices.Contains(perCommandInit, pid1):
		f.Mode = wf.DetachNone
	case systemd == systemdUserBus:
		f.Mode = wf.DetachSystemd
	}
	return f
}

// probeDetach is detachModeOf with the systemd probe run only when it can
// matter: under a per-command sandbox the mode is none whatever it says.
func probeDetach(status []byte, pid1 string, systemd func() string) wf.DetachFact {
	if slices.Contains(perCommandInit, pid1) {
		return detachModeOf(status, pid1, "not probed: per-command sandbox")
	}
	return detachModeOf(status, pid1, systemd())
}

const systemdUserBus = "user bus"

// systemdAnswer is what the systemd probe found: "not found" (no
// systemd-run), "no user bus: <why>", or "user bus". The probe runs a trivial
// transient unit and waits for it, so a bus whose socket exists but refuses
// the connection (a sandbox) is no user bus.
func systemdAnswer() string {
	if systemdRun == "" {
		return "disabled"
	}
	if _, err := exec.LookPath(systemdRun); err != nil {
		return "not found"
	}
	res, err := wf.RunTool(wf.Tool{Bin: systemdRun, Args: []string{"--user", "--quiet", "--collect", "--wait", "--no-ask-password", "--", "true"}, Timeout: probeWait})
	switch {
	case errors.Is(err, wf.ErrUnavailable):
		return fmt.Sprintf("no user bus: no answer within %v", probeWait)
	case err != nil:
		return "no user bus: systemd-run could not run"
	case res.Exit != 0:
		line, _, _ := strings.Cut(strings.TrimSpace(res.Stderr), "\n")
		return fmt.Sprintf("no user bus: exit %d: %s", res.Exit, line)
	}
	return systemdUserBus
}

// probeWait bounds the systemd probe's unit.
const probeWait = 5 * time.Second

// nestedPidNS reports whether the contents of /proc/self/status show a nested
// PID namespace: the NSpid line has more than one field.
func nestedPidNS(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			return len(strings.Fields(rest)) > 1
		}
	}
	return false
}

// unitName is the transient unit's name: wf-<run>-<seq>-<8 hex>, from the run
// directory's name and the intent seq in the proc path
// (<run dir>/effects/<seq>.proc). The random suffix keeps a re-run of the same
// intent from colliding with a unit that still exists.
func unitName(procPath string) string {
	clean := func(s string, max int) string {
		b := []byte(s)
		for i, c := range b {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
				b[i] = '_'
			}
		}
		if len(b) > max {
			b = b[:max]
		}
		return string(b)
	}
	run := filepath.Base(filepath.Dir(filepath.Dir(procPath)))
	seq := strings.TrimSuffix(filepath.Base(procPath), ".proc")
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("wf-%s-%s-%x", clean(run, 80), clean(seq, 40), rnd)
}

func (d detached) Start(r wf.DetachedRequest) (wf.DetachedProcess, error) {
	if !detachedSupported {
		return wf.DetachedProcess{}, fmt.Errorf("%w: the detached runner needs Linux /proc start ticks", wf.ErrUnavailable)
	}
	if r.ProcPath == "" || r.LogPath == "" {
		return wf.DetachedProcess{}, errors.New("detached start needs a proc path and a log path")
	}
	for _, p := range []string{r.ProcPath, r.LogPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return wf.DetachedProcess{}, err
		}
	}
	// A re-run under the same paths must not read the earlier launch's record,
	// nor answer the earlier launch's stop request.
	// A path whose parent is not a directory has nothing to remove: the
	// runner's own write then fails and is logged, as before.
	for _, p := range []string{r.ProcPath, stopPath(r.ResultPath)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return wf.DetachedProcess{}, err
		}
	}
	logPath, err := filepath.Abs(r.LogPath)
	if err != nil {
		return wf.DetachedProcess{}, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return wf.DetachedProcess{}, err
	}
	defer log.Close()
	if st, err := os.ReadFile("/proc/self/status"); err == nil && nestedPidNS(st) {
		fmt.Fprintln(log, "note: running in a nested PID namespace (a sandbox such as Codex workspace-write); the runner dies with it and is re-launched")
	}
	args := []string{DetachedVerb, "--result", r.ResultPath, "--proc", r.ProcPath, "--dir", r.Dir,
		"--heartbeat", heartbeatEvery.String()}
	if r.Context.Sandboxed {
		if r.Context.SandboxBin == "" {
			return wf.DetachedProcess{}, fmt.Errorf("%w: %w", wf.ErrNotLaunched, wf.ErrNoSandboxShim)
		}
		args = append(args, "--sandbox-bin", r.Context.SandboxBin)
	}
	args = append(append(args, "--"), r.Argv...)
	mode := r.Mode
	if mode == "" {
		mode = d.DetachMode().Mode
	}
	if mode == wf.DetachSystemd {
		return d.startSystemd(r, logPath, log, args)
	}
	return d.startForked(r, log, args)
}

// launchLine opens each launch's own lines in its log: "launch: mode <mode>:
// <how>". A dead launch's reason is what its log says after it.
const launchLine = "launch: mode "

// startSystemd launches the runner as a transient user unit. A systemd-run that
// is missing, could not be run, or answered with a non-zero exit launched no
// unit: the error wraps wf.ErrNotLaunched. One that does not answer within
// startWait is unavailable: its unit may still start, so a launch on another
// mode now could run the command twice. The runner is not our child and its
// pid is unknown until its record exists, so a runner that dies before writing
// it costs the full startWait here; its unit is then stopped and the launch,
// with nothing left running, is wf.ErrNotLaunched too.
func (d detached) startSystemd(r wf.DetachedRequest, logPath string, log io.Writer, args []string) (wf.DetachedProcess, error) {
	if systemdRun == "" {
		return wf.DetachedProcess{}, fmt.Errorf("%w: systemd-run disabled", wf.ErrNotLaunched)
	}
	if _, err := exec.LookPath(systemdRun); err != nil {
		fmt.Fprintf(log, "systemd-run not found\n")
		return wf.DetachedProcess{}, fmt.Errorf("%w: systemd-run not found", wf.ErrNotLaunched)
	}
	name := unitName(r.ProcPath)
	fmt.Fprintf(log, "%ssystemd: unit %s\n", launchLine, name)
	cmd := []string{
		"--user", "--collect", "--quiet", "--no-ask-password", "--unit=" + name,
		"--service-type=exec", "--expand-environment=no",
		// KillMode=process keeps the group semantics: when the leader dies, systemd
		// leaves the rest of the group alone, and Terminate owns the cleanup.
		"-p", "KillMode=process", "-p", "StandardInput=null",
		"-p", "StandardOutput=append:" + logPath, "-p", "StandardError=append:" + logPath,
	}
	// The user manager's environment is not the caller's. Names only: systemd-run
	// takes each value from its own environment, which is this process's, so no
	// value is on its argv (world-readable in /proc) or in an error quoting it.
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); k != "" {
			cmd = append(cmd, "--setenv="+k)
		}
	}
	cmd = append(cmd, "--", d.bin)
	cmd = append(cmd, args...)
	res, err := wf.RunTool(wf.Tool{Bin: systemdRun, Args: cmd, Timeout: startWait})
	switch {
	case errors.Is(err, wf.ErrUnavailable):
		// Timed out: the unit may exist with its start job queued or activating,
		// and a start still in flight may run it after the stop. Stop it, then
		// fence the paths: a runner that wrote its record first is the launch;
		// any later one exits before its command, so nothing ran.
		stopUnit(name)
		fmt.Fprintf(log, "systemd-run did not return within %v; unit %s stopped\n", startWait, name)
		return fenceLate(r, fmt.Errorf("systemd-run did not return within %v; unit %s stopped", startWait, name))
	case err != nil:
		// systemd-run could not be run at all: no unit exists. RunTool's error
		// quotes the argv, so only a fixed message is logged and returned.
		fmt.Fprintf(log, "systemd-run unavailable: could not run it\n")
		return wf.DetachedProcess{}, fmt.Errorf("%w: systemd-run could not run", wf.ErrNotLaunched)
	case res.Exit != 0:
		line, _, _ := strings.Cut(strings.TrimSpace(res.Stderr), "\n")
		fmt.Fprintf(log, "systemd-run unavailable: exit %d: %s\n", res.Exit, line)
		return wf.DetachedProcess{}, fmt.Errorf("%w: systemd-run exit %d: %s", wf.ErrNotLaunched, res.Exit, line)
	}
	timer := time.NewTimer(startWait)
	defer timer.Stop()
	p, res2, err := waitProc(r.ProcPath, nil, timer.C)
	if err != nil {
		return wf.DetachedProcess{}, err
	}
	if res2 == procTimeout {
		stopUnit(name)
		return fenceLate(r, fmt.Errorf("detached runner wrote no proc record within %v; runner killed: %s", startWait, logTail(r.LogPath)))
	}
	return p, nil
}

// fenceLate ends a systemd launch that did not show up in time: it writes the
// launch's stop request, then reads its proc record once more. A runner that
// wrote the record first is the launch (the request is withdrawn for it);
// any runner after the fence exits before its command, so the launch is
// wf.ErrNotLaunched with why. A fence that cannot be written leaves it
// unavailable.
func fenceLate(r wf.DetachedRequest, why error) (wf.DetachedProcess, error) {
	p, ok, err := fence(r)
	switch {
	case err != nil:
		return wf.DetachedProcess{}, fmt.Errorf("%w: %v; no fence: %v", wf.ErrUnavailable, why, err)
	case ok:
		return p, nil
	}
	return wf.DetachedProcess{}, fmt.Errorf("%w: %v", wf.ErrNotLaunched, why)
}

// fence writes r's stop request, then reads its proc record once more (the
// starter's half; runDetached records, then reads). A runner whose record came
// first is the launch: the request is withdrawn for it, or its heartbeat would
// stop it, and ok is true. Any runner after the fence exits before its command.
func fence(r wf.DetachedRequest) (p wf.DetachedProcess, ok bool, err error) {
	if err := os.WriteFile(stopPath(r.ResultPath), nil, 0o644); err != nil {
		return wf.DetachedProcess{}, false, err
	}
	if p, ok, err := readProc(r.ProcPath); err == nil && ok {
		_ = os.Remove(stopPath(r.ResultPath))
		return p, true, nil
	}
	return wf.DetachedProcess{}, false, nil
}

// stopUnit is a best-effort end of the transient unit: SIGKILL to every process
// already in its cgroup, stop to cancel a start job still queued or activating,
// and SIGKILL again for anything a start in between left under KillMode=process.
// A start request still in flight to the user manager after all three is not
// covered.
func stopUnit(name string) {
	unit := name + ".service"
	for _, a := range [][]string{
		{"--user", "kill", "--signal=SIGKILL", unit},
		{"--user", "stop", unit},
		{"--user", "kill", "--signal=SIGKILL", unit},
	} {
		_, _ = wf.RunTool(wf.Tool{Bin: "systemctl", Args: a, Timeout: 2 * time.Second})
	}
}

// startForked is the double fork: an intermediate (`__detached --spawn`)
// starts the runner in a session of its own and exits, so the runner is
// reparented to the nearest subreaper or init before Start returns. Start waits
// for the intermediate however long it takes; a deadline already past when it
// exits is a timeout unless the proc record exists.
func (d detached) startForked(r wf.DetachedRequest, log *os.File, args []string) (wf.DetachedProcess, error) {
	null, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return wf.DetachedProcess{}, err
	}
	defer null.Close()
	fmt.Fprintf(log, "%sprocess: double fork\n", launchLine)
	var out bytes.Buffer
	cmd := exec.Command(d.bin, append([]string{DetachedVerb, "--spawn"}, args[1:]...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, &out, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return wf.DetachedProcess{}, fmt.Errorf("%w: %v", wf.ErrNotLaunched, err)
	}
	timer := time.NewTimer(startWait)
	defer timer.Stop()
	// Wait for the intermediate however long it takes: it only starts the runner
	// and exits, and the pid it prints is the only handle on a runner already in a
	// session of its own. The engine reads a Start error as "nothing ran", so a
	// deadline that passes meanwhile is decided below, against that runner.
	if werr := cmd.Wait(); werr != nil {
		return wf.DetachedProcess{}, fmt.Errorf("%w: detached launcher failed: %v: %s", wf.ErrNotLaunched, werr, logTail(r.LogPath))
	}
	var pid int
	if _, err := fmt.Sscan(strings.TrimSpace(out.String()), &pid); err != nil || pid <= 0 {
		return wf.DetachedProcess{}, fmt.Errorf("detached launcher printed no runner pid: %s", logTail(r.LogPath))
	}
	gone := func() bool {
		_, state, ok := procStat(pid)
		return !ok || state == 'Z'
	}
	p, res, err := waitProc(r.ProcPath, gone, timer.C)
	switch {
	case err != nil:
		return wf.DetachedProcess{}, err
	case res == procGone:
		return wf.DetachedProcess{}, fmt.Errorf("%w: detached runner exited before its proc record: %s", wf.ErrNotLaunched, logTail(r.LogPath))
	case res == procTimeout:
		// The runner is its own session leader: kill its group.
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return wf.DetachedProcess{}, fmt.Errorf("detached runner wrote no proc record within %v and could not be killed: %w", startWait, err)
		}
		return wf.DetachedProcess{}, fmt.Errorf("%w: detached runner wrote no proc record within %v; runner killed: %s", wf.ErrNotLaunched, startWait, logTail(r.LogPath))
	}
	return p, nil
}

type procWait int

const (
	procFound procWait = iota
	procGone
	procTimeout
)

// waitProc polls for the proc record until timeout fires. gone, when set,
// reports a runner that exited before writing it (the record is re-read once
// first).
func waitProc(path string, gone func() bool, timeout <-chan time.Time) (wf.DetachedProcess, procWait, error) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if p, ok, err := readProc(path); err != nil {
			return wf.DetachedProcess{}, procFound, err
		} else if ok {
			return p, procFound, nil
		}
		if gone != nil && gone() {
			if p, ok, err := readProc(path); err == nil && ok {
				return p, procFound, nil
			}
			return wf.DetachedProcess{}, procGone, nil
		}
		select {
		case <-timeout:
			return wf.DetachedProcess{}, procTimeout, nil
		case <-tick.C:
		}
	}
}

func readProc(path string) (wf.DetachedProcess, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return wf.DetachedProcess{}, false, nil
	}
	if err != nil {
		return wf.DetachedProcess{}, false, err
	}
	var rec procRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return wf.DetachedProcess{}, false, fmt.Errorf("proc record %s: %w", path, err)
	}
	return wf.DetachedProcess{Pid: rec.Pid, Pgid: rec.Pgid, StartTicks: rec.StartTicks}, true, nil
}

func (detached) Launched(r wf.DetachedRequest) (wf.DetachedProcess, bool, error) {
	return readProc(r.ProcPath)
}

// logTail is the last reasonTail bytes of the log at path ("" when unreadable).
func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(lastBytes(b, reasonTail))
}

// readResult reads the result file: ok is false when it does not exist yet.
func readResult(path string) (wf.RunResult, bool, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return wf.RunResult{}, false, nil
	case err != nil:
		return wf.RunResult{}, false, err
	}
	var res wf.RunResult
	if err := json.Unmarshal(b, &res); err != nil {
		return wf.RunResult{}, false, fmt.Errorf("result file %s: %w", path, err)
	}
	return res, true, nil
}

func (d detached) Status(p wf.DetachedProcess, r wf.DetachedRequest) (wf.DetachedReport, error) {
	if res, ok, err := readResult(r.ResultPath); err != nil || ok {
		return wf.DetachedReport{Status: wf.DetachedPresent, Result: res}, err
	}
	// In the runner's own PID namespace the pid probe is authoritative: a
	// leader verified alive by its start ticks is running however old its
	// heartbeat (a suspended host), and a gone one is gone at once. From any
	// other namespace the pid means nothing, and the heartbeat decides: fresh
	// is running, stale or missing is dead.
	var nonEmpty bool
	if samePidNS(r.ProcPath) {
		var running bool
		if nonEmpty, running = owned(p); running {
			return wf.DetachedReport{Status: wf.DetachedRunning}, nil
		}
	} else if foreignAlive(r.ResultPath) {
		// nonEmpty stays false: another namespace's group is never ours to signal.
		return wf.DetachedReport{Status: wf.DetachedRunning}, nil
	}
	if probeHook != nil {
		probeHook()
	}
	// The result may have landed between the probe and now.
	if res, ok, err := readResult(r.ResultPath); err != nil || ok {
		return wf.DetachedReport{Status: wf.DetachedPresent, Result: res}, err
	}
	st := wf.DetachedDead
	if nonEmpty {
		st = wf.DetachedDeadLive
	}
	return wf.DetachedReport{Status: st, Reason: deathReason(r.LogPath)}, nil
}

// deathReason is why the launch whose log is at path died: what its log says
// after the last launch line, else that it died with nothing logged; the
// whole tail for a log with no launch line.
func deathReason(path string) string {
	tail := logTail(path)
	i := strings.LastIndex(tail, launchLine)
	if i < 0 {
		return tail
	}
	line, rest, _ := strings.Cut(tail[i+len(launchLine):], "\n")
	if rest = strings.TrimSpace(rest); rest != "" {
		return rest
	}
	mode, _, _ := strings.Cut(line, ":")
	return "runner died with no result (mode " + mode + ")"
}

// owned classifies the launch's process group. nonEmpty is true when the group
// has members and is this launch's; running is true when its leader is alive
// (matching start ticks, not a zombie). A group whose leader has other start
// ticks is a recycled pgid: not this launch's, treated as empty. A legacy
// record (no start ticks) is judged by the group alone.
func owned(p wf.DetachedProcess) (nonEmpty, running bool) {
	if p.Pgid <= 0 {
		return false, false
	}
	if err := syscall.Kill(-p.Pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false, false
	}
	if p.StartTicks == 0 {
		return true, true
	}
	ticks, state, ok := procStat(p.Pid)
	switch {
	case !ok:
		return true, false
	case ticks != p.StartTicks:
		return false, false
	}
	return true, state != 'Z'
}

// Terminate signals the launch's group through -pgid whenever it is this
// launch's and non-empty, whether or not its leader is alive: SIGTERM, then
// SIGKILL after terminateGrace. A runner in another PID namespace cannot be
// signalled from here: Terminate writes its stop request instead, which the
// runner answers within a heartbeat. A zero p fences r's paths: a runner
// starting later on them exits before its command, and one whose proc record
// is already there is the launch the engine adopts, so the fence is withdrawn.
func (d detached) Terminate(p wf.DetachedProcess, r wf.DetachedRequest) error {
	if p.Pgid == 0 {
		if r.ResultPath == "" {
			return nil
		}
		_, _, err := fence(r)
		return err
	}
	if !samePidNS(r.ProcPath) {
		if r.ResultPath == "" {
			return nil
		}
		if err := os.WriteFile(stopPath(r.ResultPath), nil, 0o644); err != nil {
			return err
		}
		// Wait, as the same-namespace path does, until the runner has answered:
		// its heartbeat is gone or stale. The answer takes up to one beat plus
		// terminateGrace.
		deadline := time.Now().Add(heartbeatEvery + terminateGrace + time.Second)
		for time.Now().Before(deadline) {
			if present, fresh := heartbeat(r.ResultPath); !present || !fresh {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	if nonEmpty, _ := owned(p); !nonEmpty {
		return nil
	}
	if err := syscall.Kill(-p.Pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(terminateGrace)
	for time.Now().Before(deadline) {
		if empty, _ := d.GroupEmpty(p, r); empty {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-p.Pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// GroupEmpty is true when the group has no members or is not this launch's.
// For a runner in another PID namespace the group cannot be seen: it counts as
// live while the runner's heartbeat is fresh.
func (detached) GroupEmpty(p wf.DetachedProcess, r wf.DetachedRequest) (bool, error) {
	if !samePidNS(r.ProcPath) {
		_, fresh := heartbeat(r.ResultPath)
		return !fresh, nil
	}
	nonEmpty, _ := owned(p)
	return !nonEmpty, nil
}
