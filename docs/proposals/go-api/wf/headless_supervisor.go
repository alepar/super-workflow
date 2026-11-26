package wf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// HeadlessSupervisorVerb is the hidden subcommand the spawn verb re-executes
// the workflow binary as: the supervisor of one headless child. CLIWithEnv
// runs it before anything else; a binary whose main is not MainWith or CLI
// dispatches it with RunHeadlessSupervisor. It is not listed in usage.
const HeadlessSupervisorVerb = "__headless-supervisor"

// headlessBeat is how often the supervisor touches its heartbeat file (a
// var: a test seam). The starter hands it to the supervisor, so the readers
// in this binary and the supervisor agree. A heartbeat older than
// headlessStaleBeats beats with no exit recorded is HeadlessUnknown.
var headlessBeat = 5 * time.Second

const headlessStaleBeats = 4

// headlessStartWait is how long the spawn verb waits for the supervisor to
// record the child's start (a var: a test seam).
var headlessStartWait = 10 * time.Second

// headlessPoll is how often the supervisor checks for a stop request and the
// end of the child's group.
const headlessPoll = 200 * time.Millisecond

// headlessStatus is the supervisor's status file: the child's pid and start
// time once it runs, then its exit. Err is why the child could not start.
type headlessStatus struct {
	Pid    int       `json:",omitempty"`
	Start  uint64    `json:",omitempty"`
	Exited bool      `json:",omitempty"`
	Code   int       `json:",omitempty"`
	At     time.Time `json:",omitzero"`
	Err    string    `json:",omitempty"`
}

// headlessAlivePath is the supervisor's heartbeat file, beside its status
// file, so any process that shares the filesystem can judge it in whatever
// PID namespace it runs.
func headlessAlivePath(status string) string { return status + ".alive" }

// headlessStopPath is the stop request the supervisor watches.
func headlessStopPath(status string) string { return status + ".stop" }

// startSupervised starts this binary as the supervisor of argv in its own
// session, its diagnostics appended to the child's stderr file, and waits
// for it to record the child's start.
func startSupervised(argv, env []string, dir, stdin, stdout, stderr, status string) (int, uint64, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, 0, err
	}
	for _, p := range []string{status, headlessAlivePath(status), headlessStopPath(status)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, 0, err
		}
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return 0, 0, err
	}
	defer null.Close()
	log, err := os.OpenFile(stderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer log.Close()
	args := append([]string{HeadlessSupervisorVerb, "--status", status, "--dir", dir, "--stdin", stdin, "--stdout", stdout, "--stderr", stderr, "--beat", headlessBeat.String(), "--"}, argv...)
	cmd := exec.Command(exe, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, null, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	sup := cmd.Process.Pid
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reaped if it ends while this process lives
	for deadline := time.Now().Add(headlessStartWait); ; {
		if st, err := readHeadlessStatus(status); err == nil {
			switch {
			case st.Err != "":
				return 0, 0, fmt.Errorf("start %s: %s", argv[0], st.Err)
			case st.Pid > 0:
				return st.Pid, st.Start, nil
			}
		}
		select {
		case <-exited:
			if st, err := readHeadlessStatus(status); err == nil && (st.Pid > 0 || st.Err != "") {
				continue // it wrote the status just before it ended
			}
			return 0, 0, fmt.Errorf("the headless supervisor (%s %s) ended before starting %s; see %s", exe, HeadlessSupervisorVerb, argv[0], stderr)
		case <-time.After(headlessPoll / 4):
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(-sup, syscall.SIGKILL)
			return 0, 0, fmt.Errorf("the headless supervisor did not start %s within %s; see %s", argv[0], headlessStartWait, stderr)
		}
	}
}

// supervisedState is a supervised child's state from its supervisor's files:
// exited when it recorded the exit or its status file is gone, running on a
// fresh heartbeat, else unknown.
func supervisedState(status string) HeadlessState {
	st, err := readHeadlessStatus(status)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return HeadlessExited
	case err != nil:
		return HeadlessUnknown
	case st.Exited || st.Err != "":
		return HeadlessExited
	}
	info, err := os.Stat(headlessAlivePath(status))
	if err == nil && time.Since(info.ModTime()) <= headlessStaleBeats*headlessBeat {
		return HeadlessRunning
	}
	return HeadlessUnknown
}

func readHeadlessStatus(path string) (headlessStatus, error) {
	var st headlessStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// writeHeadlessStatus replaces path atomically: a temporary file, fsync,
// rename, so a reader sees the old status or the new one.
func writeHeadlessStatus(path string, st headlessStatus) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return werr
	}
	return os.Rename(tmp.Name(), path)
}

