package wf

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// The engine the CLI builds, with no ChildLive injected, asks the claim's
// harness adapter (harness.SettlingEnd) whether a process of the child is
// live; a harness without SettlingEnd answers not live.

func cliEngineOpts(t *testing.T) EngineOptions {
	t.Helper()
	c := &cli{stdout: io.Discard, stderr: io.Discard, env: func(string) string { return "" }}
	x, err := c.engine(t.TempDir(), EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return x.(*Engine).opts
}

func TestCLIEngineChildLiveAsksTheAdapter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	opts := cliEngineOpts(t)
	if opts.ChildLive == nil {
		t.Fatal("the CLI-built engine has no ChildLive")
	}
	id := "wf-clilive" + strconv.Itoa(os.Getpid())
	// The marker entry, as the pi adapter spells it: the first word of its resume line.
	marker, _, _ := strings.Cut(harness.For("pi").ResumeLine(id, "m"), " ")
	if live, err := opts.ChildLive("pi", id); err != nil || live {
		t.Fatalf("no process: ChildLive = %v, %v; want false, nil", live, err)
	}
	cmd := startFakePi(t, marker, id)
	if live, err := opts.ChildLive("pi", id); err != nil || !live {
		t.Fatalf("running: ChildLive = %v, %v; want true, nil", live, err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	if live, err := opts.ChildLive("pi", id); err != nil || live {
		t.Fatalf("exited: ChildLive = %v, %v; want false, nil", live, err)
	}
}

// startFakePi starts a stand-in for a launched pi child id: a shell whose
// arguments hold --session-id id, under the marker entry, running a sleep
// that inherits the marker (a descendant, which ChildLive does not count).
// Killing the returned command ends the shell alone; the test's cleanup
// kills its whole process group.
func startFakePi(t *testing.T, marker, id string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 30; true", "sh", "--session-id", id)
	cmd.Env = append(os.Environ(), marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL); cmd.Wait() })
	return cmd
}

func TestCLIEngineChildLiveNilSafe(t *testing.T) {
	opts := cliEngineOpts(t)
	if opts.ChildLive == nil {
		t.Fatal("the CLI-built engine has no ChildLive")
	}
	for _, h := range []string{"claude", "codex", "generic", ""} {
		if _, ok := harness.For(h).(harness.SettlingEnd); ok {
			t.Fatalf("%q now implements SettlingEnd: pick a harness that does not", h)
		}
		if live, err := opts.ChildLive(h, "wf-anything"); err != nil || live {
			t.Errorf("%q: ChildLive = %v, %v; want false, nil", h, live, err)
		}
	}
}

func TestCLIEngineKeepsAnInjectedChildLive(t *testing.T) {
	c := &cli{stdout: io.Discard, stderr: io.Discard, env: func(string) string { return "" }}
	called := false
	x, err := c.engine(t.TempDir(), EngineOptions{ChildLive: func(_, _ string) (bool, error) { called = true; return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if live, _ := x.(*Engine).opts.ChildLive("pi", "wf-x"); !live || !called {
		t.Fatal("an injected ChildLive was replaced by the CLI default")
	}
}
