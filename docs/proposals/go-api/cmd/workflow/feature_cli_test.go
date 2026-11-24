package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestMain(m *testing.M) {
	if os.Getenv("WORKFLOW_TEST_CLI") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// Exercise the shipped registration, not a separate test-only registration:
// reverting cmd/workflow to {Def: feature.Definition()} must fail this test.
func TestFeatureCLIReadsMultilineGoalFile(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	const goal = "Export Add(a, b int) int.\n\n  Test Add from an external package.\n"
	if err := os.WriteFile(filepath.Join(workspace, "goal with spaces.txt"), []byte(goal), 0o644); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "feature", "--workspace", workspace, "--state-dir", state, "--param", "goal=@goal with spaces.txt", "--param", "suite=go test ./..."}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "WORKFLOW_TEST_CLI=1")
	cmd.Dir = t.TempDir() // @path is relative to --workspace, not the invoking cwd.
	var out, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	if err := cmd.Run(); err != nil {
		t.Fatalf("workflow run: %v: %s", err, diagnostics.String())
	}
	id := wf.RunID(strings.TrimSpace(out.String()))
	j := wf.FileJournal(state)
	unlock, err := j.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := j.Records(id)
	if err != nil || len(records) == 0 || records[0].Kind != wf.JournalRunStarted {
		t.Fatalf("run records: %v, %v", records, err)
	}
	var started wf.RunStartedData
	if err := json.Unmarshal(records[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if started.Params["goal"] != goal {
		t.Fatalf("journaled goal = %q; want actual file contents %q", started.Params["goal"], goal)
	}
	if started.Params["suite"] != "go test ./..." {
		t.Fatalf("journaled suite = %q", started.Params["suite"])
	}
	if got := started.Params["goal-check-command"]; got != "go test -count=1 -json" {
		t.Fatalf("default goal check command=%q", got)
	}
}
