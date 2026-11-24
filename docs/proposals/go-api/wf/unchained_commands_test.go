package wf_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// Register a harness with agy's command requirement without depending on
// agy's transcript and identity implementation.
type unchainedAdapter struct{ hn.Generic }

func (unchainedAdapter) Name() string            { return "unchained-test" }
func (unchainedAdapter) Markers() []string       { return []string{"WF_TEST_UNCHAINED"} }
func (unchainedAdapter) UnchainedCommands() bool { return true }
func (a unchainedAdapter) Caller(env hn.Env) hn.Caller {
	return hn.Caller{Harness: a.Name(), Session: env("WF_TEST_UNCHAINED_SESSION"), FromEnv: true}
}

func init() {
	if err := hn.Register(unchainedAdapter{}); err != nil {
		panic(err)
	}
}

func TestCLIPrintsCommandsForTheCallingHarness(t *testing.T) {
	for _, unchained := range []bool{false, true} {
		name, marker := "claude", "WF_TEST_UNCHAINED="
		if unchained {
			name, marker = "unchained-test", "WF_TEST_UNCHAINED=1"
		}
		t.Run(name, func(t *testing.T) {
			e := newCLIEnv(t)
			dir := t.TempDir()
			bin := filepath.Join(dir, "client with a space")
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Link(exe, bin); err != nil {
				t.Fatal(err)
			}
			const run = "command-shape"
			// Long absolute paths force the paged renderer to preserve commands
			// that exceed its normal prose width.
			state := filepath.Join(dir, strings.Repeat("long-state-", 12))
			env := []string{"WF_TEST_NAMED=1", marker}
			other := t.TempDir()
			check := func(body, verb string) string {
				t.Helper()
				for _, raw := range strings.Split(body, "\n") {
					line := strings.TrimSpace(raw)
					if !strings.Contains(line, bin+"' wf "+verb+" ") {
						continue
					}
					want := "'" + bin + "' wf "
					if !unchained {
						want = "cd " + e.ws + " && " + want
					}
					if !strings.HasPrefix(line, want) ||
						!strings.Contains(line, "--state-dir "+state) ||
						!strings.Contains(line, "--run-id "+run) ||
						(unchained && strings.Contains(line, "&&")) {
						t.Fatalf("malformed %s command: %s", verb, line)
					}
					return line
				}
				t.Fatalf("missing whole %s command in:\n%s", verb, body)
				return ""
			}
			code, _, boot := runProc(t, bin, e.ws, env, "run", "agent-cli", "--state-dir", state, "--run-id", run, "--paged-prompts")
			if code != 0 || !strings.Contains(boot, "set the shell tool working-directory (workdir/cwd) to "+e.ws) {
				t.Fatalf("run bootstrap: exit %d, %d bytes:\n%s", code, len(boot), boot)
			}
			next := check(boot, "next")
			code, out, er := runProc(t, bin, other, env, "next", "--state-dir", state, "--run-id", run)
			if code != 0 {
				t.Fatalf("named bootstrap from an unrelated cwd: exit %d\n%s\n%s", code, out, er)
			}
			check(out, "next")
			code, out, er = runProc(t, "/bin/sh", other, env, "-c", next)
			if code != 0 || !strings.Contains(out, "start a child agent for ") {
				t.Fatalf("bootstrap next from an unrelated cwd: exit %d\n%s\n%s", code, out, er)
			}
			prompt := check(out, "prompt")
			code, out, er = runProc(t, "/bin/sh", other, env, "-c", prompt)
			if code != 0 || !strings.Contains(out, "Required prompt page") {
				t.Fatalf("prompt from an unrelated cwd: exit %d\n%s\n%s", code, out, er)
			}
			for _, verb := range []string{"prompt", "validate", "done", "failed"} {
				check(out, verb)
			}
			alias := filepath.Join(dir, "another client name")
			if err := os.Link(bin, alias); err != nil {
				t.Fatal(err)
			}
			viaAlias := strings.Replace(prompt, "'"+bin+"'", "'"+alias+"'", 1)
			if code, refetched, er := runProc(t, "/bin/sh", other, env, "-c", viaAlias); code != 0 || refetched != out {
				t.Fatalf("refetch through a second executable path: exit %d\n%s\n%s", code, refetched, er)
			}
		})
	}
}
