package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// pfConfig serves one agent definition whose registration carries checks.
func pfConfig(checks ...wf.PreflightCheck) wf.MainConfig {
	return wf.MainConfig{Defs: []wf.Registration{{Def: clAgentDef("pf-cli"), Preflight: checks}}}
}

func pfRun(cfg wf.MainConfig, state string, extra ...string) (int, string, string) {
	var o, er bytes.Buffer
	args := append([]string{"run", "pf-cli", "--state-dir", state, "--run-id", "pf"}, extra...)
	code := wf.CLI(cfg, args, &o, &er)
	return code, o.String(), er.String()
}

func pfCheck(name string, calls *[]string, r *wf.Refusal) wf.PreflightCheck {
	return wf.PreflightCheck{Name: name, Run: func(wf.PreflightContext) *wf.Refusal {
		*calls = append(*calls, name)
		return r
	}}
}

func pfStatus(t *testing.T, cfg wf.MainConfig, state string) wf.RunStatus {
	t.Helper()
	var o, er bytes.Buffer
	if code := wf.CLI(cfg, []string{"status", "pf", "--json", "--state-dir", state}, &o, &er); code != 0 {
		t.Fatalf("status exited %d: %s", code, er.String())
	}
	var st wf.RunStatus
	if err := json.Unmarshal(o.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestPreflightRefusalExitsTwoAndCreatesNothing(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var calls []string
	cfg := pfConfig(pfCheck("first", &calls, &wf.Refusal{Cause: "no go", Remedy: "do this"}), pfCheck("second", &calls, &wf.Refusal{Cause: "never", Remedy: "never"}))
	code, out, er := pfRun(cfg, state)
	if code != 2 || out != "" {
		t.Fatalf("run = %d %q, want 2 and no stdout", code, out)
	}
	if want := "refused: first: no go\nfix: do this\n"; !strings.Contains(er, want) {
		t.Errorf("stderr = %q, want it to contain %q", er, want)
	}
	if strings.Contains(er, "second") || strings.Contains(er, "never") {
		t.Errorf("stderr = %q names the second refusal", er)
	}
	if len(calls) != 1 || calls[0] != "first" {
		t.Errorf("checks run = %v, want only first", calls)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("state dir exists after a refusal (stat err %v)", err)
	}
}

func TestPreflightPassingChecksCreateTheRun(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var calls []string
	cfg := pfConfig(pfCheck("a", &calls, nil), pfCheck("b", &calls, nil))
	if code, out, er := pfRun(cfg, state); code != 0 || strings.TrimSpace(out) != "pf" {
		t.Fatalf("run = %d %q %q", code, out, er)
	}
	if strings.Join(calls, ",") != "a,b" {
		t.Errorf("checks run = %v, want a then b", calls)
	}
}

func TestPreflightContextCarriesResolvedHarnessAndModelMap(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var got wf.PreflightContext
	cfg := pfConfig(wf.PreflightCheck{Name: "probe", Run: func(c wf.PreflightContext) *wf.Refusal { got = c; return nil }})
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	ws := t.TempDir()
	if code, _, er := pfRun(cfg, state, "--workspace", ws, "--model-map", "opus=sonnet"); code != 0 {
		t.Fatal(er)
	}
	if got.Harness != "claude" || got.Workspace != ws || got.ModelMap["opus"] != "sonnet" {
		t.Errorf("context = %+v, want detected claude, the workspace and the model map", got)
	}
	if h := pfStatus(t, cfg, state).Harness; h != got.Harness {
		t.Errorf("journaled harness = %q, want the preflight context's %q", h, got.Harness)
	}
}

func TestEmptyJournalIsNotASavedRun(t *testing.T) {
	// An empty journal and one holding only a torn first line (no newline, or
	// a malformed last line) have no committed record: run creates, so
	// preflight runs.
	for name, content := range map[string]string{
		"empty":     "",
		"torn":      `{"Seq":1,"Kind":"run_st`,
		"malformed": "{\"Seq\":1,\"Kind\":\"run_st\n",
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			dir := filepath.Join(state, "runs", "pf")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			var calls []string
			cfg := pfConfig(pfCheck("fresh", &calls, nil))
			if code, out, er := pfRun(cfg, state); code != 0 || strings.TrimSpace(out) != "pf" {
				t.Fatalf("run = %d %q %q", code, out, er)
			}
			if len(calls) != 1 {
				t.Errorf("checks run = %v, want preflight to run for a journal with no committed record", calls)
			}
		})
	}
}

