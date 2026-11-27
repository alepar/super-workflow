package wf_test

// The wiring sweep of the subagent driver: every EngineOptions field, every
// documented flag and every journal record kind must reach something. A new
// one that nobody wired fails here by name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// engineOnlyOptions are the EngineOptions fields no CLI flag or the binary's
// wiring sets: a test seam or a tuning default of the engine itself. A field
// not here and not set by main_run.go is unwired.
var engineOnlyOptions = map[string]string{
	"Clock":           "test seam: default time.Now",
	"Nonce":           "test seam: default crypto/rand hex",
	"HandOutWindow":   "tuning default (2m)",
	"SpawnWindow":     "tuning default (3m)",
	"FetchedLease":    "tuning default (30m)",
	"UnstartedGrace":  "tuning default (5m)",
	"LostCap":         "tuning default (5)",
	"EffectPoll":      "tuning default (2s)",
	"ProgressEvery":   "tuning default (30s)",
	"StallDefault":    "tuning default (20m)",
	"TranscriptMtime": "test seam: default os.Stat's ModTime",
	"HeadlessProcess": "test seam: default the host's processes (setsid start, /proc liveness)",
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestEveryEngineOptionWired: each EngineOptions field is set by the CLI's
// engine construction in main_run.go, or is listed as engine-only above.
func TestEveryEngineOptionWired(t *testing.T) {
	src := readSource(t, "main_run.go") + readSource(t, "child_claim.go")
	wired := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bopts\.(\w+)\s*(?:=|\+=)`).FindAllStringSubmatch(src, -1) {
		wired[m[1]] = true
	}
	for _, lit := range regexp.MustCompile(`EngineOptions\{([^}]*)\}`).FindAllStringSubmatch(src, -1) {
		for _, m := range regexp.MustCompile(`(\w+):`).FindAllStringSubmatch(lit[1], -1) {
			wired[m[1]] = true
		}
	}
	typ := reflect.TypeFor[wf.EngineOptions]()
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		fields[name] = true
		_, engineOnly := engineOnlyOptions[name]
		switch {
		case wired[name] && engineOnly:
			t.Errorf("EngineOptions.%s is set by main_run.go but listed as engine-only: drop it from the allowlist", name)
		case !wired[name] && !engineOnly:
			t.Errorf("EngineOptions.%s is set nowhere in main_run.go and is not an engine-only field: unwired", name)
		}
	}
	for name := range engineOnlyOptions {
		if !fields[name] {
			t.Errorf("engine-only allowlist names %s, which is not an EngineOptions field", name)
		}
	}
}

// wiEnv is a multi-definition CLI over its own state directory, with the extra
// definitions the flag probes need.
type wiEnv struct {
	t     *testing.T
	cfg   wf.MainConfig
	state string
	ws    string
	vars  map[string]string // placeholders of the probe's arguments
	// undo restores what a scenario changed for its probe alone (probeRun).
	undo []func()
}

type wiOpts struct{ Fast, Autonomous bool }

func wiConfig() wf.MainConfig {
	cfg := clConfig()
	opts := clAgentDef("opts-cli")
	wf.Options[wiOpts](opts)
	fan := fanDef("fan-opts", chItems(1, 2), 2, wf.AllSettled())
	wf.Options[wiOpts](fan)
	cfg.Defs = append(cfg.Defs, wf.Registration{Def: intGateCLIDef()}, wf.Registration{Def: opts}, wf.Registration{Def: hoGateFan(2)}, wf.Registration{Def: fan})
	cfg.Defs = append(cfg.Defs, familyDefs()...)
	return cfg
}

func newWiEnv(t *testing.T) *wiEnv {
	t.Helper()
	return &wiEnv{t: t, cfg: wiConfig(), state: filepath.Join(t.TempDir(), "state"), ws: t.TempDir(), vars: map[string]string{}}
}

func (w *wiEnv) call(args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := fixtureCLI(w.cfg, args, &o, &er)
	return code, o.String(), er.String()
}

// verb runs a run-driving verb: args are the verb, run and positionals; the
// state dir is added.
func (w *wiEnv) verb(args ...string) (int, string, string) {
	return w.call(append(args, "--state-dir", w.state)...)
}

func (w *wiEnv) must(args ...string) string {
	w.t.Helper()
	code, out, er := w.verb(args...)
	if code != 0 {
		w.t.Fatalf("%v exited %d: %s", args, code, er)
	}
	return out
}

// legacyAppend appends a record to run w1's journal as an older engine wrote
// it, stamped with the run's definition hash.
func (w *wiEnv) legacyAppend(kind wf.JournalKind, state string, attempt int, class wf.OutcomeClass, data any) {
	w.t.Helper()
	j := wf.FileJournal(w.state)
	unlock := lock(w.t, j, "w1")
	defer unlock()
	recs, err := j.Records("w1")
	if err != nil || len(recs) == 0 {
		w.t.Fatalf("records: %v (%d)", err, len(recs))
	}
	raw, err := json.Marshal(data)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := j.Append(wf.JournalRecord{Run: "w1", Kind: kind, State: state, Attempt: attempt, Class: class, DefHash: recs[0].DefHash, At: recs[len(recs)-1].At, Data: raw}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *wiEnv) start(def string) {
	w.t.Helper()
	w.must("run", def, "--workspace", w.ws, "--run-id", "w1")
}

func (w *wiEnv) instructions(args ...string) []wf.Instruction {
	w.t.Helper()
	var ins []wf.Instruction
	code, out, er := w.verb(args...)
	if code != 0 && code != 7 {
		w.t.Fatalf("%v exited %d: %s", args, code, er)
	}
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		w.t.Fatal(err)
	}
	return ins
}

func (w *wiEnv) file(content string) string {
	w.t.Helper()
	f := filepath.Join(w.t.TempDir(), "f.json")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return f
}

// Scenarios: each makes a fresh run and fills the placeholders.
var wiScenarios = map[string]func(w *wiEnv){
	"paged": wiPagedScenario,
	"paged-ref": func(w *wiEnv) {
		wiPagedScenario(w)
		w.must("read", "w1", w.vars["{ref}"], "--claim", w.vars["{claim}"])
		time.Sleep(510 * time.Millisecond)
	},
	"none": func(w *wiEnv) {},
	"agent": func(w *wiEnv) {
		w.start("agent-cli")
		w.vars["{name}"] = w.instructions("next", "w1", "1", "--json")[0].Name
	},
	"claimed": func(w *wiEnv) {
		w.start("agent-cli")
		in := w.instructions("next", "w1", "1", "--json")[0]
		n := in.Name
		w.vars["{out}"] = writeAllocated(w.t, in.Output, `{"OK":true}`)
		w.vars["{name}"] = n
		w.must("dev", "started", "w1", n, "--session", "s1", "--agent", "a1")
	},
	"fresh": func(w *wiEnv) {
		w.start("agent-cli")
	},
	"stopped": func(w *wiEnv) {
		w.start("agent-cli")
		w.must("stop", "w1", "--reason", "move")
		// Only a person clears a stop: resume runs from a plain shell, for
		// this probe alone.
		for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "HERMES_SESSION_ID"} {
			if v, ok := os.LookupEnv(k); ok {
				w.undo = append(w.undo, func() { os.Setenv(k, v) })
				os.Unsetenv(k)
			}
		}
	},
	"fan": func(w *wiEnv) {
		w.start("fan-cli")
		ins := w.instructions("next", "w1", "2", "--json")
		w.vars["{name}"], w.vars["{child}"] = ins[0].Name, ins[0].Child
	},
	"fan-fresh": func(w *wiEnv) {
		w.start("fan-cli")
	},
	"gate": func(w *wiEnv) {
		w.start("gate-cli")
		w.vars["{gate}"] = w.instructions("next", "w1", "all", "--json")[0].State
	},
	"integergate": func(w *wiEnv) {
		w.start("integer-gate-cli")
		w.vars["{gate}"] = w.instructions("next", "w1", "all", "--json")[0].State
	},
	"childgate": func(w *wiEnv) {
		w.start("ho-gate-fan")
		for _, in := range w.instructions("next", "w1", "2", "--json") {
			w.must("dev", "started", "w1", in.Name, "--session", "s1", "--agent", "a1")
			w.must("done", "w1", in.Name, "--output", writeAllocated(w.t, in.Output, `{"OK":true}`))
		}
		for _, in := range w.instructions("next", "w1", "all", "--json") {
			if in.Kind == wf.InstructionHumanGate {
				w.vars["{gate}"], w.vars["{child}"], w.vars["{visit}"] = in.State, in.Child, fmt.Sprint(in.Visit)
			}
		}
	},
	"effect": func(w *wiEnv) {
		w.start("cmd-cli")
		bin, err := os.Executable()
		if err != nil {
			w.t.Fatal(err)
		}
		if code, _, _ := runProc(w.t, bin, "", []string{"WF_TEST_CRASH=1"}, "next", "w1", "all", "--state-dir", w.state); code != 3 {
			w.t.Fatalf("crashing next exited %d, want 3", code)
		}
		w.vars["{effect}"] = fmt.Sprint(w.instructions("next", "w1", "all", "--json")[0].Effect)
	},
	"exhausted": func(w *wiEnv) {
		// The first attempt fails; the second's exhaustion is the halt for
		// retry or stop an engine before C3 journaled (no engine writes one
		// now, but such a journal still folds and is answered).
		w.start("agent-cli")
		n := w.instructions("next", "w1", "1", "--json")[0].Name
		w.must("dev", "started", "w1", n, "--session", "s1", "--agent", "a1")
		w.must("failed", "w1", n, "--reason", "no")
		w.legacyAppend(wf.JournalRefused, "clAsk", 2, wf.Refused,
			wf.RefusedData{Reasons: []string{"abandoned: no"}, Fingerprint: "abandoned: no", Spent: true, Halt: true, Exhausted: wf.ExhaustedBudget})
	},
	"rehashed": func(w *wiEnv) {
		// A run at an older definition hash than the binary's, with a unit
		// handed out: the journal says it migrated to a hash no binary has.
		w.start("agent-cli")
		w.instructions("next", "w1", "1", "--json")
		j := wf.FileJournal(w.state)
		unlock := lock(w.t, j, "w1")
		recs, err := j.Records("w1")
		unlock()
		if err != nil {
			w.t.Fatal(err)
		}
		w.legacyAppend(wf.JournalMigrated, "", 0, 0, wf.MigratedData{FromHash: recs[0].DefHash, ToHash: "sha256:0000000000000000"})
	},
	"ended": func(w *wiEnv) {
		w.start("agent-cli")
		in := w.instructions("next", "w1", "1", "--json")[0]
		w.must("dev", "started", "w1", in.Name, "--session", "s1", "--agent", "a1")
		w.must("done", "w1", in.Name, "--output", writeAllocated(w.t, in.Output, `{"OK":true}`))
	},
}

var (
	hexRun  = regexp.MustCompile(`[0-9a-f]{16,}`)
	timeRun = regexp.MustCompile(`\d{4}-\d\d-\d\dT[\d:.]+(?:Z|[+-]\d\d:\d\d)`)
)

// observe is everything one call did: its exit code and streams and the run's
// journal afterwards, with random tokens, clock stamps and temp paths masked.
func (w *wiEnv) observe(code int, out, er string) string {
	_, journal, _ := w.verb("journal", "w1")
	s := fmt.Sprintf("exit %d\nstdout %s\nstderr %s\njournal %s", code, out, er, journal)
	s = strings.NewReplacer(w.state, "STATE", w.ws, "WS").Replace(s)
	return timeRun.ReplaceAllString(hexRun.ReplaceAllString(s, "H"), "T")
}

// probeRun builds a fresh scenario and runs one command line in it; "{x}"
// placeholders are the scenario's, "{out}" is a file holding a valid output.
func probeRun(t *testing.T, scenario, line string) string {
	t.Helper()
	w := newWiEnv(t)
	wiScenarios[scenario](w)
	if w.vars["{out}"] == "" {
		w.vars["{out}"] = w.file(`{"OK":true}`)
	}
	w.vars["{optsA}"], w.vars["{optsB}"] = w.file(`{"Fast":true}`), w.file(`{"Fast":false}`)
	w.vars["{esc}"] = "go test ./...: no network: ran"
	args := strings.Fields(line)
	for i, a := range args {
		if v, ok := w.vars[a]; ok {
			args[i] = v
		}
	}
	defer func() {
		for _, f := range w.undo {
			f()
		}
	}()
	return w.observe(w.verb(args...))
}

// wiProbe is one flag's evidence: the same scenario run with the flag and
// without it (or with another value) must end differently.
type wiProbe struct{ scenario, with, without string }

// wiProbes has one entry per documented flag of a verb that is not common to
// all verbs. A flag the table lacks, or one whose use changes nothing, fails.
var wiProbes = map[string]wiProbe{
	"attach-coordinator --initial":            {"fresh", "attach-coordinator w1 --initial --previous-run w1", "attach-coordinator w1"},
	"attach-coordinator --previous-run":       {"fresh", "attach-coordinator w1 --previous-run w1", "attach-coordinator w1"},
	"attach-coordinator --previous-state-dir": {"fresh", "attach-coordinator w1 --previous-state-dir /", "attach-coordinator w1"},

	"next --expected-admission":     {"fresh", "next w1 1 --json --expected-admission 1", "next w1 1 --json"},
	"may-end-turn --json":           {"fresh", "may-end-turn w1 --json", "may-end-turn w1"},
	"acknowledge-handoff --receipt": {"fresh", "acknowledge-handoff w1 --receipt 1", "acknowledge-handoff w1"},

	"run --halt-before":    {"none", "run agent-cli --run-id w1 --halt-before clAsk", "run agent-cli --run-id w1"},
	"run --paged-prompts":  {"none", "run agent-cli --run-id w1 --paged-prompts", "run agent-cli --run-id w1"},
	"prompt --page":        {"paged", "prompt w1 --claim {claim} --page 2 --code {code}", "prompt w1 --claim {claim} --page 1 --code {code}"},
	"prompt --code":        {"paged", "prompt w1 --claim {claim} --page 2 --code {code}", "prompt w1 --claim {claim} --page 2"},
	"read --claim":         {"paged", "read w1 {ref} --claim {claim}", "read w1 {ref} --claim invalid"},
	"read --ref":           {"paged", "read w1 {name} --claim {claim} --ref {ref}", "read w1 {name} --claim {claim} --ref invalid"},
	"read --page":          {"paged-ref", "read w1 {ref} --claim {claim} --page 2 --code {refcode}", "read w1 {ref} --claim {claim} --page 1 --code {refcode}"},
	"read --code":          {"paged-ref", "read w1 {ref} --claim {claim} --page 2 --code {refcode}", "read w1 {ref} --claim {claim} --page 2"},
	"next --until-unit":    {"fresh", "next w1 1 --until-unit clAsk#1 --json", "next w1 1 --until-unit missing#1 --json"},
	"run --snapshots":      {"none", "run agent-cli --run-id w1 --snapshots", "run agent-cli --run-id w1"},
	"run --param":          {"none", "run gatedchain --param topic=a", "run gatedchain --param topic=b"},
	"run --options":        {"none", "run opts-cli --run-id w1 --options {optsA}", "run opts-cli --run-id w1 --options {optsB}"},
	"run --workspace":      {"none", "run agent-cli --run-id w1 --workspace /ws-a", "run agent-cli --run-id w1 --workspace /ws-b"},
	"run --run-id":         {"none", "run agent-cli --run-id w1", "run agent-cli --run-id w2"},
	"run --continue":       {"none", "run agent-cli --continue w0", "run agent-cli --run-id w1"},
	"run --model-map":      {"none", "run agent-cli --run-id w1 --model-map opus=sonnet", "run agent-cli --run-id w1"},
	"run --budget":         {"none", "run agent-cli --run-id w1 --budget turns=5", "run agent-cli --run-id w1"},
	"run --delegate":       {"none", "run gate-cli --run-id w1 --delegate clGate=no", "run gate-cli --run-id w1"},
	"run --harness":        {"none", "run agent-cli --run-id w1 --harness claude", "run agent-cli --run-id w1 --harness generic"},
	"run --family":         {"none", "run family-other --run-id w1 --family claude=claude", "run family-other --run-id w1"},
	"run --family-home":    {"none", "run family-codex --run-id w1 --family-home codex=/h", "run family-codex --run-id w1"},
	"run --child-env-drop": {"none", "run family-codex --run-id w1 --child-env-drop HERDR_*", "run family-codex --run-id w1"},
	"run --headless-cmd":   {"none", `run family-codex --run-id w1 --headless-cmd codex=["x","{model}"]`, "run family-codex --run-id w1"},
	"spawn --claim":        {"fresh", "spawn w1 --claim 0123456789abcdef", "spawn w1"},
	"next --timeout":       {"claimed", "next w1 1 --timeout 100ms", "next w1 1 --timeout 300ms"},
	"next --json":          {"agent", "next w1 1 --json", "next w1 1"},
	"next --children":      {"fan-fresh", "next w1 all --children 1 --json", "next w1 all --json"},
	"status --full":        {"agent", "status w1 --full", "status w1"},
	"stop --reason":        {"fresh", "stop w1 --reason because", "stop w1"},
	"next --adopt":         {"rehashed", "next w1 1 --adopt", "next w1 1"},
	"resume --adopt":       {"rehashed", "resume w1 --adopt", "resume w1"},
	"resume --model-map":   {"stopped", "resume w1 --operator op --reason r --model-map opus=sonnet", "resume w1 --operator op --reason r"},
	"resume --budget":      {"stopped", "resume w1 --operator op --reason r --budget turns=5", "resume w1 --operator op --reason r"},
	"resume --operator":    {"stopped", "resume w1 --operator op --reason r", "resume w1 --reason r"},
	"resume --reason":      {"stopped", "resume w1 --operator op --reason r", "resume w1 --operator op"},
	"started --harness":    {"agent", "dev started w1 {name} --session s --agent a --harness codex", "dev started w1 {name} --session s --agent a"},
	"started --session":    {"agent", "dev started w1 {name} --session s --agent a", "dev started w1 {name} --agent a"},
	"started --agent":      {"agent", "dev started w1 {name} --session s --agent a", "dev started w1 {name} --session s"},
	"started --transcript": {"agent", "dev started w1 {name} --session s --agent a --transcript /t.jsonl", "dev started w1 {name} --session s --agent a"},
	"started --context":    {"agent", "dev started w1 {name} --session s --agent a --context fresh", "dev started w1 {name} --session s --agent a"},
	"started --usage":      {"agent", "dev started w1 {name} --usage tokens=5", "dev started w1 {name} --session s --agent a"},
	"started --escalated":  {"agent", "dev started w1 {name} --escalated {esc}", "dev started w1 {name} --session s --agent a"},
	"done --claim":         {"claimed", "done w1 {name} --claim invalid", "done w1 {name}"},
	"validate --claim":     {"claimed", "validate w1 {name} --claim invalid", "validate w1 {name}"},
	"failed --claim":       {"claimed", "failed w1 {name} --claim invalid --reason x", "failed w1 {name} --reason x"},
	"prompt --claim":       {"agent", "prompt w1 {name} --claim T1", "prompt w1 {name}"},
	"prompt --json":        {"agent", "prompt w1 {name} --json", "prompt w1 {name}"},
	"done --output":        {"claimed", "done w1 {name} --output {out}", "done w1 {name} --output /unrelated"},
	"done --usage":         {"claimed", "done w1 {name} --output {out} --usage tokens=5", "done w1 {name} --output {out}"},
	"validate --output":    {"claimed", "validate w1 {name} --output {out}", "validate w1 {name} --output /unrelated"},
	"done --escalated":     {"claimed", "done w1 {name} --output {out} --escalated {esc}", "done w1 {name} --output {out}"},
	"failed --reason":      {"claimed", "failed w1 {name} --reason because", "failed w1 {name}"},
	"failed --class":       {"claimed", "failed w1 {name} --class blocked", "failed w1 {name}"},
	"failed --escalated":   {"claimed", "failed w1 {name} --escalated {esc}", "failed w1 {name}"},
	"answer --gate":        {"gate", "answer w1 --gate {gate} --value no", "answer w1 --value no"},
	"answer --value":       {"gate", "answer w1 --gate {gate} --value no", "answer w1 --gate {gate} --value maybe"},
	"answer --number":      {"integergate", "answer w1 --gate {gate} --value raise --number 0", "answer w1 --gate {gate} --value raise"},
	"answer --child":       {"childgate", "answer w1 --gate {gate} --value yes --child {child}", "answer w1 --gate {gate} --value yes"},
	"answer --visit":       {"childgate", "answer w1 --gate {gate} --value yes --child {child} --visit {visit}", "answer w1 --gate {gate} --value yes --child {child} --visit 99"},
	"settle --effect":      {"effect", "settle w1 --effect {effect} --not-applied --evidence e", "settle w1 --effect 999 --not-applied --evidence e"},
	"settle --applied":     {"effect", "settle w1 --effect {effect} --applied --exit 0 --evidence e", "settle w1 --effect {effect} --not-applied --evidence e"},
	"settle --not-applied": {"effect", "settle w1 --effect {effect} --not-applied --evidence e", "settle w1 --effect {effect} --evidence e"},
	"settle --exit":        {"effect", "settle w1 --effect {effect} --applied --exit 1 --evidence e", "settle w1 --effect {effect} --applied --exit 0 --evidence e"},
	"settle --stdout-file": {"effect", "settle w1 --effect {effect} --applied --exit 0 --stdout-file {out} --evidence e", "settle w1 --effect {effect} --applied --exit 0 --evidence e"},
	"settle --operator":    {"effect", "settle w1 --effect {effect} --not-applied --evidence e --operator op", "settle w1 --effect {effect} --not-applied --evidence e"},
	"settle --reason":      {"effect", "settle w1 --effect {effect} --not-applied --evidence e --reason r", "settle w1 --effect {effect} --not-applied --evidence e"},
	"settle --evidence":    {"effect", "settle w1 --effect {effect} --not-applied --evidence out.txt-absent", "settle w1 --effect {effect} --not-applied --evidence out.txt-present"},
	"retry --child":        {"agent", "retry w1 --child nope", "retry w1"},
	"retry --stop":         {"exhausted", "retry w1 --stop", "retry w1"},
	"status --json":        {"agent", "status w1 --json", "status w1"},
	"lock --json":          {"agent", "lock w1 --json", "lock w1"},
	"heartbeat --session":  {"claimed", "heartbeat w1 died --session s1", "heartbeat w1 died"},
	"heartbeat --note":     {"claimed", "heartbeat w1 active {name} --note seen", "heartbeat w1 active {name}"},

	"retry --approval-hold": {"exhausted", "retry w1 --approval-hold 1", "retry w1"},
}

// undocumentedFlags are flags a verb parses that its Flags row (and so the
// usage text, which TestUsageUnchanged pins) does not list. They are probed like
// the documented ones; documenting them is a follow-up that rewrites the usage
// golden.
// noEffectFlags are flags a verb accepts on purpose and that change nothing,
// each with why: a probe could only show them identical.
var noEffectFlags = map[string]string{
	"next --wait": "the earlier protocol's spelling of what next always does (H1)",
}

var undocumentedFlags = map[string]string{
	"next --wait": "accepted from the earlier protocol, no effect (noEffectFlags)",

	"settle --exit":        "usage lists --applied | --not-applied only",
	"settle --stdout-file": "usage lists --applied | --not-applied only",
}

func TestEveryFlagReachesTheEngine(t *testing.T) {
	seen := map[string]bool{}
	rows := append([]wf.Subcommand(nil), wf.Subcommands...)
	for _, row := range wf.DevSubcommands {
		if row.Name == "started" {
			rows = append(rows, row)
		}
	}
	for _, row := range rows {
		if len(row.Flags) == 0 {
			continue
		}
		documented := map[string]bool{}
		for _, f := range row.Flags {
			for _, field := range strings.Fields(f) {
				if name, ok := strings.CutPrefix(field, "--"); ok {
					documented[name] = true
				}
			}
		}
		// The parser defines exactly the flags the row documents.
		localHook := row.Name == "coordinator-stop" || row.Name == "install-coordinator-stop"
		// version takes no run: its --json is exercised by TestVersionVerb.
		// setup-codex-rules: TestSetupCodexRules; dead-ends reads no run: TestDeadEndsCommand
		selfOnly := row.Name == "version" || row.Name == "setup-codex-rules" || row.Name == "dead-ends"
		args := []string{row.Name, "x"}
		if localHook || selfOnly {
			args = args[:1]
		}
		for range row.Args {
			args = append(args, "y")
		}
		var o, er bytes.Buffer
		helpArgs := append(args, "-help")
		if row.Name == "started" {
			helpArgs = append([]string{"dev"}, helpArgs...)
		}
		fixtureCLI(wiConfig(), helpArgs, &o, &er)
		defined := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?m)^\s+-(\S+)`).FindAllStringSubmatch(er.String(), -1) {
			defined[m[1]] = true
		}
		for name := range defined {
			if _, ok := undocumentedFlags[row.Name+" --"+name]; ok {
				documented[name] = true // probed below like a documented one
				continue
			}
			if !documented[name] {
				t.Errorf("%s: the parser defines --%s but the row's Flags do not document it", row.Name, name)
			}
		}
		for name := range documented {
			if _, ok := undocumentedFlags[row.Name+" --"+name]; !ok && !defined[name] {
				t.Errorf("%s: Flags documents --%s but the parser does not define it", row.Name, name)
			}
		}
		for name := range documented {
			key := row.Name + " --" + name
			seen[key] = true
			if localHook || selfOnly {
				continue // native flag handling is exercised by TestCoordinatorHookCLIFlags; version's by TestVersionVerb
			}
			if name == "workspace" && row.Name != "run" {
				// Common root remapping is exercised by TestCLIMovedRunUsesExplicitRootsAndRememberedAssets.
				continue
			}
			if name == "assets" {
				// Common root selection is exercised end-to-end, including printed
				// child commands, by TestCLIExplicitAssetsReplaceMissingDefault.
				continue
			}
			if name == "state-dir" {
				// Common to every verb: pointed at an empty directory the run is unknown.
				if row.Name == "run" {
					continue
				}
				w := newWiEnv(t)
				line := append([]string{row.Name, "w1"}, args[2:]...)
				if row.Name == "started" {
					line = append([]string{"dev"}, line...)
				}
				if code, _, er := w.call(append(line, "--state-dir", w.state)...); code != 3 {
					t.Errorf("%s with an empty --state-dir exited %d (%s), want 3: the flag does not reach the journal lookup", key, code, er)
				}
				continue
			}
			if why, ok := noEffectFlags[key]; ok {
				_ = why // accepted on purpose, changes nothing: no probe can show a difference
				continue
			}
			p, ok := wiProbes[key]
			if !ok {
				t.Errorf("%s has no probe in wiProbes: add the scenario that shows it changes the call", key)
				continue
			}
			with, without := probeRun(t, p.scenario, p.with), probeRun(t, p.scenario, p.without)
			if with == without {
				t.Errorf("%s: %q and %q ended identically:\n%s", key, p.with, p.without, with)
			}
		}
	}
	for key := range wiProbes {
		if !seen[key] {
			t.Errorf("wiProbes has %q, which no verb documents", key)
		}
	}
}

