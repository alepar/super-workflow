package wf

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A reader in another PID namespace (re-executed under unshare by
// TestHeadlessSupervisorAcrossPIDNamespaces): it prints the state it reads
// of the child, whether it can see the child's pid, and its PID namespace,
// then with WF_TEST_HEADLESS_STOP=1 asks for the stop, as a sandboxed
// coordinator's verbs do.
func init() {
	status := os.Getenv("WF_TEST_HEADLESS_READER")
	if status == "" {
		return
	}
	h := HeadlessStartedData{Status: status}
	h.Pid, _ = strconv.Atoi(os.Getenv("WF_TEST_HEADLESS_PID"))
	_, err := os.Stat("/proc/" + strconv.Itoa(h.Pid))
	ns, _ := os.Readlink("/proc/self/ns/pid")
	fmt.Printf("state=%d sees_pid=%v ns=%s\n", (osProcess{}).State(h), err == nil, ns)
	if os.Getenv("WF_TEST_HEADLESS_STOP") == "1" {
		if err := (osProcess{}).Stop(h); err != nil {
			fmt.Println("stop:", err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// startTestChild starts argv under the supervisor with its files in a
// temporary directory, and returns the record a spawn would journal.
func startTestChild(t *testing.T, argv ...string) HeadlessStartedData {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "wf-c0ffee")
	if err := os.WriteFile(base+".brief", []byte("brief\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := HeadlessStartedData{ErrLog: base + ".stderr", Out: base + ".stdout", Status: base + ".status"}
	pid, start, err := (osProcess{}).Start(argv, os.Environ(), dir, base+".brief", h.Out, h.ErrLog, h.Status)
	if err != nil {
		t.Fatal(err)
	}
	h.Pid, h.Start = pid, start
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return h
}

// waitState waits for the child's state to become want.
func waitState(t *testing.T, h HeadlessStartedData, want HeadlessState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for (osProcess{}).State(h) != want {
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(h.ErrLog)
			t.Fatalf("state %d, want %d; stderr:\n%s", (osProcess{}).State(h), want, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The coordinator's verbs may run in another PID namespace than the child
// (Codex's sandbox), where its pid means nothing: a reader there still sees
// the child running through the supervisor's files, and its stop request
// ends the child's whole process group, which the supervisor signals.
func TestHeadlessSupervisorAcrossPIDNamespaces(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PID namespaces are Linux's")
	}
	if err := exec.Command("unshare", "-Urpf", "--mount-proc", "true").Run(); err != nil {
		t.Skipf("unprivileged unshare is not available here: %v", err)
	}
	h := startTestChild(t, "sh", "-c", "sleep 60 & sleep 60 & wait")
	waitState(t, h, HeadlessRunning)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	read := func(stop bool) string {
		cmd := exec.Command("unshare", "-Urpf", "--mount-proc", exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "WF_TEST_HEADLESS_READER="+h.Status, "WF_TEST_HEADLESS_PID="+strconv.Itoa(h.Pid))
		if stop {
			cmd.Env = append(cmd.Env, "WF_TEST_HEADLESS_STOP=1")
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the reader in another PID namespace: %v\n%s", err, out)
		}
		return string(out)
	}
	ownNS, _ := os.Readlink("/proc/self/ns/pid")
	out := read(false)
	if !strings.Contains(out, fmt.Sprintf("state=%d ", HeadlessRunning)) || !strings.Contains(out, "sees_pid=false") || strings.Contains(out, "ns="+ownNS) {
		t.Fatalf("want the child running, its pid invisible, from another namespace (ours %s): %s", ownNS, out)
	}
	read(true)
	waitState(t, h, HeadlessExited)
	deadline := time.Now().Add(killGrace + 5*time.Second)
	for groupMembers(h.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if groupMembers(h.Pid) {
		t.Fatal("the child's tool processes outlived the stop request")
	}
	st, err := readHeadlessStatus(h.Status)
	if err != nil || !st.Exited || st.Code != 128+int(syscall.SIGTERM) || st.Pid != h.Pid {
		t.Errorf("status %+v %v: want the child's exit by SIGTERM", st, err)
	}
	if out := read(false); !strings.Contains(out, fmt.Sprintf("state=%d ", HeadlessExited)) {
		t.Errorf("the other namespace does not see the exit: %s", out)
	}
}

// The supervisor records the child's exit code; a child that cannot start
// is the spawn's error, naming why.
func TestHeadlessSupervisorExitAndStartFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process sessions are POSIX")
	}
	h := startTestChild(t, "sh", "-c", "echo out; echo err >&2; exit 7")
	waitState(t, h, HeadlessExited)
	if st, err := readHeadlessStatus(h.Status); err != nil || st.Code != 7 {
		t.Errorf("status %+v %v: want exit 7", st, err)
	}
	if b, _ := os.ReadFile(h.Out); string(b) != "out\n" {
		t.Errorf("stdout %q", b)
	}
	if b, _ := os.ReadFile(h.ErrLog); !strings.Contains(string(b), "err\n") {
		t.Errorf("stderr %q", b)
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "wf-dead")
	_ = os.WriteFile(base+".brief", nil, 0o600)
	_, _, err := (osProcess{}).Start([]string{filepath.Join(dir, "no-such-cli")}, os.Environ(), dir, base+".brief", base+".stdout", base+".stderr", base+".status")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "no-such-cli")) {
		t.Errorf("want the start failure named, got %v", err)
	}
}

// State is read from the files alone: no status file is gone, a recorded
// exit is exited, a fresh heartbeat is running, and a stale heartbeat with
// no exit is unknown, never dead.
func TestHeadlessStateFromFiles(t *testing.T) {
	status := filepath.Join(t.TempDir(), "wf-x.status")
	h := HeadlessStartedData{Pid: 1 << 22, Status: status} // a pid nobody here has
	if got := (osProcess{}).State(h); got != HeadlessExited {
		t.Errorf("no status file: %d, want exited", got)
	}
	if err := writeHeadlessStatus(status, headlessStatus{Pid: h.Pid}); err != nil {
		t.Fatal(err)
	}
	if err := touch(headlessAlivePath(status)); err != nil {
		t.Fatal(err)
	}
	if got := (osProcess{}).State(h); got != HeadlessRunning {
		t.Errorf("fresh heartbeat, pid invisible: %d, want running", got)
	}
	old := time.Now().Add(-headlessStaleBeats*headlessBeat - time.Second)
	if err := os.Chtimes(headlessAlivePath(status), old, old); err != nil {
		t.Fatal(err)
	}
	if got := (osProcess{}).State(h); got != HeadlessUnknown {
		t.Errorf("stale heartbeat, no exit: %d, want unknown", got)
	}
	if err := writeHeadlessStatus(status, headlessStatus{Pid: h.Pid, Exited: true, Code: 0, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := (osProcess{}).State(h); got != HeadlessExited {
		t.Errorf("recorded exit: %d, want exited", got)
	}
	if err := (osProcess{}).Stop(h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(headlessStopPath(status)); err != nil {
		t.Errorf("Stop wrote no request: %v", err)
	}
}
