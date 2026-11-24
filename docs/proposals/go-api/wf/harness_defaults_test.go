package wf_test

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// slotCount matches any slot wording in engine output: the engine has no cap,
// so no journal, status or bootstrap output may name one.
var slotCount = regexp.MustCompile(`(?i)slot`)

// namesSlot reports slot wording in out once the paths it prints (the
// checkout and the state dir) are taken out: a checkout path that happens to
// hold the word is not the engine naming a count.
func namesSlot(t *testing.T, out string, paths ...string) bool {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{wd}, paths...) {
		out = strings.ReplaceAll(out, p, "<path>")
		if j, err := json.Marshal(p); err == nil {
			out = strings.ReplaceAll(out, strings.Trim(string(j), `"`), "<path>")
		}
	}
	return slotCount.MatchString(out)
}

// These cases catch wrong default selection, dropped overrides and inconsistent
// journal/status/bootstrap propagation through the real CLI.
func TestRunPersistsHarness(t *testing.T) {
	for _, tc := range []struct {
		name, harness, codex, claude string
		wantHarness                  string
		bootHarness                  string // the bootstrap's line is the caller's harness (A3a)
	}{
		{name: "claude", harness: "Claude-Code", wantHarness: "claude"},
		{name: "codex", harness: "CODEX", wantHarness: "codex"},
		{name: "generic", harness: "generic", codex: "parent", wantHarness: "generic", bootHarness: "codex"},
		{name: "unknown", harness: "Other", wantHarness: "other"},
		{name: "codex-env", codex: "parent", claude: "1", wantHarness: "codex"},
		{name: "claude-env", claude: "1", wantHarness: "claude"},
		{name: "no-env", wantHarness: "generic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_THREAD_ID", tc.codex)
			t.Setenv("CLAUDECODE", tc.claude)
			t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
			def := clAgentDef("capacity")
			state := t.TempDir()
			args := []string{"run", "--state-dir", state, "--run-id", "r1", "--model-map", "strong=opus"}
			if tc.harness != "" {
				args = append(args, "--harness", tc.harness)
			}
			mustSingle(t, def, args...)
			raw := mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)
			var got struct {
				Harness       string
				LegacyHarness bool
			}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Harness != tc.wantHarness || got.LegacyHarness {
				t.Fatalf("status harness = %+v; want %s", got, tc.wantHarness)
			}
			j := wf.FileJournal(state)
			unlock := lock(t, j, "r1")
			recs, err := j.Records("r1")
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]any
			if err := json.Unmarshal(recs[0].Data, &stored); err != nil {
				t.Fatal(err)
			}
			if stored["Harness"] != tc.wantHarness {
				t.Fatalf("run_started = %+v", stored)
			}
			for _, out := range []string{raw, string(recs[0].Data)} {
				if namesSlot(t, out, state) {
					t.Errorf("slot count persisted or reported: %s", out)
				}
			}
			for _, args := range [][]string{{"wf", "status", "--full", "--state-dir", state}, {"next", "--state-dir", state}} {
				line := "harness: " + tc.wantHarness + "\n"
				if args[0] == "next" && tc.bootHarness != "" {
					line = "harness: " + tc.bootHarness + "\n"
				}
				out := mustSingle(t, def, args...)
				if !strings.Contains(out, line) {
					t.Errorf("missing persisted harness %q: %s", line, out)
				}
				if namesSlot(t, out, state) {
					t.Errorf("%v names a slot count: %s", args, out)
				}
			}
		})
	}
}

// --slots is gone: the engine keeps no cap, so the flag is a usage error and
// nothing is journaled.
func TestRunRefusesSlotsFlag(t *testing.T) {
	state := t.TempDir()
	code, _, er := singleCLI(clAgentDef("capacity"), "run", "--state-dir", state, "--run-id", "r1", "--slots", "3")
	if code != 2 || !strings.Contains(er, "slots") {
		t.Fatalf("exit %d, stderr %q", code, er)
	}
	runs, err := wf.FileJournal(state).Runs()
	if err != nil || len(runs) != 0 {
		t.Fatalf("refused run journaled: %v %v", runs, err)
	}
}