// TestEveryRecordKindIsWritten: each journal record kind is emitted by some
// engine path, not just declared and folded. The source is searched for a
// non-test line that both names Journal<Kind> and emits or builds a record.
func TestEveryRecordKindIsWritten(t *testing.T) {
	// Kinds only an old journal holds: the fold still reads them, no path writes them.
	oldJournalOnly := map[wf.JournalKind]string{
		wf.JournalLeaseClaimed:   "superseded by attempt_started and handed_out; kept so old journals fold",
		wf.JournalSessionStopped: "a coordinator error stops no session any more; kept so old journals fold",
		wf.JournalSessionCleared: "the clear of a session stop, which no longer exists; kept so old journals fold",
	}
	// Kinds the original journal design declared and folded and nothing has
	// ever written; not part of the subagent driver.
	reserved := map[wf.JournalKind]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "executor.go" || f == "engine_state.go" {
			continue // the declarations and the fold are not writers
		}
		for _, l := range strings.Split(readSource(t, f), "\n") {
			if strings.Contains(l, "emit(") || strings.Contains(l, "emitAs(") || strings.Contains(l, "Kind:") || strings.Contains(l, "JournalRecord{") {
				lines = append(lines, l)
			}
		}
	}
	src := strings.Join(lines, "\n")
	var kinds []wf.JournalKind
	for k := range wf.JournalPayloads {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	for _, k := range kinds {
		name := "Journal" + strings.ReplaceAll(strings.Title(strings.ReplaceAll(k.String(), "_", " ")), " ", "")
		written := regexp.MustCompile(`\b` + name + `\b`).MatchString(src)
		why, old := oldJournalOnly[k]
		if r, ok := reserved[k]; ok {
			why, old = r, true
		}
		switch {
		case written && old:
			t.Errorf("%s is written by the engine but listed as old-journal-only (%s): drop it from the allowlist", k, why)
		case !written && !old:
			t.Errorf("record kind %s (%s) is declared and folded but no engine path writes it", k, name)
		}
	}
}