// touch sets path's modification time to now, creating it if need be.
func touch(path string) error {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

// RunHeadlessSupervisor is the hidden verb: it starts `-- argv...` in --dir
// in its own session, stdin from --stdin, stdout and stderr appended to
// their files, and supervises it. It records the child's pid and start
// time in --status, touches the heartbeat file every --beat, and on a stop
// request sends the child's process group SIGTERM, then SIGKILL after
// killGrace. When the child exits it records the exit code and time, then
// stays while any of the child's group remains, so a stop still reaches its
// tool processes. A child that cannot start is recorded with Err. It exits
// 0 once it is done, 2 on a usage or write error (reported on stderr, the
// child's log).
func RunHeadlessSupervisor(args []string, stderr io.Writer) int {
	code := runHeadlessSupervisor(args, stderr)
	if code != 0 {
		fmt.Fprintf(stderr, "%s exited %d\n", HeadlessSupervisorVerb, code)
	}
	return code
}

func runHeadlessSupervisor(args []string, stderr io.Writer) int {
	var status, dir, stdin, stdout, errPath string
	var argv []string
	beat := headlessBeat
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			argv = args[i+1:]
			break
		}
		if i+1 >= len(args) {
			fmt.Fprintf(stderr, "%s: %s needs a value\n", HeadlessSupervisorVerb, args[i])
			return 2
		}
		v := args[i+1]
		switch args[i] {
		case "--status":
			status = v
		case "--dir":
			dir = v
		case "--stdin":
			stdin = v
		case "--stdout":
			stdout = v
		case "--stderr":
			errPath = v
		case "--beat":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				fmt.Fprintf(stderr, "%s: --beat %q: want a positive duration\n", HeadlessSupervisorVerb, v)
				return 2
			}
			beat = d
		default:
			fmt.Fprintf(stderr, "%s: unexpected argument %q\n", HeadlessSupervisorVerb, args[i])
			return 2
		}
		i++
	}
	if status == "" || stdin == "" || stdout == "" || errPath == "" || len(argv) == 0 {
		fmt.Fprintf(stderr, "usage: %s --status <path> --dir <dir> --stdin <path> --stdout <path> --stderr <path> [--beat <d>] -- argv...\n", HeadlessSupervisorVerb)
		return 2
	}
	fail := func(err error) int {
		if werr := writeHeadlessStatus(status, headlessStatus{Exited: true, Code: 127, At: time.Now(), Err: err.Error()}); werr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", HeadlessSupervisorVerb, werr)
			return 2
		}
		return 0
	}
	in, err := os.Open(stdin)
	if err != nil {
		return fail(err)
	}
	defer in.Close()
	out, err := os.OpenFile(stdout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer out.Close()
	errf, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer errf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, in, out, errf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	pid := cmd.Process.Pid
	running := headlessStatus{Pid: pid, Start: procStart(pid)}
	_ = touch(headlessAlivePath(status))
	if err := writeHeadlessStatus(status, running); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", HeadlessSupervisorVerb, err)
		_ = syscall.Kill(-pid, syscall.SIGKILL) // unrecorded, it would run unwatched
		_ = cmd.Wait()
		return 2
	}
	done := make(chan int, 1)
	go func() { done <- exitCode(cmd.Wait(), cmd.ProcessState) }()
	tick := time.NewTicker(headlessPoll)
	defer tick.Stop()
	exited, lastBeat := false, time.Now()
	var termAt time.Time
	for {
		select {
		case code := <-done:
			exited = true
			end := running
			end.Exited, end.Code, end.At = true, code, time.Now()
			if err := writeHeadlessStatus(status, end); err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", HeadlessSupervisorVerb, err)
			}
		case <-tick.C:
		}
		if exited && !groupMembers(pid) {
			return 0
		}
		if now := time.Now(); now.Sub(lastBeat) >= beat {
			_ = touch(headlessAlivePath(status))
			lastBeat = now
		}
		if termAt.IsZero() {
			if _, err := os.Stat(headlessStopPath(status)); err == nil {
				termAt = time.Now()
				_ = syscall.Kill(-pid, syscall.SIGTERM)
			}
		} else if time.Since(termAt) >= killGrace {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

// exitCode is a finished command's exit code: 128 plus the signal for one a
// signal ended.
func exitCode(err error, ps *os.ProcessState) int {
	if err == nil || ps == nil {
		if err != nil {
			return 1
		}
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
