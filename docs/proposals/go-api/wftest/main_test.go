package wftest_test

// Owns: the wftest test binary's process-wide environment: a run of these
// tests is never an ambient harness's (an agy shell's conversation marker and
// records root included), whatever shell hosts it.

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

// harnessEnvVars are the variables a harness's shell sets that the engine
// reads to detect or classify a caller, or to size a call: every registered
// adapter's markers, every ANTIGRAVITY_* variable in the environment, and the
// non-marker ones the engine reads beside them. Derived from the registry.
// A copy of package wf's test helper: a separate test binary cannot see it.
func harnessEnvVars() []string {
	vars := []string{"CODEX_SESSION_ID", "BASH_DEFAULT_TIMEOUT_MS", "TERMINAL_TIMEOUT", "HERMES_DELEGATED_CHILD_CONTEXT", "HERMES_KANBAN_TASK", "OPENCODE_PARENT_SESSION_ID", "OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS"}
	for _, n := range harness.Names() {
		if d, ok := harness.For(n).(harness.Detector); ok {
			vars = append(vars, d.Markers()...)
		}
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ANTIGRAVITY_") {
			vars = append(vars, k)
		}
	}
	return vars
}

func TestMain(m *testing.M) {
	for _, k := range harnessEnvVars() {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// TestAmbientHarnessEnvIsCleared: re-executed under an agy shell's variables,
// TestMain leaves none of them set.
func TestAmbientHarnessEnvIsCleared(t *testing.T) {
	if os.Getenv("WF_TEST_ENV_PROBE") == "1" {
		for _, k := range []string{"ANTIGRAVITY_APP_DATA_DIR", "ANTIGRAVITY_CONVERSATION_ID"} {
			if v := os.Getenv(k); v != "" {
				t.Errorf("%s = %q survived TestMain", k, v)
			}
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAmbientHarnessEnvIsCleared$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_CONVERSATION_ID=c", "ANTIGRAVITY_APP_DATA_DIR="+t.TempDir(), "WF_TEST_ENV_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe under an agy shell failed: %v\n%s", err, out)
	}
}