func TestResumePreservesHarness(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "original")
	t.Setenv("CLAUDECODE", "")
	def := clAgentDef("capacity")
	state := t.TempDir()
	args := []string{"run", "--state-dir", state, "--run-id", "r1", "--model-map", "strong=opus"}
	mustSingle(t, def, args...)
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	for _, extra := range [][]string{nil, {"--harness", "codex"}, {"--harness", "CODEX"}} {
		mustSingle(t, def, append(args, extra...)...)
	}
	after := mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)
	var b map[string]any
	json.Unmarshal([]byte(after), &b)
	if b["Harness"] != "codex" {
		t.Fatalf("resume changed harness: %s", after)
	}
	if code, _, er := singleCLI(def, append(args, "--harness", "claude")...); code == 0 || !strings.Contains(er, "different") {
		t.Errorf("conflicting resume: exit %d stderr %q", code, er)
	}
	j := wf.FileJournal(state)
	unlock := lock(t, j, "r1")
	defer unlock()
	recs, err := j.Records("r1")
	if err != nil || len(recs) != 1 {
		t.Fatalf("resume rewrote journal: %d %v", len(recs), err)
	}
}

// A journal written while the engine still recorded a slot count loads: the
// field is ignored, never migrated, and no output repeats it.
func TestResumeJournalWithSlotsLoads(t *testing.T) {
	def := clAgentDef("old-journal")
	j := wf.MemJournal()
	unlock := lock(t, j, "old")
	data := []byte(`{"Harness":"codex","Slots":20,"Def":"old-journal","Version":1,"Models":{"strong":"opus"}}`)
	if err := j.Append(wf.JournalRecord{Run: "old", Kind: wf.JournalRunStarted, DefHash: def.Hash(), Data: data}); err != nil {
		t.Fatal(err)
	}
	unlock()
	e := wf.NewEngine(j, nil, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "old"})
	if _, err := e.Run(def, nil, nil); err != nil {
		t.Fatalf("resume of a journal with slots: %v", err)
	}
	st, err := e.Status("old")
	if err != nil {
		t.Fatal(err)
	}
	if st.Harness != "codex" || st.LegacyHarness {
		t.Fatalf("status harness = %q legacy %v, want codex", st.Harness, st.LegacyHarness)
	}
	ins, err := e.HandOut("old", -1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut on the resumed run = %v, %v; want its one unit", ins, err)
	}
	raw, _ := json.Marshal(st)
	for _, out := range []string{string(raw), wf.RenderStatus(st), wf.Bootstrap(st, "super", "state")} {
		if slotCount.MatchString(out) {
			t.Errorf("output names a slot count: %s", out)
		}
	}
	unlock = lock(t, j, "old")
	recs, _ := j.Records("old")
	unlock()
	if string(recs[0].Data) != string(data) {
		t.Fatalf("resume rewrote run_started: %s", recs[0].Data)
	}
}

func TestHarnessLegacy(t *testing.T) {
	def := clAgentDef("legacy-capacity")
	j := wf.MemJournal()
	unlock := lock(t, j, "old")
	data, _ := json.Marshal(wf.RunStartedData{Def: def.Name(), Version: 1})
	if err := j.Append(wf.JournalRecord{Run: "old", Kind: wf.JournalRunStarted, DefHash: def.Hash(), Data: data}); err != nil {
		t.Fatal(err)
	}
	unlock()
	e := wf.NewEngine(j, nil, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "old"})
	st, err := e.Status("old")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	var legacy struct {
		Harness       string
		LegacyHarness bool
	}
	json.Unmarshal(raw, &legacy)
	if legacy.Harness != "generic" || !legacy.LegacyHarness {
		t.Fatalf("legacy harness = %+v", legacy)
	}
	if _, err := e.Run(def, nil, nil); err != nil {
		t.Fatalf("legacy resume: %v", err)
	}
	for _, out := range []string{wf.RenderStatus(st), wf.Bootstrap(st, "super", "state")} {
		if !strings.Contains(out, "legacy generic fallback") {
			t.Errorf("unmarked legacy: %s", out)
		}
	}
	unlock = lock(t, j, "old")
	recs, _ := j.Records("old")
	unlock()
	if len(recs) != 1 || strings.Contains(string(recs[0].Data), "Harness") {
		t.Fatal("legacy inspection rewrote journal")
	}
}