// touched records which fields of v (a struct, recursively through pointers,
// slices and maps of structs) hold a non-zero value, as "Type.Field".
func touched(v reflect.Value, set map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			touched(v.Elem(), set)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			touched(v.Index(i), set)
		}
	case reflect.Struct:
		if v.Type().PkgPath() != reflect.TypeFor[wf.RunStatus]().PkgPath() {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); !f.IsZero() {
				set[v.Type().Name()+"."+v.Type().Field(i).Name] = true
				touched(f, set)
			}
		}
	}
}

func structFields(typ reflect.Type, out map[string]bool) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ.PkgPath() != reflect.TypeFor[wf.RunStatus]().PkgPath() {
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		// Unexported fields are not part of the JSON status schema.
		if !field.IsExported() {
			continue
		}
		out[typ.Name()+"."+field.Name] = true
		structFields(field.Type, out)
	}
}

// TestStatusSchemaRoundTrips: `status --json` of a mixed set of runs decodes
// into RunStatus with no unknown field, re-encodes to the same JSON, and between
// them the runs set every field of RunStatus and the types it holds.
func TestStatusSchemaRoundTrips(t *testing.T) {
	// Fields no CLI-driven run in this test reaches.
	unreached := map[string]string{
		"ModuleStamp.Revision":           "a test binary has no VCS stamp; TestRunStartedRecordsTheBuild sets one through the status",
		"ModuleStamp.Modified":           "a test binary has no VCS stamp; TestRunStartedRecordsTheBuild sets one through the status",
		"ModuleStamp.Source":             "a test binary has no VCS stamp; TestRunStartedRecordsTheBuild sets one through the status",
		"ModuleStamp.Replace":            "needs a replaced engine dependency; TestBuildStampFromBuildInfo covers it",
		"RunStatus.BuildDiffers":         "needs another binary reading the status; TestRunStartedRecordsTheBuild covers it",
		"LockHolder.Session":             "a coordinator verb that names no unit records its harness session; TestLockHolderNamesVerbUnitAndHostPid covers it",
		"RunStatus.PendingCleanups":      "needs shared-workspace units; TestWorktreeRemovalWaitsForOpenUnitWorkspace checks the pending status",
		"PendingCleanup.Deferred":        "TestWorktreeRemovalWaitsForOpenUnitWorkspace checks the pending status",
		"PendingCleanup.Instance":        "TestWorktreeRemovalWaitsForOpenUnitWorkspace checks the pending status",
		"PendingCleanup.Effect":          "TestWorktreeRemovalWaitsForOpenUnitWorkspace checks the pending status",
		"OpenIntent.Detached":            "needs a detached effect with a runner; TestDetachedSuiteStepEndToEnd drives one",
		"OpenIntent.Due":                 "needs a detached effect with a runner; TestDetachedStatusNamesGroupWait covers it",
		"OpenIntent.WaitGroup":           "needs a detached re-run waiting on a dead launch's group; TestDetachedStatusNamesGroupWait covers it",
		"OpenIntent.Reason":              "needs a detached re-run after a dead launch; TestDetachedStatusNamesGroupWait covers it",
		"OpenIntent.RerunOf":             "needs a detached re-run after a dead launch; TestDetachedStatusNamesGroupWait covers it",
		"OpenIntent.Instance":            "an open intent of a child instance; the effect scenario's is the run's own",
		"RecoveryView.GiveUp":            "needs a failure with no route offered for recovery; TestBlockedFailureWithNoRouteIsOfferedToTheCoordinator covers it",
		"UnitStatus.Quiet":               "needs the engine clock advanced past a stall; engine_liveness_test covers it",
		"InstanceStatus.Reason":          "needs a halted instance; TestDefaultUnavailableBackoffBounded covers it",
		"RecoveryView.Transcript":        "needs a child transcript that ended without done (F1); TestEndedChildIsOfferedForRecovery covers it",
		"RecoveryView.LastTool":          "needs a child transcript that ended without done (F1); TestEndedChildIsOfferedForRecovery covers it",
		"RecoveryView.LastMessage":       "needs a child transcript that ended without done (F1); TestEndedChildIsOfferedForRecovery covers it",
		"RecoveryView.Result":            "needs a child transcript that ended without done (F1); TestEndedChildIsOfferedForRecovery covers it",
		"RecoveryView.Resume":            "needs a child transcript that ended without done (F1); TestEndedChildIsOfferedForRecovery covers it",
		"RunStatus.Held":                 "approval holds are covered by TestApprovalHoldPreservesRetriesAndClaimOwnership",
		"HeldUnit.Child":                 "requires a held nested instance; the approval hold coverage uses a root unit",
		"HeldUnit.Name":                  "requires an approval hold or the legacy unit_held record",
		"HeldUnit.Cause":                 "requires an approval hold or the legacy unit_held record",
		"HeldUnit.Parked":                "legacy autonomous holds only",
		"HeldUnit.ApprovalAction":        "exact action checked by TestApprovalHoldPreservesRetriesAndClaimOwnership",
		"HeldUnit.ApprovalHold":          "release scoping checked by TestApprovalHoldPreservesRetriesAndClaimOwnership",
		"RunStatus.Future":               "needs agent states ahead of the current one; TestStatusFutureCount covers it",
		"RunStatus.Predecessor":          "needs a run on an ended run (a successor); TestRunOnAnEndedRunStartsASuccessor covers it",
		"RunStatus.Detach":               "needs an effect runner that probes a detach mode; shell's TestBootstrapNamesDetachNone and TestDetachNoneRunsInline cover it",
		"Progress.WaitingOnDeps":         "needs units with dependencies; TestWaitingOnDepsCounted covers it",
		"Progress.TotalIsLowerBound":     "needs a source that can still grow; TestProgressLowerBound covers it",
		"SegmentView.End":                "needs a run that changed harness segments (stop and resume, rewind); TestHarnessSegmentsMigrateThroughStopAndResume covers it",
		"UnitStatus.Harness":             "needs a unit on another harness; TestHeadlessStatusShowsChildHarness covers it",
		"UnitStatus.Pid":                 "needs a started headless child; TestHeadlessStatusShowsChildHarness covers it",
		"UnitStatus.StartErr":            "needs a headless child that exited before fetching; TestHeadlessStatusShowsChildHarness covers it",
		"RunStatus.Children":             "needs a family hint and a bound segment; TestHeadlessStatusShowsChildHarness covers it",
		"HarnessCheckedData.Harness":     "needs a family hint and a bound segment; TestHeadlessStatusShowsChildHarness covers it",
		"HarnessCheckedData.Coordinator": "as Harness",
		"HarnessCheckedData.Available":   "as Harness",
		"HarnessCheckedData.Version":     "as Harness",
		"HarnessCheckedData.Why":         "needs an unavailable child harness; TestHeadlessUnavailableNeverFallsBack covers it",
		"HarnessCheckedData.Fix":         "as Why",
		"SegmentModels.Children":         "needs a definition with a family hint; TestHeadlessPrimitiveClaudeToCodex and TestChildTablesSurviveResume cover it",
		"SegmentView.Harness":            "needs a segment bound by a coordinator with a session; TestHarnessSegmentsMigrateThroughStopAndResume covers it",
		"InstanceStatus.Returned":        "needs a send-back definition; TestSendBackReturnsTheWorkToItsProducerWithTheReasons checks the status",
		"Return.From":                    "as InstanceStatus.Returned",
		"Return.Label":                   "as InstanceStatus.Returned",
		"Return.Round":                   "as InstanceStatus.Returned",
		"Return.Of":                      "as InstanceStatus.Returned",
		"Return.Reasons":                 "as InstanceStatus.Returned",
	}
	var statuses []string
	snap := func(w *wiEnv) { statuses = append(statuses, w.must("status", "w1", "--json")) }

	paged := newWiEnv(t)
	wiPagedScenario(paged)
	snap(paged)

	// A paged run: three units, two handed out, one finished, one claimed, one handed.
	w := newWiEnv(t)
	w.start("fan-cli")
	ins := w.instructions("next", "w1", "2", "--json")
	w.must("dev", "started", "w1", ins[0].Name, "--session", "s1", "--agent", "a1")
	w.must("done", "w1", ins[0].Name, "--output", writeAllocated(t, ins[0].Output, `{"OK":true}`), "--usage", "tokens=9",
		"--escalated", "go test ./...: no network: ran")
	snap(w) // ins[1] is handed and unstarted
	w.must("dev", "started", "w1", ins[1].Name, "--session", "s1", "--agent", "a2")
	snap(w) // ins[1] runs, and its class now has history

	// A coordinator lost: its unit's attempt is noted lost.
	w = newWiEnv(t)
	w.start("fan-cli")
	ins = w.instructions("next", "w1", "2", "--json")
	w.must("dev", "started", "w1", ins[0].Name, "--session", "s1", "--agent", "a1")
	w.must("heartbeat", "w1", "died", "--session", "s1")
	snap(w)

	// A fan whose children still have a gate ahead.
	w = newWiEnv(t)
	w.start("ho-gate-fan")
	w.instructions("next", "w1", "1", "--json")
	snap(w)

	// A run that ended.
	w = newWiEnv(t)
	wiScenarios["ended"](w)
	snap(w)

	// A blocked failure holds a unit: an interactive run asks the human, an
	// autonomous run parks the cause and goes on with the other unit.
	w = newWiEnv(t)
	w.start("fan-cli")
	ins = w.instructions("next", "w1", "1", "--json")
	w.must("dev", "started", "w1", ins[0].Name, "--session", "s1", "--agent", "a1")
	w.must("failed", "w1", ins[0].Name, "--class", "blocked", "--reason", "BLOCKED_AUTH")
	snap(w)
	w = newWiEnv(t)
	w.must("run", "fan-opts", "--workspace", w.ws, "--run-id", "w1", "--options", w.file(`{"Autonomous":true}`))
	ins = w.instructions("next", "w1", "1", "--json")
	w.must("dev", "started", "w1", ins[0].Name, "--session", "s1", "--agent", "a1")
	w.must("failed", "w1", ins[0].Name, "--class", "blocked", "--reason", "BLOCKED_AUTH")
	snap(w)

	// A stopped run: its handed unit may still finish; nothing new starts.
	w = newWiEnv(t)
	w.start("agent-cli")
	w.instructions("next", "w1", "1", "--json")
	w.must("stop", "w1", "--reason", "operator")
	snap(w)

	// A breakpoint in a mapped child carries a qualified state and instance.
	w = newWiEnv(t)
	w.must("run", "fan-cli", "--run-id", "w1", "--halt-before", "chWork")
	if code, _, stderr := w.verb("next", "w1", "1", "--json"); code != 7 {
		t.Fatalf("next at child breakpoint exited %d: %s", code, stderr)
	}
	snap(w)

	// An exhaustion halt: the run is blocked with its fix: answers.
	w = newWiEnv(t)
	wiScenarios["exhausted"](w)
	snap(w)

	// A spent budget offered to the coordinator (B2).
	w = newWiEnv(t)
	w.start("agent-cli")
	for i := 0; i < 2; i++ {
		n := w.instructions("next", "w1", "1", "--json")[0].Name
		w.must("dev", "started", "w1", n, "--session", "s1", "--agent", "a1")
		w.must("failed", "w1", n, "--reason", "no")
	}
	snap(w)

	// An uncertain effect left open by a crash.
	w = newWiEnv(t)
	wiScenarios["effect"](w)
	snap(w)

	// Options and a delegated gate.
	w = newWiEnv(t)
	w.must("run", "opts-cli", "--workspace", w.ws, "--run-id", "w1", "--options", w.file(`{"Fast":true}`))
	snap(w)
	w = newWiEnv(t)
	w.must("run", "gate-cli", "--workspace", w.ws, "--run-id", "w1", "--delegate", "clGate=no")
	snap(w)

	// A journal with no harness exercises the legacy status marker and JSON shape.
	legacyDef := clAgentDef("legacy-status")
	legacyJournal := wf.MemJournal()
	unlock := lock(t, legacyJournal, "legacy")
	data, err := json.Marshal(wf.RunStartedData{Def: legacyDef.Name(), Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyJournal.Append(wf.JournalRecord{Run: "legacy", Kind: wf.JournalRunStarted, DefHash: legacyDef.Hash(), Data: data}); err != nil {
		t.Fatal(err)
	}
	unlock()
	legacyEngine := wf.NewEngine(legacyJournal, nil, wf.EngineOptions{Defs: []*wf.Def{legacyDef}})
	legacyStatus, err := legacyEngine.Status("legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacyJSON, err := json.Marshal(legacyStatus)
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(legacyJSON))

	// A manufactured run (wf dev manufacture) shows its mark.
	madeDef := clAgentDef("made-status").Describe("make the status fixture's work")
	madeEngine := wf.NewEngine(wf.MemJournal(), nil, wf.WithManufactured(wf.EngineOptions{Defs: []*wf.Def{madeDef}, RunID: "made"},
		&wf.ManufacturedData{Edge: "made-status/Work/edge[0]", Until: "ended", Hash: madeDef.Hash(), Script: "sha256:status"}))
	madeRun, err := madeEngine.Run(madeDef, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	madeStatus, err := madeEngine.Status(madeRun)
	if err != nil {
		t.Fatal(err)
	}
	madeJSON, err := json.Marshal(madeStatus)
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(madeJSON))

	// A run with a --model-map whose segment a coordinator bound.
	tierFan := tieredFan("models-status", 1, wf.TierStrong, 1)
	mapped := newHarnessOpts(t, tierFan, nil, wf.EngineOptions{Harness: "codex", ModelMap: map[string]string{"all": "gpt-6.1-terra"}})
	mappedRun := mapped.run(tierFan, nil)
	// Its coordinator then moves to another records root: the note's Root.
	for _, root := range []string{"/codex/a", "/codex/b"} {
		engine := mapped.e.(*wf.Engine)
		co := wf.Coordinator{Harness: "codex", Session: "c-models", Root: root}
		ins, err := engine.HandOutAs(mappedRun, 1, co)
		if err != nil {
			t.Fatal(err)
		}
		for _, unit := range agentsOf(ins) {
			who := hn.Caller{Harness: co.Harness, Session: co.Session, Root: co.Root, Is: hn.IsSelf, FromEnv: true}
			if _, err := engine.PromptFetchAs(mappedRun, unit.Name, claimToken(t, unit), who); err == nil {
				t.Fatal("the coordinator's prompt fetch succeeded")
			}
		}
	}
	mappedStatus, err := mapped.e.Status(mappedRun)
	if err != nil {
		t.Fatal(err)
	}
	mappedJSON, err := json.Marshal(mappedStatus)
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(mappedJSON))

	// A run its budget stopped: the budget in force and why it stopped.
	bd := rsChainDef()
	bh := newHarnessOpts(t, bd, nil, wf.EngineOptions{RunBudget: wf.RunBudget{Turns: 1, Tokens: 1000, Wall: time.Hour}})
	brun := bh.run(bd, nil)
	bh.submit(brun, bh.next(brun)[0], `{"V":"ok"}`)
	bh.next(brun)
	budgetJSON, err := json.Marshal(bh.status(brun))
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(budgetJSON))

	// An ended keyed run advertises a usable successor command.
	d := clAgentDef("keyed-status")
	d.RunKey(d.Param("topic"))
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: d}}}
	state, workspace := t.TempDir(), t.TempDir()
	call := func(args ...string) string {
		var out, er bytes.Buffer
		if code := fixtureCLI(cfg, append(args, "--state-dir", state), &out, &er); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er.String())
		}
		return out.String()
	}
	run := strings.TrimSpace(call("run", d.Name(), "--workspace", workspace, "--param", "topic=example"))
	var keyedUnits []wf.Instruction
	if err := json.Unmarshal([]byte(call("next", run, "all", "--json")), &keyedUnits); err != nil || len(keyedUnits) != 1 {
		t.Fatalf("keyed units: %+v (%v)", keyedUnits, err)
	}
	u := keyedUnits[0]
	call("prompt", run, u.Name)
	writeAllocated(t, u.Output, `{"OK":true}`)
	call("done", run, u.Name)
	statuses = append(statuses, call("status", run, "--json"))

	// A unit whose coordinator answered a stall with the work it saw.
	obsDef := laDef("observed-status", 5*time.Minute)
	obs := newHarnessOpts(t, obsDef, nil, lsOpts())
	obsRun, obsIns := fetchedUnit(t, obs, obsDef)
	obs.now = obs.now.Add(5 * time.Minute)
	obs.handOut(obsRun, 0)
	if err := obs.e.Heartbeat(obsRun, wf.HeartbeatTarget{Name: obsIns.Name, Note: "tests still running"}, true); err != nil {
		t.Fatal(err)
	}
	obsStatus, err := obs.e.Status(obsRun)
	if err != nil {
		t.Fatal(err)
	}
	obsJSON, err := json.Marshal(obsStatus)
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(obsJSON))

	te, tr, _, _ := coordinatorTurnStall(t)
	turnStatus, err := te.Status(tr)
	if err != nil {
		t.Fatal(err)
	}
	rawTurnStatus, err := json.Marshal(turnStatus)
	if err != nil {
		t.Fatal(err)
	}
	statuses = append(statuses, string(rawTurnStatus))
	set := map[string]bool{}
	for i, raw := range statuses {
		var st wf.RunStatus
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&st); err != nil {
			t.Fatalf("status %d does not decode into RunStatus: %v\n%s", i, err, raw)
		}
		back, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(back, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("status %d changed in the round trip:\nwas  %s\nnow  %s", i, raw, back)
		}
		touched(reflect.ValueOf(st), set)
	}
	all := map[string]bool{}
	structFields(reflect.TypeFor[wf.RunStatus](), all)
	for f := range all {
		if _, skip := unreached[f]; !set[f] && !skip {
			t.Errorf("no run set %s: the status schema field is never filled (or is dead)", f)
		}
	}
}

