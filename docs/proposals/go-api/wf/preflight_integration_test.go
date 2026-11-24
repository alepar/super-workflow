package wf_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Seam: the model-tiers check (preflight runner) and run creation (run_started
// Models) resolve tiers from the same harness and --model-map.

func tierCLIConfig(probe *wf.PreflightContext) wf.MainConfig {
	reg := wf.Registration{Def: tierDef("p", wf.ModelTier(wf.TierStrong))}
	if probe != nil {
		reg.Preflight = []wf.PreflightCheck{{Name: "probe", Run: func(c wf.PreflightContext) *wf.Refusal { *probe = c; return nil }}}
	}
	return wf.MainConfig{Defs: []wf.Registration{reg}}
}

// journaledModels reads run_started.Models from the run's journal file.
func journaledModels(t *testing.T, state, run string) map[string]string {
	t.Helper()
	return journaledRunStarted(t, state, run).Models
}

// journaledRunStarted reads the run_started entry from the run's journal file.
func journaledRunStarted(t *testing.T, state, run string) wf.RunStartedData {
	t.Helper()
	f, err := os.Open(filepath.Join(state, "runs", run, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var e wf.JournalRecord
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != wf.JournalRunStarted {
			continue
		}
		var s wf.RunStartedData
		if err := json.Unmarshal(e.Data, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Fatal("no run_started entry in the journal")
	return wf.RunStartedData{}
}

func tierCLI(cfg wf.MainConfig, args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := wf.CLI(cfg, args, &o, &er)
	return code, o.String(), er.String()
}

func TestGenericTierMapPassesCheckAndIsJournaled(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var probe wf.PreflightContext
	cfg := tierCLIConfig(&probe)
	if code, _, er := tierCLI(cfg, "run", "tiers", "--state-dir", state, "--run-id", "g", "--harness", "generic", "--model-map", "strong=my-model"); code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	if probe.Harness != "generic" || probe.ModelMap["strong"] != "my-model" {
		t.Fatalf("check context = %+v, want generic and strong=my-model", probe)
	}
	want := map[string]string{"strong": "my-model"}
	if got := journaledModels(t, state, "g"); !reflect.DeepEqual(got, want) {
		t.Errorf("run_started Models = %v, want %v", got, want)
	}
}

// A3c: an unknown harness with no --model-map runs; the tiers are hints.
func TestGenericTierWithoutMapRuns(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	code, _, er := tierCLI(tierCLIConfig(nil), "run", "tiers", "--state-dir", state, "--run-id", "g", "--harness", "generic")
	if code != 0 {
		t.Fatalf("run = %d %q, want a run", code, er)
	}
}

func TestCodexTierOverrideJournaledAndDispatched(t *testing.T) {
	codexEnv(t)
	state := filepath.Join(t.TempDir(), "state")
	var probe wf.PreflightContext
	cfg := tierCLIConfig(&probe)
	if code, _, er := tierCLI(cfg, "run", "tiers", "--state-dir", state, "--run-id", "c", "--harness", "codex", "--model-map", "strong=my-codex"); code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	rs := journaledRunStarted(t, state, "c")
	if rs.Models["strong"] != "my-codex" {
		t.Errorf("run_started Models = %v, want strong=my-codex", rs.Models)
	}
	if rs.Harness != probe.Harness {
		t.Errorf("run_started Harness = %q, preflight saw %q", rs.Harness, probe.Harness)
	}
	code, out, er := tierCLI(cfg, "next", "c", "all", "--state-dir", state, "--json")
	if code != 0 {
		t.Fatalf("next exited %d: %s", code, er)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next = %q, %v", out, err)
	}
	if ins[0].Dispatch.Model != "my-codex" || ins[0].Dispatch.Tier != wf.TierStrong {
		t.Errorf("dispatch = %+v, want model my-codex tier strong", ins[0].Dispatch)
	}
}

// The harness row, not a --model-map, supplies the model here, so the result
// depends on run creation and preflight agreeing on the harness.
func TestCodexHarnessRowResolvesWithoutMap(t *testing.T) {
	codexEnv(t)
	state := filepath.Join(t.TempDir(), "state")
	var probe wf.PreflightContext
	if code, _, er := tierCLI(tierCLIConfig(&probe), "run", "tiers", "--state-dir", state, "--run-id", "r", "--harness", "codex"); code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	rs := journaledRunStarted(t, state, "r")
	if probe.Harness != "codex" || rs.Harness != "codex" {
		t.Errorf("harness: preflight %q, run_started %q, want codex for both", probe.Harness, rs.Harness)
	}
	if want := "gpt-6.2-astra"; rs.Models["strong"] != want {
		t.Errorf("run_started Models = %v, want strong=%s (codex row)", rs.Models, want)
	}
}

// codexEnv makes this test's environment a codex session, so a run created
// with --harness codex matches it.
func codexEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "thread-1")
}
