package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// keyedAgentDef is one agent keyed on the topic param, then a terminal.
func keyedAgentDef(name string) *wf.Def {
	return keyedAgentDefSaying(name, "Say hello about {{topic}}.")
}

func keyedAgentDefSaying(name, prompt string, alias ...string) *wf.Def {
	d := wf.New(name)
	topic := d.Param("topic").Alias(alias...)
	d.RunKey(topic)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText(prompt, topic), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	return d
}

func finishOne(t *testing.T, def *wf.Def, state string) {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	mustSingle(t, def, "wf", "dev", "started", ins[0].Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, ins[0].Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", ins[0].Name, "--state-dir", state)
}

// Section C1: `run` on a run that has ended never reopens it. It starts a
// successor: a new run id, the predecessor's records carried over, the
// predecessor journaled, and a line saying so. A live successor is attached,
// not succeeded again.
func TestRunOnAnEndedRunStartsASuccessor(t *testing.T) {
	def := keyedAgentDef("succ")
	state := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	first := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x"))
	recs := filepath.Join(state, "runs", first, "records")
	if err := os.MkdirAll(filepath.Join(recs, "sdd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recs, "run.md"), []byte("phase: code\nepic: E1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recs, "sdd", "progress.md"), []byte("ledger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finishOne(t, def, state)

	code, out, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x")
	if code != 0 {
		t.Fatalf("run on an ended run exited %d: %s", code, er)
	}
	second := strings.TrimSpace(out)
	if second == first || second != first+".2" {
		t.Fatalf("successor id %q, want %s.2", second, first)
	}
	if !strings.Contains(er, "previous run "+first+" ended") || !strings.Contains(er, "continuing as "+second) {
		t.Errorf("no successor line on stderr: %q", er)
	}
	for _, f := range []string{"run.md", "sdd/progress.md"} {
		raw, err := os.ReadFile(filepath.Join(state, "runs", second, "records", f))
		if err != nil || len(raw) == 0 {
			t.Errorf("successor records lack %s: %v", f, err)
		}
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if string(st.Run) != second || string(st.Predecessor) != first || st.Phase == wf.RunEnded {
		t.Errorf("status = run %s predecessor %s phase %s; want the live successor of %s", st.Run, st.Predecessor, st.Phase, first)
	}
	// The predecessor stays ended and untouched.
	if code, _, _ := singleCLI(def, "wf", "next", "1", "--run-id", first, "--state-dir", state, "--json"); code != 7 {
		t.Errorf("the predecessor answered next with %d, want 7 (ended)", code)
	}
	// A live successor is attached by the same command.
	if again := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x")); again != second {
		t.Errorf("run with a live successor = %s, want it attached (%s)", again, second)
	}
}

// Section C2: next and status on an ended run carry the command that resumes.
func TestEndedRunPrintsTheResumeCommand(t *testing.T) {
	def := keyedAgentDef("succ-resume")
	state := filepath.Join(t.TempDir(), "state")
	id := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--param", "topic=x y"))
	finishOne(t, def, state)
	// The binary by absolute path, the state dir, the run it continues (its
	// chain), the workspace, and only the params the caller gave.
	want := " run succ-resume --state-dir " + state + " --continue " + id + " --workspace "
	out := mustSingle(t, def, "wf", "status", "--state-dir", state)
	if !strings.Contains(out, "resume, to continue this run later:\n  cd /") || !strings.Contains(out, want) || !strings.Contains(out, "--param 'topic=x y'") {
		t.Errorf("status of an ended run lacks the resume line %q:\n%s", want, out)
	}
	code, out, _ := singleCLI(def, "next", "--state-dir", state)
	if code != 7 {
		t.Fatalf("next on an ended run exited %d, want 7", code)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil || len(ins) != 1 || !strings.Contains(ins[0].Resume, want) || !strings.Contains(ins[0].Resume, "topic=x y") {
		t.Errorf("next on an ended run = %s (%v), want the ended instruction with resume %q", out, err, want)
	}
}

// A rebuilt binary changed the definition: the ended run is not migrated
// (nothing is written to a terminal journal), and its successor starts.
func TestSuccessorAfterADefinitionChange(t *testing.T) {
	def := keyedAgentDef("succ-def")
	state := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	first := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x"))
	finishOne(t, def, state)
	before, err := os.ReadFile(filepath.Join(state, "runs", first, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	changed := keyedAgentDefSaying("succ-def", "Say goodbye about {{topic}}.") // another hash
	if changed.Hash() == def.Hash() {
		t.Fatal("the changed definition hashes the same")
	}
	code, out, er := singleCLI(changed, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x")
	if code != 0 || strings.TrimSpace(out) != first+".2" {
		t.Fatalf("run after a definition change = %d %q %s, want the successor %s.2", code, out, er, first)
	}
	after, _ := os.ReadFile(filepath.Join(state, "runs", first, "journal.jsonl"))
	if string(after) != string(before) {
		t.Errorf("the ended run's journal was written to")
	}
}

type succOpts struct{ Mode string }

// optionsDef is keyedAgentDef with an options type and a registration
// default, as every fork entry has.
func optionsCfg(name string) (wf.MainConfig, *wf.Def) {
	d := keyedAgentDef(name)
	wf.Options[succOpts](d)
	return wf.MainConfig{Adapters: clConfig().Adapters, Binary: "super",
		Defs: []wf.Registration{{Def: d, DefaultOptions: succOpts{Mode: "default"}}}}, d
}

// A successor carries on its predecessor's options, not the registration's
// default; the resume line repeats the given param; and a successor from
// another workspace is refused.
func TestSuccessorInheritsOptionsAndWorkspace(t *testing.T) {
	cfg, def := optionsCfg("succ-opts")
	state := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	optFile := filepath.Join(t.TempDir(), "o.json")
	if err := os.WriteFile(optFile, []byte(`{"Mode":"interactive"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) { return cliCall(cfg, args...) }
	if code, _, er := run("run", "--state-dir", state, "--workspace", ws, "--param", "topic=x", "--options", optFile); code != 0 {
		t.Fatalf("first run: %d %s", code, er)
	}
	finishOne(t, def, state)
	var st wf.RunStatus
	_, out, _ := run("wf", "status", "--json", "--state-dir", state)
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Resume, "--param topic=x") {
		t.Errorf("resume line %q lacks the given param", st.Resume)
	}
	if code, _, er := run("run", "--state-dir", state, "--workspace", t.TempDir(), "--param", "topic=x"); code != 2 || !strings.Contains(er, "its successor runs there") {
		t.Errorf("a successor from another workspace = %d %q, want refused", code, er)
	}
	code, out, er := run("run", "--state-dir", state, "--workspace", ws, "--param", "topic=x")
	if code != 0 {
		t.Fatalf("successor: %d %s", code, er)
	}
	_, out, _ = run("wf", "status", "--json", "--state-dir", state)
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Predecessor == "" || !strings.Contains(string(st.Options), "interactive") {
		t.Errorf("successor options %s (predecessor %q), want the predecessor's interactive mode", st.Options, st.Predecessor)
	}
	// Attaching to the live successor keeps its options without --options.
	if code, _, er := run("run", "--state-dir", state, "--workspace", ws, "--param", "topic=x"); code != 0 {
		t.Errorf("attach to the live successor: %d %s", code, er)
	}
}