// payloadFieldExempt are payload fields no live path sets, each with the
// reason. An exemption must say what covers the field instead.
var payloadFieldExempt = map[string]string{
	"EffectStartedData.Start": "legacy: only an old journal carries it (spec §4.14)",
}

// TestEveryJournalPayloadFieldIsWritten: each field of the amend-code-1
// payload types (the proc record, the launch request and report, the effect
// records) is set by some non-test line of wf or shell, not just declared.
func TestEveryJournalPayloadFieldIsWritten(t *testing.T) {
	var lines []string
	for _, glob := range []string{"*.go", "../shell/*.go"} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			lines = append(lines, strings.Split(readSource(t, f), "\n")...)
		}
	}
	types := []reflect.Type{
		reflect.TypeFor[wf.EffectStartedData](),
		reflect.TypeFor[wf.EffectRerun](),
		reflect.TypeFor[wf.EffectIntentData](),
		reflect.TypeFor[wf.DetachedRequest](),
		reflect.TypeFor[wf.DetachedProcess](),
		reflect.TypeFor[wf.DetachedReport](),
	}
	checked := map[string]bool{}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			name := typ.Name() + "." + typ.Field(i).Name
			checked[name] = true
			field := typ.Field(i).Name
			set := regexp.MustCompile(`\b` + field + `:|\.` + field + `\s*=[^=]`)
			decl := regexp.MustCompile(`^\s*(\w+,\s*)*` + field + `(,\s*\w+)*\s+[\w.*\[\]]+`)
			written := false
			for _, l := range lines {
				if set.MatchString(l) && !decl.MatchString(l) {
					written = true
					break
				}
			}
			// An exempt field may still be copied through (a legacy record's
			// Start is carried into the process value), so exemption is not
			// checked against the search.
			if _, exempt := payloadFieldExempt[name]; !written && !exempt {
				t.Errorf("%s is declared but no non-test line sets it", name)
			}
		}
	}
	for name := range payloadFieldExempt {
		if !checked[name] {
			t.Errorf("exemption names %s, which is not a checked field", name)
		}
	}
}

