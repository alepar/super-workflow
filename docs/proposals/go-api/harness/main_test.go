package harness

// Owns: the harness test binary's process-wide environment: a run of these
// tests is never an ambient harness's (an agy shell's conversation marker and
// records root included), whatever shell hosts it.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// harnessEnvVars are the variables a harness's shell sets that the engine
// reads to detect or classify a caller, or to size a call: every registered
// adapter's markers, every ANTIGRAVITY_* variable in the environment, and the
// non-marker ones the engine reads beside them. Derived from the registry.
// A copy of package wf's test helper: a separate test binary cannot see it.
func harnessEnvVars() []string {
	vars := []string{"CODEX_SESSION_ID", "BASH_DEFAULT_TIMEOUT_MS", "TERMINAL_TIMEOUT", "HERMES_DELEGATED_CHILD_CONTEXT", "HERMES_KANBAN_TASK"}
	for _, n := range Names() {
		if d, ok := For(n).(Detector); ok {
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
// TestMain leaves none of them set and no harness is detected.
func TestAmbientHarnessEnvIsCleared(t *testing.T) {
	if os.Getenv("WF_TEST_ENV_PROBE") == "1" {
		for _, k := range []string{"ANTIGRAVITY_APP_DATA_DIR", "ANTIGRAVITY_CONVERSATION_ID"} {
			if v := os.Getenv(k); v != "" {
				t.Errorf("%s = %q survived TestMain", k, v)
			}
		}
		if got := Detect(os.Getenv); got == "agy" {
			t.Errorf("Detect = %q under a cleared environment", got)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAmbientHarnessEnvIsCleared$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_CONVERSATION_ID=c", "ANTIGRAVITY_APP_DATA_DIR="+t.TempDir(), "WF_TEST_ENV_PROBE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe under an agy shell failed: %v\n%s", err, out)
	}
}