func TestPreflightSkippedOnResume(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if code, _, er := pfRun(pfConfig(), state); code != 0 {
		t.Fatal(er)
	}
	var calls []string
	cfg := pfConfig(pfCheck("late", &calls, &wf.Refusal{Cause: "c", Remedy: "r"}))
	if code, out, er := pfRun(cfg, state); code != 0 || strings.TrimSpace(out) != "pf" {
		t.Fatalf("resume = %d %q %q, want it to skip preflight", code, out, er)
	}
	if len(calls) != 0 {
		t.Errorf("resume ran checks %v", calls)
	}
}

func TestResumeInheritsJournaledHarness(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	// The run is created under codex, where --harness codex matches the
	// environment, and resumed from a claude session.
	t.Setenv("CODEX_THREAD_ID", "thread-1")
	t.Setenv("CLAUDECODE", "")
	cfg := pfConfig()
	if code, _, er := pfRun(cfg, state, "--harness", "codex"); code != 0 {
		t.Fatal(er)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	if code, _, er := pfRun(cfg, state); code != 0 {
		t.Fatalf("resume without --harness exited %d: %s", code, er)
	}
	if h := pfStatus(t, cfg, state).Harness; h != "codex" {
		t.Errorf("harness after resume = %q, want codex", h)
	}
	t.Setenv("CLAUDECODE", "")
	if code, _, er := pfRun(cfg, state); code != 0 {
		t.Fatalf("resume from a generic environment exited %d: %s", code, er)
	}
	if h := pfStatus(t, cfg, state).Harness; h != "codex" {
		t.Errorf("harness after generic resume = %q, want codex", h)
	}
}

func TestResumeWithDifferingExplicitHarnessIsRefused(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfg := pfConfig()
	t.Setenv("CODEX_THREAD_ID", "thread-1")
	t.Setenv("CLAUDECODE", "")
	if code, _, er := pfRun(cfg, state, "--harness", "codex"); code != 0 {
		t.Fatal(er)
	}
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	code, _, er := pfRun(cfg, state, "--harness", "claude")
	if code == 0 || !strings.Contains(er, "different harness") {
		t.Errorf("resume --harness claude = %d %q, want a different-harness refusal", code, er)
	}
}

func TestPreflightContextCarriesResolvedParams(t *testing.T) {
	var got wf.PreflightContext
	reg := wf.Registration{
		Def: clAgentDef("pf-cli"),
		Params: []wf.ParamSpec{
			{Name: "base", Value: "<branch>", Source: "test", Derive: func(wf.RunContext) (string, error) { return "derived-main", nil }},
		},
		Preflight: []wf.PreflightCheck{{Name: "probe", Run: func(c wf.PreflightContext) *wf.Refusal { got = c; return nil }}},
	}
	cfg := wf.MainConfig{Defs: []wf.Registration{reg}}
	if code, _, er := pfRun(cfg, filepath.Join(t.TempDir(), "s1")); code != 0 {
		t.Fatal(er)
	}
	if got.Params["base"] != "derived-main" {
		t.Errorf("derived base = %q, want derived-main", got.Params["base"])
	}
	if code, _, er := pfRun(cfg, filepath.Join(t.TempDir(), "s2"), "--param", "base=explicit-x"); code != 0 {
		t.Fatal(er)
	}
	if got.Params["base"] != "explicit-x" {
		t.Errorf("explicit base = %q, want explicit-x", got.Params["base"])
	}
}