// emittedEventKinds returns every EventKind constant of event.go mapped to
// whether some other non-test line of wf assigns it to an event's Kind.
func emittedEventKinds(t *testing.T) map[string]bool {
	t.Helper()
	consts := regexp.MustCompile(`(Event\w+)\s+EventKind\s*=`).FindAllStringSubmatch(readSource(t, "event.go"), -1)
	if len(consts) < 10 {
		t.Fatalf("found %d EventKind constants in event.go, want at least 10", len(consts))
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, l := range strings.Split(readSource(t, f), "\n") {
			if !regexp.MustCompile(`\bEventKind\s*=`).MatchString(l) {
				lines = append(lines, l)
			}
		}
	}
	out := map[string]bool{}
	for _, c := range consts {
		k := c[1]
		assign := regexp.MustCompile(`Kind\s*(,\s*\w+(\.\w+)?)*\s*=\s*.*\b` + k + `\b|Kind:\s*` + k + `\b`)
		out[k] = false
		for _, l := range lines {
			if assign.MatchString(l) {
				out[k] = true
				break
			}
		}
	}
	return out
}

// TestEveryEventKindIsEmitted: each event kind Wait can report is assigned to
// an event somewhere in the engine, so a declared kind nobody emits fails by name.
func TestEveryEventKindIsEmitted(t *testing.T) {
	for k, emitted := range emittedEventKinds(t) {
		if !emitted {
			t.Errorf("%s is declared in event.go but no non-test line assigns it to an event's Kind", k)
		}
	}
}

