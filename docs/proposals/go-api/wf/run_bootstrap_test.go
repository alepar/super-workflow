package wf_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Catch missing run bootstrap, wrong CLI mode, unquoted paths, and a loop
// pointed at the predecessor rather than the run just created or resumed.
func TestRunPrintsExecutableBootstrap(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "multi", true: "named"}[named], func(t *testing.T) {
			e := newCLIEnv(t)
			e.state = filepath.Join(t.TempDir(), "state dir 'quoted")
			bin := filepath.Join(t.TempDir(), "client 'quoted")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Link(exe, bin); err != nil {
				t.Fatal(err)
			}
			env := []string{"WF_TEST_NAMED=0", "WF_TEST_BOOTSTRAP=1"}
			if named {
				env = []string{"WF_TEST_NAMED=1", "WF_TEST_BOOTSTRAP=1"}
			}
			relativeState, err := filepath.Rel(e.ws, e.state)
			if err != nil {
				t.Fatal(err)
			}
			run := func() (string, string) {
				t.Helper()
				code, out, er := runProc(t, bin, e.ws, env, "run", "gate-cli", "--param", "key=boot", "--state-dir", relativeState)
				if code != 0 {
					t.Fatalf("run: %d: %s", code, er)
				}
				id := strings.TrimSpace(out)
				if !strings.Contains(er, "run: "+id+"\n") {
					t.Fatalf("missing bootstrap for %q: %s", id, er)
				}
				_, tail, ok := strings.Cut(er, "\nloop:\n")
				if !ok {
					t.Fatalf("missing loop: %s", er)
				}
				line, _, _ := strings.Cut(tail, "\n")
				if strings.Contains(line, "{{") {
					t.Fatalf("unfilled loop: %s", line)
				}
				return id, strings.TrimSpace(line)
			}
			first, _ := run()
			resumed, line := run()
			if !strings.HasPrefix(first, "gate-cli-") || resumed != first {
				t.Fatalf("fresh/resumed IDs: %q / %q", first, resumed)
			}
			code, out, er := runProc(t, "/bin/sh", t.TempDir(), env, "-c", line)
			if code != 7 || !strings.Contains(out, "This run is waiting for a person at clGate:") {
				t.Fatalf("execute bootstrap %s: %d, %s, %s", line, code, out, er)
			}
			args := []string{"answer", first, "--gate", "clGate", "--value", "a b'c", "--visit", "1", "--state-dir", e.state}
			if named {
				args = []string{"wf", "answer", "--run-id", first, "--gate", "clGate", "--value", "a b'c", "--visit", "1", "--state-dir", e.state}
			}
			if code, out, er := runProc(t, bin, e.ws, env, args...); code != 0 {
				t.Fatalf("answer: %d, %s, %s", code, out, er)
			}
			if code, out, er := runProc(t, "/bin/sh", t.TempDir(), env, "-c", line); code != 7 {
				t.Fatalf("finish: %d, %s, %s", code, out, er)
			}
			successor, line := run()
			if successor == first {
				t.Fatalf("successor reused %q", first)
			}
			if code, out, er := runProc(t, "/bin/sh", t.TempDir(), env, "-c", line); code != 7 || !strings.Contains(out, "This run is waiting for a person at clGate:") {
				t.Fatalf("successor loop: %d, %s, %s", code, out, er)
			}
			for _, args := range [][]string{
				{"run", "missing", "--run-id", "bad"},
				{"run", "needs-cli", "--run-id", "bad"},
				{"run", "agent-cli", "--run-id", "../bad"},
			} {
				code, out, er := runProc(t, bin, e.ws, env, append(args, "--state-dir", e.state)...)
				if code == 0 || out != "" || strings.Contains(er, "\nloop:\n") {
					t.Errorf("failed run emits bootstrap: %d, %q, %s", code, out, er)
				}
			}
		})
	}
}

// The run is durable before the CLI records its named-binary default. If
// that convenience file cannot be written, callers still need the live ID.
func TestRunPrintsIDBeforePostCreationError(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "run-id"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, er := singleCLI(clAgentDef("bootstrap-error"), "run", "--run-id", "recoverable", "--state-dir", state)
	if code != 6 || !strings.Contains(er, "recording the run id") {
		t.Fatalf("run: exit %d, stderr %q; want sidecar write failure", code, er)
	}
	if out != "recoverable\n" {
		t.Fatalf("run ID hidden by post-creation failure: stdout %q", out)
	}
	if strings.Contains(er, "\nloop:\n") {
		t.Fatalf("failed run printed bootstrap: %s", er)
	}
	runs, err := wf.FileJournal(state).Runs()
	if err != nil || len(runs) != 1 || runs[0] != "recoverable" {
		t.Fatalf("created run lost: %v, %v", runs, err)
	}
}

// A state name never determines who acts: the same next verb labels a unit
// for delegation and leaves its actual work prompt on the child-only page.
func TestNextCLIExplicitlyStartsAChild(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "multi", true: "named"}[named], func(t *testing.T) {
			e := newCLIEnv(t)
			env := []string{"WF_TEST_NAMED=0"}
			if named {
				env = []string{"WF_TEST_NAMED=1"}
			}
			code, out, er := e.proc(e.ws, env, "run", "agent-cli", "--run-id", "child-offer", "--state-dir", e.state)
			if code != 0 {
				t.Fatalf("run: %d, %s, %s", code, out, er)
			}
			args := []string{"next", "child-offer", "1", "--wait", "--state-dir", e.state}
			if named {
				args = []string{"wf", "next", "1", "--wait", "--run-id", "child-offer", "--state-dir", e.state}
			}
			code, out, er = e.proc(e.ws, env, args...)
			if code != 0 || !strings.Contains(out, "start a child agent for ") || !strings.Contains(out, "prompt ") || !strings.Contains(out, "--claim ") {
				t.Fatalf("unit offer does not identify the child action: %d, %s, %s", code, out, er)
			}
			if strings.Contains(out, "Say hello.") || strings.Contains(out, "this needs a person") {
				t.Fatalf("unit offer leaks child work or asks a person: %s", out)
			}
		})
	}
}