// The loop's n is the coordinator's own number: the bootstrap prescribes
// none and names no slot count or budget.
func TestBootstrapTakeGuidance(t *testing.T) {
	out := wf.Bootstrap(wf.RunStatus{Run: "run", Harness: "codex"}, "./super", "/state")
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "n = ") && strings.ContainsAny(l, "0123456789") {
			t.Errorf("guidance prescribes a number: %q", l)
		}
	}
	if slotCount.MatchString(out) || strings.Contains(out, "remaining budget") {
		t.Errorf("bootstrap names a slot count or budget: %s", out)
	}
}

// agyTierDef has one agent per dispatch tier.
func agyTierDef() *wf.Def {
	d := wf.New("agy-tiers")
	for _, tier := range []wf.Tier{wf.TierLight, wf.TierStandard, wf.TierStrong, wf.TierTop} {
		a := wf.NewAgent[struct{}, struct{}](d)
		a.Dispatch(wf.ModelTier(tier))
	}
	return d
}

// an earlier revision: an agy run resolves every tier to inherit with no degraded
// note, and a --model-map value agy does not take is refused.
func TestAgyRunResolvesTiersToInherit(t *testing.T) {
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "conv-1")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	d := agyTierDef()
	h := newHarnessOpts(t, d, nil, wf.EngineOptions{Harness: "agy"})
	run := h.run(d, nil)
	recs := h.kind(run, wf.JournalRunStarted)
	if len(recs) != 1 {
		t.Fatalf("run_started records = %d", len(recs))
	}
	var started wf.RunStartedData
	if err := json.Unmarshal(recs[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if len(started.Models) != 4 {
		t.Fatalf("models = %v, want the four tiers", started.Models)
	}
	for tier, m := range started.Models {
		if m != "inherit" {
			t.Errorf("%s = %q, want inherit", tier, m)
		}
	}
	for _, r := range h.kind(run, wf.JournalDegraded) {
		var dg wf.DegradedData
		if err := json.Unmarshal(r.Data, &dg); err == nil && dg.Kind == wf.DegradedTiers {
			t.Errorf("agy run recorded a tiers note: %+v", dg)
		}
	}

	d2 := agyTierDef()
	bad := newHarnessOpts(t, d2, nil, wf.EngineOptions{Harness: "agy", ModelMap: map[string]string{"light": "gemini-3.8-flash-high"}})
	if _, err := bad.e.Run(d2, nil, nil); err == nil || !strings.Contains(err.Error(), "light=gemini-3.8-flash-high") || !strings.Contains(err.Error(), "inherit, flash_lite, flash, pro") {
		t.Fatalf("Run with a model id = %v, want the bad-map refusal", err)
	}

	code, out, er := singleCLI(clAgentDef("agy-cli"), "run", "--run-id", "r1", "--harness", "agy", "--model-map", "light=gemini-3.8-flash-high", "--state-dir", t.TempDir(), "--workspace", t.TempDir())
	if code == 0 || !strings.Contains(out+er, "light=gemini-3.8-flash-high") || !strings.Contains(out+er, "inherit, flash_lite, flash, pro") {
		t.Fatalf("CLI = %d %q %q", code, out, er)
	}
}