// wiPagedScenario exercises real CLI transport and exposes its continuation codes.
func wiPagedScenario(w *wiEnv) {
	d, _ := simple("paged-wiring", wf.PromptText(strings.Repeat("task instruction ", 900)+"\n{{spec}}", wf.Reference(wf.Bound("spec", strings.Repeat("reference body ", 900)), true)))
	d.OnFailure(wf.Blocked, wf.NewTerminal[eOther](d, wf.Stopped))
	w.cfg.Defs = append(w.cfg.Defs, wf.Registration{Def: d})
	w.must("run", d.Name(), "--workspace", w.ws, "--run-id", "w1", "--paged-prompts")
	in := w.instructions("next", "w1", "1", "--json")[0]
	w.vars["{name}"], w.vars["{claim}"] = in.Name, claimToken(w.t, in)
	w.must("prompt", "w1", "--claim", w.vars["{claim}"])
	j := wf.FileJournal(w.state)
	unlock := lock(w.t, j, "w1")
	records, err := j.Records("w1")
	unlock()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, r := range records {
		if r.Kind != wf.JournalPromptFetched {
			continue
		}
		var d wf.PromptFetchedData
		if err = json.Unmarshal(r.Data, &d); err != nil {
			w.t.Fatal(err)
		}
		if d.Packet == nil {
			continue
		}
		w.vars["{code}"] = d.Packet.Codes[0]
		for id, ref := range d.Packet.References {
			w.vars["{ref}"], w.vars["{refcode}"] = id, ref.Codes[0]
		}
	}
	if w.vars["{ref}"] == "" {
		w.t.Fatal("paged scenario has no reference")
	}
	time.Sleep(510 * time.Millisecond)
}
