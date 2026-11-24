package wf

import (
	"testing"
	"time"
)

// H2: a blocking next waits for its harness's tool limit less the margin:
// Claude Code's Bash 120 s, Hermes's terminal 30 s (the timeout its models
// pass per call), each as its environment sets it; a harness whose limit is unknown leaves the default (100 s).
func TestNextWaitsForTheHarnessToolLimit(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want time.Duration
	}{
		{"claude", map[string]string{"CLAUDE_CODE_SESSION_ID": "s"}, 110 * time.Second},
		{"claude, longer Bash", map[string]string{"CLAUDE_CODE_SESSION_ID": "s", "BASH_DEFAULT_TIMEOUT_MS": "600000"}, 590 * time.Second},
		{"claude, short Bash", map[string]string{"CLAUDE_CODE_SESSION_ID": "s", "BASH_DEFAULT_TIMEOUT_MS": "8000"}, 4 * time.Second},
		{"claude, Bash under the margin", map[string]string{"CLAUDE_CODE_SESSION_ID": "s", "BASH_DEFAULT_TIMEOUT_MS": "3000"}, 1500 * time.Millisecond},
		{"hermes", map[string]string{"HERMES_SESSION_ID": "h"}, 20 * time.Second},
		{"codex", map[string]string{"CODEX_THREAD_ID": "c"}, 0},
		{"a person's shell", map[string]string{}, 0},
	} {
		co := envCoordinator(func(k string) string { return c.env[k] })
		if co.Wait != c.want {
			t.Errorf("%s: next waits %s, want %s", c.name, co.Wait, c.want)
		}
	}
	if DefaultNextWait != 100*time.Second {
		t.Errorf("the default wait is %s", DefaultNextWait)
	}
}
