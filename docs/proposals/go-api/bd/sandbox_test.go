package bd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/bd"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A run restored from a snapshot (a fork, a rewind, a manufactured fork)
// must never reach a real remote: every bd call it makes carries --sandbox,
// which turns off bd's sync to a configured remote. A source run's calls do
// not (bd config review).
func TestForkedRunsCallBdSandboxed(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "bd")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\necho '[]'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rt := bd.Runtime(bd.Config{Bin: bin})
	read, ok := rt.(wf.ContextualObservationRuntime)
	if !ok {
		t.Fatal("the bd runtime takes no observation context")
	}
	if _, ok := rt.(wf.ContextualEffectRuntime); !ok {
		t.Fatal("the bd runtime takes no effect context")
	}
	desc := wf.ObservationDesc{Adapter: "bd", Op: "list", Args: map[string]any{"label": "x"}}
	for _, sandboxed := range []bool{false, true} {
		if err := os.WriteFile(log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := read.ReadAt(dir, wf.EffectContext{Run: "r", Sandboxed: sandboxed}, desc); err != nil {
			t.Fatal(err)
		}
		calls, _ := os.ReadFile(log)
		if got := strings.HasPrefix(string(calls), "--sandbox "); got != sandboxed {
			t.Errorf("sandboxed=%v: bd was called with %q", sandboxed, calls)
		}
	}
}

// A sandboxed bd call runs with an empty environment but PATH and HOME: it
// never sees bd's storage overrides, BEADS_DB and the like, which would
// point it at another database (bd config review rounds 1 and 2).
func TestSandboxedBdCallsDropStorageOverrides(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "env")
	bin := filepath.Join(dir, "bd")
	script := "#!/bin/sh\nenv > " + log + "\necho '[]'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_DB", "/elsewhere/beads.db")
	t.Setenv("BD_DOLT_HOST", "10.0.0.2")
	t.Setenv("BD_ACTOR", "fork-agent")
	read := bd.Runtime(bd.Config{Bin: bin}).(wf.ContextualObservationRuntime)
	desc := wf.ObservationDesc{Adapter: "bd", Op: "list", Args: map[string]any{"label": "x"}}
	for _, sandboxed := range []bool{false, true} {
		if _, err := read.ReadAt(dir, wf.EffectContext{Run: "r", Sandboxed: sandboxed}, desc); err != nil {
			t.Fatal(err)
		}
		env, _ := os.ReadFile(log)
		for _, name := range []string{"BEADS_DB=", "BD_DOLT_HOST="} {
			if got := strings.Contains(string(env), "\n"+name) || strings.HasPrefix(string(env), name); got == sandboxed {
				t.Errorf("sandboxed=%v: %s seen: %v", sandboxed, name, got)
			}
		}
		// A sandboxed call keeps PATH and HOME only (review round 2).
		if got := strings.Contains(string(env), "BD_ACTOR=fork-agent"); got == sandboxed {
			t.Errorf("sandboxed=%v: BD_ACTOR seen: %v", sandboxed, got)
		}
		// Metrics are off on every call: bd's first-run notice would prefix
		// the errors the adapter quotes (the regression case seq 2826).
		if !strings.Contains(string(env), "BD_DISABLE_METRICS=1\n") {
			t.Errorf("sandboxed=%v: BD_DISABLE_METRICS=1 not set:\n%s", sandboxed, env)
		}
		if !strings.Contains(string(env), "HOME=") || !strings.Contains(string(env), "PATH=") {
			t.Errorf("sandboxed=%v: PATH or HOME dropped:\n%s", sandboxed, env)
		}
	}
}
