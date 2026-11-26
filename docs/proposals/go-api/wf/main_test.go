package wf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/examples/gatedchain"
	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness/agytest"
	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// State marker types for the CLI tests.
type (
	clAsk    struct{}
	clDone   struct{}
	clGaveUp struct{}
	clMake   struct{}
	clOK     struct{}
	clFail   struct{}
	clExit   int
)

type clAnswer struct{ OK bool }

// clAgentDef is one agent with dispatch hints, then a terminal.
func clAgentDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	a.Dispatch(wf.ModelTier(wf.TierStrong), wf.Effort("high"), wf.AgentType("reviewer"))
	return d
}

// clCommandDef is one shell command routed on its exit code.
func clCommandDef() *wf.Def {
	d := wf.New("cmd-cli")
	exits := wf.NewEnum(d, "cl-exit", clExit(0), clExit(1))
	cmd := wf.NewCommand[clMake](d, "make")
	ok := wf.NewTerminal[clOK](d, wf.Success)
	fail := wf.NewTerminal[clFail](d, wf.Stopped)
	cmd.RouteCases(wf.Cases[clMake, wf.RunResult](wf.ExitAs[clExit], exits,
		wf.On(clExit(0), cmd.To(ok, "passes")),
		wf.On(clExit(1), cmd.To(fail, "fails"))))
	return d
}

type (
	clGate  struct{}
	clYes   struct{}
	clNoEnd struct{}
)

// clGateDef is one human gate whose answer needs shell quoting.
func clGateDef() *wf.Def {
	d := wf.New("gate-cli")
	ans := wf.NewEnum(d, "cl-ans", "a b'c", "no")
	g := wf.NewGate[clGate](d, "Proceed?", ans, wf.Reserved)
	yes := wf.NewTerminal[clYes](d, wf.Success)
	no := wf.NewTerminal[clNoEnd](d, wf.Stopped)
	g.Answers(wf.On("a b'c", g.To(yes, "yes")), wf.On("no", g.To(no, "no")))
	return d
}

// clWorld is the registry every CLI test process serves: every read and every
// effect succeeds, except that WF_TEST_CRASH=1 kills the process in the middle
// of an effect, leaving an intent with no result.
type clWorld struct{}

func (clWorld) Ops() []string { return []string{"*"} }
func (clWorld) Read(_ string, d wf.ObservationDesc) (json.RawMessage, error) {
	if d.Adapter == "fsx" {
		return json.RawMessage(`{"OK":true}`), nil
	}
	return json.RawMessage(`{"Exit":0}`), nil
}
func (clWorld) Apply(string, wf.ExternalEffect) (json.RawMessage, error) {
	if os.Getenv("WF_TEST_CRASH") == "1" {
		os.Exit(3)
	}
	return json.RawMessage(`{"Exit":0}`), nil
}
func (clWorld) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectUnknown, nil
}

func clConfig() wf.MainConfig {
	reg := wf.NewAdapters()
	if err := reg.Register(clWorld{}); err != nil {
		panic(err)
	}
	return wf.MainConfig{
		Adapters: reg,
		Defs: []wf.Registration{
			{Def: gatedchain.Definition()},
			{Def: fanDef("fan-cli", chItems(1, 2), 2, wf.AllSettled())},
			{Def: clAgentDef("agent-cli")},
			{Def: clAgentDef("needs-cli"), Required: []string{"skillsRoot"}},
			{Def: clCommandDef()},
			{Def: itDef()},
			{Def: clGateDef()},
		},
	}
}

// TestMain re-enters as the CLI when WF_TEST_CLI=1, so an instruction's Submit
// line (which names this binary) runs as a real subprocess.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == shell.DetachedVerb { // the detached runner re-executes this binary
		os.Exit(shell.RunDetached(os.Args[2:], os.Stdout, os.Stderr))
	}
	if os.Getenv("WF_TEST_CLI") == "1" {
		wf.TrustSessionlessCallers = true // as the suite: it drives from CLAUDECODE alone
		cfg := clConfig()
		if os.Getenv("WF_TEST_BOOTSTRAP") == "1" {
			for _, reg := range cfg.Defs {
				if reg.Def.Name() == "gate-cli" {
					reg.Def.RunKey(reg.Def.Param("key"))
				}
			}
		}
		if os.Getenv("WF_TEST_NAMED") == "1" {
			cfg.Binary = "test-client"
		}
		os.Exit(fixtureCLI(cfg, os.Args[1:], os.Stdout, os.Stderr))
	}
	// Harness detection (resolveCapacity) reads the ambient harness env, so the
	// suite pins it: every run without --harness is a claude run, whatever
	// harness hosts the tests (an agy shell's ANTIGRAVITY_* variables too: its
	// records root is one of them). Tests of detection set these vars with t.Setenv.
	for _, k := range wf.HarnessEnvVars() {
		os.Unsetenv(k)
	}
	os.Setenv("CLAUDECODE", "1")
	// No test blocks for a harness's whole tool limit by accident: a next
	// that picks its own wait waits at most this (an explicit --timeout is
	// kept).
	os.Setenv(wf.NextWaitCapEnv, "1s")
	// The suite drives the CLI from CLAUDECODE alone, with no session; the
	// tests of J1/J2 turn this off (strictCallers).
	wf.TrustSessionlessCallers = true
	// Nor does it read this machine's harness configuration or records (a
	// Codex max_threads, a Claude Code output setting, transcripts): each
	// home points at an empty directory; tests of them set their own.
	for _, k := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HERMES_HOME"} {
		dir, err := os.MkdirTemp("", "wf-test-"+strings.ToLower(k))
		if err != nil {
			panic(err)
		}
		os.Setenv(k, dir)
	}
	code := m.Run()
	for _, k := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HERMES_HOME"} {
		os.RemoveAll(os.Getenv(k))
	}
	os.Exit(code)
}

// TestAmbientAgyEnvIsCleared runs the probe below in a re-executed copy of
// this binary whose environment is an agy coordinator's: its conversation
// marker and a records root that makes that conversation a valid coordinator.
// TestMain must have removed both before any test runs, so the probe sees a
// claude run and never reads the sentinel records.
func TestAmbientAgyEnvIsCleared(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	tr := agytest.New(t)
	tr.Coordinator(id, time.Now().Add(-time.Minute))
	cmd := exec.Command(os.Args[0], "-test.run=^TestAmbientAgyEnvProbe$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_CONVERSATION_ID="+id, "ANTIGRAVITY_APP_DATA_DIR="+tr.Root, "WF_TEST_ENV_PROBE=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestAmbientAgyEnvProbe") {
		t.Fatalf("probe under an agy shell failed: %v\n%s", err, out)
	}
}

// TestAmbientAgyEnvProbe is TestAmbientAgyEnvIsCleared's subprocess half: it
// skips unless re-executed with an agy shell's variables set.
func TestAmbientAgyEnvProbe(t *testing.T) {
	if os.Getenv("WF_TEST_ENV_PROBE") != "1" {
		t.Skip("run by TestAmbientAgyEnvIsCleared")
	}
	for _, k := range []string{"ANTIGRAVITY_APP_DATA_DIR", "ANTIGRAVITY_CONVERSATION_ID"} {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s = %q survived TestMain", k, v)
		}
	}
	if got := hn.Detect(os.Getenv); got != "claude" {
		t.Fatalf("Detect = %q, want claude", got)
	}
	if lim := hn.For(hn.Detect(os.Getenv)).ToolLimit(os.Getenv); lim == 0 {
		t.Errorf("ToolLimit = 0: the suite inherited agy's limit")
	}
	def := clAgentDef("ambient-probe")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	started := agyJournal(t, state, wf.JournalRunStarted)
	var rs wf.RunStartedData
	if len(started) != 1 || json.Unmarshal(started[0].Data, &rs) != nil || rs.Harness != "claude" {
		t.Fatalf("run_started = %d records, harness %q, want one claude run", len(started), rs.Harness)
	}
}

type cliEnv struct {
	t     *testing.T
	state string
	ws    string
	runs  int // runs e.start numbered, for definitions with no run key
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	e := &cliEnv{t: t, state: filepath.Join(t.TempDir(), "state"), ws: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(e.ws, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ws, "prompts", "draft-outline.md"), []byte("Draft an outline."), 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

// cli runs wf.CLI in this process.
func (e *cliEnv) cli(args ...string) (int, string, string) {
	var o, er bytes.Buffer
	code := fixtureCLI(clConfig(), args, &o, &er)
	return code, o.String(), er.String()
}

// proc runs the same CLI as a subprocess of this test binary.
func (e *cliEnv) proc(dir string, env []string, args ...string) (int, string, string) {
	return runProc(e.t, os.Args[0], dir, env, args...)
}

func runProc(t *testing.T, bin, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"WF_TEST_CLI=1"}, env...)...)
	var o, er bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &er
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, o.String(), er.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), o.String(), er.String()
	}
	t.Fatal(err)
	return 0, "", ""
}

func (e *cliEnv) must(args ...string) string {
	e.t.Helper()
	code, out, er := e.cli(args...)
	if code != 0 {
		e.t.Fatalf("%v exited %d: %s", args, code, er)
	}
	return out
}

func (e *cliEnv) start(def string, extra ...string) wf.RunID {
	e.t.Helper()
	args := append([]string{"run", def, "--state-dir", e.state, "--workspace", e.ws}, extra...)
	if !slices.Contains(extra, "--run-id") && !slices.Contains(extra, "--param") { // a keyless definition needs an id
		e.runs++
		args = append(args, "--run-id", fmt.Sprintf("%s-t%d", def, e.runs))
	}
	return wf.RunID(strings.TrimSpace(e.must(args...)))
}

// exitEndedCode is the exit code of `next` on an ended run.
const exitEndedCode = 7

func (e *cliEnv) next(run wf.RunID, extra ...string) []wf.Instruction {
	e.t.Helper()
	return e.nextBy(e.cli, run, extra...)
}

// nextBy runs next <run> all (or the count extra starts with) and returns its
// instructions without the ended one.
func (e *cliEnv) nextBy(call func(...string) (int, string, string), run wf.RunID, extra ...string) []wf.Instruction {
	e.t.Helper()
	if len(extra) == 0 || strings.HasPrefix(extra[0], "-") {
		extra = append([]string{"all"}, extra...)
	}
	code, out, er := call(append([]string{"next", string(run)}, append(extra, "--state-dir", e.state, "--json")...)...)
	if code != 0 && code != exitEndedCode {
		e.t.Fatalf("next exited %d: %s", code, er)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil {
		e.t.Fatalf("next output is not a JSON array: %v\n%s", err, out)
	}
	return withoutEnded(ins)
}

func (e *cliEnv) status(run wf.RunID) wf.RunStatus {
	e.t.Helper()
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(e.must("status", string(run), "--json", "--state-dir", e.state)), &st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

// prompt fetches the prompt of a handed-out unit the way its child does: the
// prompt verb with the brief's claim token.
func (e *cliEnv) prompt(in wf.Instruction) string {
	e.t.Helper()
	return withBody(e.t, e.must("prompt", string(in.Run), "--claim", claimToken(e.t, in), "--state-dir", e.state))
}

// doneArgs fetches in's prompt, writes output where the unit's done reads it
// and returns the done command's arguments after the binary.
func (e *cliEnv) doneArgs(in wf.Instruction, output string) []string {
	e.t.Helper()
	e.prompt(in)
	args := []string{"done", string(in.Run), in.Name, "--state-dir", e.state}
	if in.Output != "" {
		writeAllocated(e.t, in.Output, output)
		return args
	}
	return append(args, "--output", writeAllocated(e.t, filepath.Join(e.t.TempDir(), "out.json"), output))
}

// submit finishes a handed-out unit with output through prompt and done.
func (e *cliEnv) submit(in wf.Instruction, output string) {
	e.t.Helper()
	if code, _, er := e.cli(e.doneArgs(in, output)...); code != 0 {
		e.t.Fatalf("done exited %d: %s", code, er)
	}
}

func TestGatedChainThroughCLI(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("gatedchain", "--param", "topic=tides")
	answers := map[string]string{"Draft": `{"Path":"` + string(run) + `/outline.json"}`, "Expand": `{"Path":"full.md"}`}
	var states []string
	for i := 0; i < 10; i++ {
		ins := e.next(run)
		if len(ins) == 0 {
			break
		}
		for _, in := range ins {
			states = append(states, in.State)
			out, ok := answers[in.State]
			if !ok {
				t.Fatalf("unexpected instruction at %s", in.State)
			}
			e.submit(in, out)
		}
	}
	if strings.Join(states, ",") != "Draft,Expand" {
		t.Fatalf("instructions seen = %v, want Draft then Expand; status %+v", states, e.status(run))
	}
	st := e.status(run)
	if st.Phase != wf.RunEnded || st.Terminal != "Done" || st.Outcome != wf.Success {
		t.Fatalf("status = %+v, want ended at Done with success", st)
	}
	// An ended run answers every form of next with the single ended
	// instruction and exit 7, and --json status carries the same cause.
	for _, args := range [][]string{{"next", string(run), "all", "--json"}, {"next", string(run), "2", "--json"}} {
		code, out, er := e.cli(append(args, "--state-dir", e.state)...)
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(out), &ins); err != nil {
			t.Fatalf("%v output is not a JSON array: %v\n%s", args, err, out)
		}
		if code != exitEndedCode || len(ins) != 1 || ins[0].Kind != wf.InstructionEnded || ins[0].Terminal != "Done" || ins[0].Outcome != wf.Success {
			t.Fatalf("%v = exit %d %+v (%s), want exit 7 and one ended instruction at Done", args, code, ins, er)
		}
		if ins[0].Cause != st.Cause {
			t.Errorf("%v cause %q differs from status cause %q", args, ins[0].Cause, st.Cause)
		}
	}
}

func TestConcurrentMapSubmitsFromSeparateProcesses(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("fan-cli")
	ins := e.next(run)
	if len(ins) != 2 || ins[0].Child == ins[1].Child {
		t.Fatalf("want two distinct child instructions, got %+v", ins)
	}
	var wg sync.WaitGroup
	codes := make([]int, len(ins))
	errs := make([]string, len(ins))
	for i, in := range ins {
		args := e.doneArgs(in, `{"OK":true}`)
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, errs[i] = runProc(t, os.Args[0], "", nil, args...)
		}()
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Fatalf("child %d done exited %d: %s", i, codes[i], errs[i])
		}
	}
	counts := map[wf.JournalKind]int{}
	childAccepted := 0
	for _, line := range strings.Split(strings.TrimSpace(e.must("journal", string(run), "--state-dir", e.state)), "\n") {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		counts[r.Kind]++
		if r.Kind == wf.JournalAccepted && r.Instance != "" {
			childAccepted++
		}
	}
	if childAccepted != 2 || counts[wf.JournalChildSettled] != 2 || counts[wf.JournalSubmitted] != 2 {
		t.Fatalf("child accepted/settled/submitted records = %d/%d/%d, want 2/2/2",
			childAccepted, counts[wf.JournalChildSettled], counts[wf.JournalSubmitted])
	}
	if st := e.status(run); st.Phase != wf.RunEnded || st.Terminal != "chMerged" {
		t.Fatalf("status = %+v, want ended at chMerged", st)
	}
}

func TestUnknownRunBadTokenMalformedOutputExitCodes(t *testing.T) {
	e := newCLIEnv(t)
	if code, _, er := e.cli("next", "nope", "1", "--state-dir", e.state, "--json"); code != 3 || !strings.Contains(er, `unknown run "nope"`) {
		t.Errorf("unknown run = %d %q, want 3", code, er)
	}
	if _, err := os.Stat(filepath.Join(e.state, "runs", "nope")); err == nil {
		t.Errorf("an unknown run id created a run directory")
	}
	run := e.start("agent-cli")
	in := e.next(run)[0]
	if code, _, er := e.cli("prompt", string(run), "--claim", "not-a-token", "--state-dir", e.state); code != 4 || !strings.Contains(er, "rejected") {
		t.Errorf("bad claim token = %d %q, want 4", code, er)
	}
	args := e.doneArgs(in, `this is not json`)
	// The refusal spends the attempt (C5).
	if code, _, er := e.cli(args...); code != 5 || !strings.Contains(er, "done: this attempt is spent\n") {
		t.Errorf("malformed output = %d %q, want 5 and the attempt spent", code, er)
	}
	if again := e.next(run); len(again) != 1 || again[0].Name == in.Name || again[0].Attempt != 2 {
		t.Errorf("after a refusal next = %+v, want a fresh attempt 2", again)
	}
	if code, _, er := e.cli("answer", string(run), "--state-dir", e.state, "--gate", "Nowhere", "--value", "x"); code != 4 {
		t.Errorf("answer with no such gate = %d %q, want 4", code, er)
	}
	if code, _, _ := e.cli("settle", string(run), "--state-dir", e.state, "--effect", "1"); code != 2 {
		t.Errorf("settle without --applied or --not-applied = %d, want 2", code)
	}
	if code, _, er := e.cli("frobnicate"); code != 2 || !strings.Contains(er, "usage:") {
		t.Errorf("unknown subcommand = %d %q, want 2 and usage", code, er)
	}
}

func TestNextShowsDispatchAndPrompt(t *testing.T) {
	e := newCLIEnv(t)
	in := e.next(e.start("agent-cli"))[0]
	prompt := e.prompt(in)
	want := wf.DispatchHints{Model: "opus", Effort: "high", AgentType: "reviewer", Tier: wf.TierStrong}
	if in.Dispatch != want {
		t.Errorf("dispatch = %+v, want %+v", in.Dispatch, want)
	}
	if !strings.Contains(prompt, "\nSay hello.") || !strings.Contains(prompt, "## Submission") {
		t.Errorf("prompt = %q, want the rendered prompt with its Submission block", prompt)
	}
	if in.Workspace != e.ws {
		t.Errorf("workspace = %q, want %q", in.Workspace, e.ws)
	}
}

// TestSubmissionBlockCommandParses: the done line the fetched prompt names
// runs as printed.
func TestSubmissionBlockCommandParses(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	in := e.next(run)[0]
	prompt := e.prompt(in)
	var line string
	for _, l := range strings.Split(prompt, "\n") {
		if i := strings.Index(l, " done "+string(run)); i >= 0 {
			line = l[i:]
			break
		}
	}
	if line == "" {
		t.Fatalf("the prompt carries no done line:\n%s", prompt)
	}
	line, _, _ = strings.Cut(line, ",")
	writeAllocated(t, in.Output, `{"OK":true}`)
	if code, _, er := e.cli(strings.Fields(line)...); code != 0 {
		t.Fatalf("done line %q from the prompt: %d %q", line, code, er)
	}
	if st := e.status(run); st.Phase != wf.RunEnded {
		t.Errorf("status = %+v, want ended", st)
	}
}

func TestRequiredParam(t *testing.T) {
	e := newCLIEnv(t)
	args := []string{"run", "needs-cli", "--state-dir", e.state, "--workspace", e.ws, "--run-id", "needs-cli-1"}
	code, out, er := e.cli(args...)
	if code != 2 || out != "" || !strings.Contains(er, "requires --param skillsRoot") {
		t.Fatalf("without the param = %d %q %q, want 2 naming skillsRoot", code, out, er)
	}
	code, out, er = e.cli(append(args, "--param", "skillsRoot=/skills")...)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "needs-cli-") {
		t.Fatalf("with the param = %d %q %q, want a run id", code, out, er)
	}
	if code, _, er := e.cli("run", "nope", "--state-dir", e.state); code != 2 || !strings.Contains(er, "unknown definition nope") {
		t.Errorf("unknown definition = %d %q", code, er)
	}
}

func TestSubmitFromDifferentCwd(t *testing.T) {
	e := newCLIEnv(t)
	in := e.next(e.start("agent-cli"))[0]
	if code, _, er := runProc(t, os.Args[0], t.TempDir(), nil, e.doneArgs(in, `{"OK":true}`)...); code != 0 {
		t.Fatalf("done from another cwd exited %d: %s", code, er)
	}
	if st := e.status(wf.RunID(in.Run)); st.Phase != wf.RunEnded {
		t.Errorf("status = %+v, want ended", st)
	}
}

var tokenRE = regexp.MustCompile(`[0-9a-f]{16,}`)

func TestNextOutputGolden(t *testing.T) {
	e := newCLIEnv(t)
	e.start("agent-cli", "--run-id", "golden")
	out := e.must("next", "golden", "all", "--state-dir", e.state, "--json")
	bin, _ := os.Executable()
	var norm []string
	for _, r := range [][2]string{{bin, "<bin>"}, {e.state, "<state>"}, {e.ws, "<ws>"}} {
		norm = append(norm, r[0], r[1])
	}
	got := strings.NewReplacer(norm...).Replace(out)
	var in []wf.Instruction
	if err := json.Unmarshal([]byte(out), &in); err != nil || len(in) != 1 {
		t.Fatalf("next output: %v %s", err, out)
	}
	got = strings.ReplaceAll(got, claimToken(t, in[0]), "<token>")
	if tokenRE.MatchString(got) {
		t.Fatalf("an unnormalised token remains:\n%s", got)
	}
	golden := filepath.Join("testdata", "next_golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("next output differs from %s (UPDATE_GOLDEN=1 rewrites it):\n%s", golden, got)
	}
}

func TestModelMapFlag(t *testing.T) {
	e := newCLIEnv(t)
	for _, bad := range []string{"opus", "opus=", "=sonnet", "a=b,c", "a=b,a=c"} {
		if code, _, er := e.cli("run", "agent-cli", "--state-dir", e.state, "--run-id", "mm", "--model-map", bad); code != 2 || !strings.Contains(er, "--model-map") {
			t.Errorf("--model-map %q = %d %q, want 2", bad, code, er)
		}
	}
	if m, err := wf.ParseModelMap("opus=sonnet,fable=haiku"); err != nil || len(m) != 2 || m["opus"] != "sonnet" || m["fable"] != "haiku" {
		t.Errorf("ParseModelMap = %v, %v", m, err)
	}
	run := e.start("agent-cli", "--model-map", "opus=sonnet")
	ins := e.nextBy(func(a ...string) (int, string, string) { return e.proc("", nil, a...) }, run)
	if len(ins) != 1 || ins[0].Dispatch.Model != "sonnet" || ins[0].Dispatch.Effort != "high" {
		t.Fatalf("fresh-process next = %+v, want the model downgraded to sonnet", ins)
	}
}

// A served registration that names a literal model is refused through the CLI:
// run and lint both exit 2 and name the rule and the qualified state.
func TestServedLiteralModelRefusedThroughCLI(t *testing.T) {
	d := wf.New("lm-cli")
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	a.Dispatch(wf.Model("sonnet"))
	cfg := wf.MainConfig{Adapters: wf.NewAdapters(), Defs: []wf.Registration{{Def: d}}}
	state := t.TempDir()
	for _, args := range [][]string{
		{"run", "lm-cli", "--state-dir", state, "--run-id", "lm"},
		{"lint"},
	} {
		var o, er bytes.Buffer
		code := fixtureCLI(cfg, args, &o, &er)
		if code != 2 || !strings.Contains(er.String(), "literal-model") || !strings.Contains(er.String(), "lm-cli/") {
			t.Errorf("%v = exit %d, stderr %q; want exit 2 naming literal-model and lm-cli/<state>", args, code, er.String())
		}
	}
}

func TestRunEndsHumanInstructionsWithLaunchLine(t *testing.T) {
	const launch = "Drive this workflow run until it ends or asks for a person. Start sub-agents whenever it asks you to. Never answer a question meant for a person yourself."
	for _, binary := range []string{"", "super"} {
		t.Run("binary="+binary, func(t *testing.T) {
			cfg := wf.MainConfig{Defs: []wf.Registration{{Def: clAgentDef("launch")}}, Binary: binary}
			args := []string{"run"}
			if binary == "" {
				args = append(args, "launch")
			}
			args = append(args, "--state-dir", t.TempDir(), "--workspace", t.TempDir(), "--run-id", "launch-run")
			for range 2 { // Fresh and resumed runs carry the same launch instruction.
				code, out, er := cliCall(cfg, args...)
				if code != 0 || out != "launch-run\n" || !strings.HasSuffix(er, launch+"\n") {
					t.Fatalf("run = %d, stdout %q, stderr %q", code, out, er)
				}
			}
		})
	}
}

func TestRunPrintsIDAndResumes(t *testing.T) {
	e := newCLIEnv(t)
	if id := e.start("agent-cli", "--run-id", "fixed"); id != "fixed" {
		t.Fatalf("run id = %q, want fixed", id)
	}
	in := e.next("fixed")[0]
	if id := e.start("agent-cli", "--run-id", "fixed"); id != "fixed" {
		t.Fatalf("resumed id = %q", id)
	}
	if again := e.next("fixed"); len(again) != 0 {
		t.Errorf("resume reissued the handed unit %s: %+v", in.Name, again)
	}
	a := e.start("gatedchain", "--param", "topic=same")
	b := e.start("gatedchain", "--param", "topic=same")
	c := e.start("gatedchain", "--param", "topic=other")
	if a != b || a == c || !strings.HasPrefix(string(a), "gatedchain-") {
		t.Errorf("run key ids = %q %q %q, want the same key to resume and another to differ", a, b, c)
	}
	if code, _, er := e.cli("run", "gatedchain", "--state-dir", e.state); code != 2 || !strings.Contains(er, "topic") {
		t.Errorf("run without its run key = %d %q, want 2 naming topic", code, er)
	}
}

func TestSettleAndNextEffectField(t *testing.T) {
	for _, applied := range []bool{true, false} {
		e := newCLIEnv(t)
		run := e.start("cmd-cli")
		if code, _, _ := e.proc("", []string{"WF_TEST_CRASH=1"}, "next", string(run), "all", "--state-dir", e.state); code != 3 {
			t.Fatalf("crashing next exited %d, want the crash code 3", code)
		}
		ins := e.next(run)
		if len(ins) != 1 || ins[0].Kind != wf.InstructionManualVerification || ins[0].Effect == 0 || ins[0].Workspace != e.ws {
			t.Fatalf("next = %+v, want one manual-verification instruction with an effect id and the workspace", ins)
		}
		if !strings.Contains(ins[0].Submit, " settle "+string(run)) {
			t.Errorf("settle line = %q", ins[0].Submit)
		}
		flag := "--applied"
		if !applied {
			flag = "--not-applied"
		}
		effect := ins[0].Effect
		args := []string{"settle", string(run), "--state-dir", e.state, "--effect", itoa(effect), flag, "--evidence", "checked"}
		if applied {
			args = append(args, "--exit", "0") // a command's routing reads its exit code
		}
		if code, _, er := e.cli(args...); code != 0 {
			t.Fatalf("settle %s exited %d: %s", flag, code, er)
		}
		if code, _, er := e.cli(args...); code != 4 || !strings.Contains(er, "already settled") {
			t.Errorf("second settle = %d %q, want 4", code, er)
		}
		st := e.status(run)
		if applied && st.Phase == wf.RunBlocked || !applied && st.Phase != wf.RunBlocked {
			t.Errorf("applied=%v: phase = %s", applied, st.Phase)
		}
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestFailThroughCLI(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	first := e.next(run)[0]
	e.prompt(first)
	code, _, er := e.cli("failed", string(run), first.Name, "--state-dir", e.state, "--reason", "gave up early")
	if code != 0 || !strings.Contains(er, "attempt 2 queued") {
		t.Fatalf("failed = %d %q, want 0 and 'attempt 2 queued'", code, er)
	}
	second := e.next(run)[0]
	if second.Name == first.Name || second.Attempt != 2 {
		t.Fatalf("after failed next = %+v, want a fresh attempt 2", second)
	}
	if code, _, er := e.cli("failed", string(run), first.Name, "--state-dir", e.state, "--reason", "again"); code != 4 {
		t.Errorf("failed on the superseded unit = %d %q, want 4", code, er)
	}
}

// quoteForSh single-quotes s for a POSIX shell, as whoever fills a placeholder
// must.
func quoteForSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestEmittedLinesRunInARealShell runs the brief's prompt command and the
// prompt's done line, and fills the documented placeholders of every emitted
// answer and settle line, running each through sh with a state directory
// whose name needs quoting.
func TestEmittedLinesRunInARealShell(t *testing.T) {
	e := newCLIEnv(t)
	e.state = filepath.Join(t.TempDir(), "it's a $x dir")
	sh := func(line string, env ...string) (int, string, string) {
		t.Helper()
		if strings.Contains(line, "|") {
			t.Fatalf("emitted line has a bare alternative: %q", line)
		}
		return runProc(t, "sh", "", env, "-c", line)
	}
	records := func(run wf.RunID) []wf.JournalRecord {
		t.Helper()
		var recs []wf.JournalRecord
		for _, l := range strings.Split(strings.TrimSpace(e.must("journal", string(run), "--state-dir", e.state)), "\n") {
			var r wf.JournalRecord
			if err := json.Unmarshal([]byte(l), &r); err != nil {
				t.Fatal(err)
			}
			recs = append(recs, r)
		}
		return recs
	}
	last := func(run wf.RunID, k wf.JournalKind) wf.JournalRecord {
		t.Helper()
		recs := records(run)
		for i := len(recs) - 1; i >= 0; i-- {
			if recs[i].Kind == k {
				return recs[i]
			}
		}
		t.Fatalf("no %v record in %d records", k, len(recs))
		return wf.JournalRecord{}
	}

	// prompt and done: the brief's command and the prompt's done line, with a
	// state dir that needs quoting.
	run := e.start("agent-cli")
	in := e.next(run)[0]
	cmd, ok := wf.BriefCommand(in.Brief)
	if !ok {
		t.Fatalf("brief = %q, want the child's brief around its command", in.Brief)
	}
	code, prompt, er := sh(cmd, "WF_TEST_CLI=1")
	if code != 0 {
		t.Fatalf("brief command %q exited %d: %s", cmd, code, er)
	}
	prompt = withBody(t, prompt)
	_, line, found := strings.Cut(prompt, "- Then finish. A refused done spends your attempt; never re-run a command that was accepted:\n  ")
	line, _, _ = strings.Cut(line, "\n")
	if !found || !strings.Contains(line, " done ") {
		t.Fatalf("prompt has no done line:\n%s", prompt)
	}
	writeAllocated(t, in.Output, `{"OK":true}`)
	if code, _, er := sh(line, "WF_TEST_CLI=1"); code != 0 {
		t.Fatalf("done line %q exited %d: %s", line, code, er)
	}
	var sub wf.SubmittedData
	if err := json.Unmarshal(last(run, wf.JournalSubmitted).Data, &sub); err != nil || string(sub.Output) != `{"OK":true}` {
		t.Errorf("submitted output = %s (%v), want the file's contents", sub.Output, err)
	}

	// answer: <value> is a value holding a space and an apostrophe.
	grun := e.start("gate-cli")
	gin := e.next(grun)[0]
	if gin.Kind != wf.InstructionStopped || gin.Question == "" {
		t.Fatalf("instruction = %+v, want a human gate", gin)
	}
	line = strings.Replace(gin.Submit, "<value>", quoteForSh("a b'c"), 1)
	if code, _, er := sh(line, "WF_TEST_CLI=1"); code != 0 {
		t.Fatalf("answer line %q exited %d: %s", line, code, er)
	}
	var ans wf.GateAnsweredData
	if err := json.Unmarshal(last(grun, wf.JournalGateAnswered).Data, &ans); err != nil || ans.Answer != "a b'c" {
		t.Errorf("answer = %q (%v), want a b'c verbatim", ans.Answer, err)
	}

	// settle: <settlement> is --not-applied, <evidence> a quoted text holding
	// a space, an apostrophe and a dollar sign.
	srun := e.start("cmd-cli")
	if code, _, _ := e.proc("", []string{"WF_TEST_CRASH=1"}, "next", string(srun), "all", "--state-dir", e.state); code != 3 {
		t.Fatalf("crashing next exited %d, want 3", code)
	}
	sin := e.next(srun)[0]
	line = strings.Replace(sin.Submit, "<settlement>", "--not-applied", 1)
	const evidence = "out.txt absent; it's not there, $HOME untouched"
	line = strings.Replace(line, "<evidence>", quoteForSh(evidence), 1)
	if code, _, er := sh(line, "WF_TEST_CLI=1"); code != 0 {
		t.Fatalf("settle line %q exited %d: %s", line, code, er)
	}
	var set wf.EffectSettledData
	if err := json.Unmarshal(last(srun, wf.JournalEffectSettled).Data, &set); err != nil || set.Applied || set.Intent == 0 || set.Evidence != evidence {
		t.Errorf("settlement = %+v (%v), want not applied with the evidence verbatim", set, err)
	}
}

// envProbeDef is one shell command that exits 0 only when it sees X=1 and no Y.
func envProbeDef() *wf.Def {
	d := wf.New("env-probe")
	exits := wf.NewEnum(d, "ep-exit", clExit(0), clExit(1))
	cmd := shell.Command[clMake](d, shell.Run("sh", "-c", `[ "$X" = 1 ] && ! env | grep -q '^Y='`))
	ok := wf.NewTerminal[clOK](d, wf.Success)
	fail := wf.NewTerminal[clFail](d, wf.Stopped)
	cmd.RouteCases(wf.Cases[clMake, wf.RunResult](wf.ExitAs[clExit], exits,
		wf.On(clExit(0), cmd.To(ok, "sees the env")),
		wf.On(clExit(1), cmd.To(fail, "misses the env"))))
	return d
}

func envProbeOutcome(t *testing.T, envFile string) (int, string) {
	t.Helper()
	reg := wf.NewAdapters()
	if err := reg.Register(shell.Runtime(shell.Config{})); err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Adapters: reg, Defs: []wf.Registration{{Def: envProbeDef()}}}
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if envFile != "" {
		if err := os.WriteFile(filepath.Join(state, "env"), []byte(envFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("Y", "inherited")
	t.Setenv("X", "")
	os.Unsetenv("X")
	call := func(args ...string) (int, string, string) {
		var o, er bytes.Buffer
		code := fixtureCLI(cfg, args, &o, &er)
		return code, o.String(), er.String()
	}
	code, out, er := call("run", "env-probe", "--state-dir", state, "--workspace", ws, "--run-id", "env-probe-1")
	if code != 0 {
		return code, er
	}
	run := strings.TrimSpace(out)
	if code, _, er = call("next", run, "all", "--state-dir", state, "--json"); code != 0 && code != exitEndedCode {
		return code, er
	}
	code, out, er = call("status", run, "--json", "--state-dir", state)
	if code != 0 {
		return code, er
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	return 0, st.Terminal
}

func TestStateEnvFileAppliesToScripts(t *testing.T) {
	if code, term := envProbeOutcome(t, "X=1\n-Y\n"); code != 0 || term != "clOK" {
		t.Errorf("with the env file: exit %d, terminal %q, want the script to see X=1 and no Y (terminal clOK)", code, term)
	}
	if code, term := envProbeOutcome(t, ""); code != 0 || term != "clFail" {
		t.Errorf("without the env file: exit %d, terminal %q, want the probe to fail (terminal clFail)", code, term)
	}
}

func TestBadStateEnvLineIsInfra(t *testing.T) {
	for _, bad := range []string{"nonsense\n", "=v\n", "-\n"} {
		if code, msg := envProbeOutcome(t, bad); code != 6 || !strings.Contains(msg, "env:") {
			t.Errorf("env file %q: exit %d %q, want 6 naming the file", bad, code, msg)
		}
	}
}

// TestEveryVerbIsOneRow: usage and dispatch read the same table, so every row
// names a line of usage and a verb CLI dispatches.
func TestEveryVerbIsOneRow(t *testing.T) {
	var usage bytes.Buffer
	fixtureCLI(wf.MainConfig{}, []string{"no-such-verb"}, &bytes.Buffer{}, &usage)
	seen := map[string]bool{}
	for _, row := range wf.Subcommands {
		if seen[row.Name] {
			t.Errorf("verb %q has two rows", row.Name)
		}
		seen[row.Name] = true
		if !strings.Contains(usage.String(), "\n  "+row.Name) {
			t.Errorf("usage has no line for %q", row.Name)
		}
		var o, er bytes.Buffer
		fixtureCLI(wf.MainConfig{}, []string{row.Name}, &o, &er)
		if strings.Contains(er.String(), "unknown subcommand") {
			t.Errorf("CLI does not dispatch %q: %s", row.Name, er.String())
		}
	}
}

// TestUsageUnchanged pins the usage text, including the run harness flag.
func TestUsageUnchanged(t *testing.T) {
	want := `no-such-verb: unknown subcommand
usage: workflow <subcommand> [args]
  attach-coordinator --state-dir dir --assets dir --workspace dir --initial --previous-run ID --previous-state-dir DIR select this named session to coordinate the run
  may-end-turn --state-dir dir --assets dir --workspace dir --json read the committed coordinator stop decision
  acknowledge-handoff --state-dir dir --assets dir --workspace dir --receipt ID acknowledge a current person handoff
  coordinator-stop --hook HARNESS --workspace DIR handle a native coordinator Stop event
  install-coordinator-stop --hook HARNESS --workspace DIR install a local coordinator Stop hook for this client
  lint      lint the definitions (optional definition name)
  graph     render the definitions (the default)
  mermaid   render the definitions as Mermaid
  hash      print the definition hashes
  version  --json print this binary's build stamp: engine and workflow source, Go toolchain, journal format (--json: the BuildStamp)
  run      --param k=v --options file --workspace dir --state-dir dir --assets dir --run-id id --snapshots --paged-prompts --halt-before STATE[,STATE] --model-map key=model[,key=model] --delegate gate=value --harness h --continue run start or resume a run of <def>; prints the RunID on stdout and the coordinator bootstrap on stderr
  next     --expected-admission seq --until-unit name --state-dir dir --assets dir --workspace dir --timeout d --children k --adopt --json block until <run> has something for the coordinator, hand out up to n ready units (left out: up to your child cap, --children or the harness's own limit where its adapter reads one, less your children running or starting, else every ready unit) and print one line per item; --json: hand out at once, without blocking, as JSON, for a program
  stop     --state-dir dir --assets dir --workspace dir --reason r stop <run>: next offers nothing from now on and running children finish; a person's resume continues it
  resume   --state-dir dir --assets dir --workspace dir --operator name --reason r --adopt --model-map key=model[,key=model] clear a stop of <run> as its operator (--operator, --reason: journaled; the run's own coordinator session is refused); --adopt carries it onto a changed definition whose live states are unchanged; --model-map sets the models from this resume on
  prompt   --state-dir dir --assets dir --workspace dir --claim token --page N --code code --json claim a handed unit by --claim token and print its prompt (--json: full prompt and inputs)
  read     --state-dir dir --assets dir --workspace dir --claim token --ref id --page N --code code fetch one bounded prompt reference page
  done     --state-dir dir --claim token --assets dir --workspace dir --output allocated-file --usage tokens=N --escalated 'command: reason: outcome' read a claimed unit's allocated result
  validate --state-dir dir --claim token --assets dir --workspace dir --output allocated-file check a claimed unit's allocated result as done would, recording nothing
  failed   --state-dir dir --claim token --assets dir --workspace dir --reason r --class transient|blocked|approval --escalated 'command: reason: outcome' abandon a claimed unit's attempt
  answer   --state-dir dir --assets dir --workspace dir --gate state --value choice --number INT --child id --visit n answer a human gate (optional --number overrides its integer proposal)
  settle   --state-dir dir --assets dir --workspace dir --effect id --applied [--exit N [--stdout-file f]] | --not-applied --evidence text --operator name --reason r settle an Uncertain effect
  retry    --state-dir dir --assets dir --workspace dir --child id --stop --approval-hold seq return a halted instance of <run>, or release its held unit, to ready at the next attempt (--stop: answer a retry-exhausted halt by taking its fallback edge)
  recover  --state-dir dir --assets dir --workspace dir answer a unit's recovery offer: resume (the same child continues, where the harness can), restart or give-up
  status   --state-dir dir --assets dir --workspace dir --json --full print one line per subagent unit and a totals line (--json: the RunStatus as JSON); from an agent's shell on a running run, one line that says to run next (--full: the whole view)
  heartbeat --state-dir dir --assets dir --workspace dir --session s --note text answer a stall: active <name> [--note <work you saw>], died <name>, or died --session <id> for a gone coordinator's orphans
  explain  --state-dir dir --assets dir --workspace dir explain a unit with its transition, route, checks and effects
  journal  --state-dir dir --assets dir --workspace dir print the journal as JSON Lines
  lock     --state-dir dir --assets dir --workspace dir --json say whether a process holds the run's lock now, and who last held it (read without taking it)
  check     check <definition> without a model: lint, calibration and simulation
Read this command's --help and correct its arguments, then run it again.
`
	var er bytes.Buffer
	if code := fixtureCLI(wf.MainConfig{}, []string{"no-such-verb"}, &bytes.Buffer{}, &er); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if er.String() != want {
		t.Errorf("usage changed:\n%s\nwant:\n%s", er.String(), want)
	}
}

// TestNextWithCount: next <run> <n> hands out n agent units, paged; n is
// required, a count or all.
func TestNextWithCount(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("fan-cli")

	code, out, er := e.cli("next", string(run), "1", "--state-dir", e.state, "--json")
	if code != 0 {
		t.Fatalf("next <run> 1 exited %d: %s", code, er)
	}
	var paged []map[string]any
	if err := json.Unmarshal([]byte(out), &paged); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, out)
	}
	if len(paged) != 1 {
		t.Fatalf("next <run> 1 printed %d instructions, want 1:\n%s", len(paged), out)
	}
	for _, k := range []string{"name", "class", "brief"} {
		if s, _ := paged[0][k].(string); s == "" {
			t.Errorf("paged instruction has no %s: %v", k, paged[0])
		}
	}
	for _, k := range []string{"prompt", "token", "submit"} {
		if _, has := paged[0][k]; has {
			t.Errorf("paged instruction carries %s: %v", k, paged[0])
		}
	}
	if brief, _ := paged[0]["brief"].(string); !strings.Contains(brief, " prompt "+string(run)+" --claim ") || !strings.Contains(brief, "--state-dir "+e.state) {
		t.Errorf("brief does not name the running binary's prompt verb with the state dir:\n%s", brief)
	}

	code, out, _ = e.cli("next", string(run), "2", "--state-dir", e.state, "--json")
	var rest []wf.Instruction
	if err := json.Unmarshal([]byte(out), &rest); err != nil || code != 0 || len(rest) != 1 || rest[0].Name == "" || rest[0].Child == paged[0]["child"] {
		t.Fatalf("second next <run> 2 = %d %+v (%v), want the one unit not yet handed", code, rest, err)
	}

	if left := e.next(run); len(left) != 0 {
		t.Fatalf("next <run> all with both units handed = %+v, want none", left)
	}

	for _, bad := range [][]string{{}, {"x"}, {"-1"}, {"1", "--child", "x"}} {
		if code, _, _ := e.cli(append([]string{"next", string(run)}, append(bad, "--state-dir", e.state, "--json")...)...); code != 2 {
			t.Errorf("next %v exited %d, want 2", bad, code)
		}
	}
}

// TestPromptClaimRefusedExitCode: a re-offered unit has two journaled claim
// tokens; once the first child's prompt --claim binds its token, the second
// child's prompt --claim exits 4 "another child already owns this unit".
func TestPromptClaimRefusedExitCode(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	sd := []string{"--state-dir", e.state}
	code, o, er := e.cli(append([]string{"next", string(run), "1", "--json"}, sd...)...)
	var first []wf.Instruction
	if err := json.Unmarshal([]byte(o), &first); err != nil || code != 0 || len(first) != 1 {
		t.Fatalf("next 1 = %d %q (%v) %s", code, o, err, er)
	}
	name := first[0].Name
	// The CLI has no clock to turn: re-offer through an engine on the same
	// journal whose clock is past the hand-out window.
	later := time.Now().Add(3 * time.Minute)
	eng := wf.NewEngine(wf.FileJournal(e.state), clConfig().Adapters, wf.EngineOptions{
		Defs: []*wf.Def{clAgentDef("agent-cli")}, Clock: func() time.Time { return later },
	})
	again, err := eng.HandOut(run, 1)
	if err != nil || len(again) != 1 || again[0].Name != name {
		t.Fatalf("re-offer = %+v, %v, want %s again", again, err, name)
	}
	t1, t2 := claimToken(t, first[0]), claimToken(t, again[0])
	if code, _, er := e.cli(append([]string{"prompt", string(run), name, "--claim", t1}, sd...)...); code != 0 {
		t.Fatalf("first child's prompt --claim exited %d: %s", code, er)
	}
	code, _, er = e.cli(append([]string{"prompt", string(run), name, "--claim", t2}, sd...)...)
	if code != 4 || !strings.Contains(er, "another child already owns this unit; exit without working") {
		t.Fatalf("second child's prompt --claim = %d %q, want 4 telling the duplicate to exit without working", code, er)
	}
	if !strings.Contains(er, "end only your child task") ||
		!strings.Contains(er, "The coordinator continues with next") ||
		strings.Contains(er, "stop and report this to the person who started the run") {
		t.Fatalf("second child's prompt --claim gave the wrong stop guidance: %q", er)
	}
	if code, _, er := e.cli(append([]string{"done", string(run), name, "--claim", t1, "--output",
		writeAllocated(t, first[0].Output, `{"OK":true}`)}, sd...)...); code != 0 {
		t.Fatalf("the winning child could not finish after the duplicate refusal: exit %d %s", code, er)
	}
	// The duplicate can also reach prompt after the winner has finished.
	// Its token no longer resolves to a live unit, as in the regression case.
	code, _, er = e.cli(append([]string{"prompt", string(run), "--claim", t2}, sd...)...)
	if code != 4 || !strings.Contains(er, "use the command in your task text") ||
		!strings.Contains(er, "The coordinator continues with next") ||
		strings.Contains(er, "stop and report this to the person who started the run") {
		t.Fatalf("late duplicate's prompt --claim = exit %d %q, want child-only exit guidance", code, er)
	}
}

// TestPagedVerbsExitCodes: the subagent verbs through the CLI, with their exit
// codes: a second agent's claim and a done on a retired name are 4, a refused
// output is 5.
func TestPagedVerbsExitCodes(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	sd := []string{"--state-dir", e.state}
	verb := func(args ...string) (int, string, string) {
		return e.cli(append(args, sd...)...)
	}
	out := func(content string) string {
		f := filepath.Join(t.TempDir(), "out.json")
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}

	code, o, er := e.cli(append([]string{"next", string(run), "1", "--json"}, sd...)...)
	var handed []wf.Instruction
	if err := json.Unmarshal([]byte(o), &handed); err != nil || code != 0 || len(handed) != 1 {
		t.Fatalf("next 1 = %d %q (%v) %s", code, o, err, er)
	}
	name := handed[0].Name

	started := func(agent string) int {
		code, _, _ := verb("dev", "started", string(run), name, "--harness", "claude", "--session", "s1", "--agent", agent, "--transcript", "/t.jsonl")
		return code
	}
	if code, _, _ := verb("dev", "started", string(run), name, "--agent", "a"); code != 2 {
		t.Errorf("started without --session exited %d, want 2", code)
	}
	if got := started("a"); got != 0 {
		t.Fatalf("started by a exited %d, want 0", got)
	}
	if got := started("a"); got != 0 {
		t.Errorf("repeat started by a exited %d, want 0", got)
	}
	if got := started("b"); got != 4 {
		t.Errorf("started by b exited %d, want 4", got)
	}

	code, o, er = verb("prompt", string(run), name)
	o = withBody(t, o)
	if code != 0 || !strings.Contains(o, "Say hello.") || !strings.Contains(o, " done "+string(run)+" "+name+" --state-dir "+e.state) {
		t.Fatalf("prompt = %d %q %s, want the prompt text and the done line", code, o, er)
	}

	// validate prints done's refusal and records nothing; a refused done
	// prints the reason, then that the attempt is spent (C5).
	bad := writeAllocated(t, handed[0].Output, `"not an object"`)
	code, _, er = verb("validate", string(run), name, "--output", bad)
	if code != 5 || !strings.HasPrefix(er, "validate: refused: undecodable submission: ") || !strings.Contains(er, "validate: nothing was recorded: fix the output and validate again, then run done\n") {
		t.Errorf("validate of a refused output = %d %q", code, er)
	}
	code, _, er = verb("done", string(run), name, "--output", bad)
	if code != 5 {
		t.Fatalf("done with a refused output exited %d, want 5: %s", code, er)
	}
	if lines := strings.Split(strings.TrimSuffix(er, "\n"), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], "done: refused: undecodable submission: ") || lines[1] != "done: this attempt is spent" || !strings.Contains(lines[2], "end only your child task") {
		t.Errorf("done refused printed %q, want the reason, the spent line and child-only exit guidance", er)
	}
	if code, _, _ = verb("done", string(run), name, "--output", out(`{"OK":true}`)); code != 4 {
		t.Errorf("done on the retired name exited %d, want 4", code)
	}
	if code, _, _ = verb("prompt", string(run), name); code != 4 {
		t.Errorf("prompt on the retired name exited %d, want 4", code)
	}
	// A late started on the ended unit attaches and says so; a different agent
	// or an unknown name is refused (exit 4).
	if code, o, er = verb("dev", "started", string(run), name, "--harness", "claude", "--session", "s1", "--agent", "a"); code != 0 || !strings.Contains(o, "note: unit already failed") {
		t.Errorf("late started exited %d %q %s, want 0 and the note", code, o, er)
	}
	if code, _, er = verb("dev", "started", string(run), name, "--session", "s1", "--agent", "b"); code != 4 || !strings.Contains(er, `agent "a" is already attached, not "b"`) {
		t.Errorf("conflicting late started exited %d %s, want 4 naming the conflict", code, er)
	}
	if code, _, _ = verb("dev", "started", string(run), "no/such#1", "--session", "s1", "--agent", "a"); code != 4 {
		t.Errorf("started on an unknown name exited %d, want 4", code)
	}

	// The refusal reissued the state: hand it out, start it, finish it.
	code, o, er = e.cli(append([]string{"next", string(run), "1", "--json"}, sd...)...)
	if err := json.Unmarshal([]byte(o), &handed); err != nil || code != 0 || len(handed) != 1 || handed[0].Name == name {
		t.Fatalf("next 1 after the refusal = %d %q (%v) %s, want a new unit", code, o, err, er)
	}
	name = handed[0].Name
	if got := started("a"); got != 0 {
		t.Fatalf("started on the reissue exited %d", got)
	}
	if code, _, er = verb("failed", string(run), name, "--reason", "no"); code != 0 || strings.Contains(er, "refused") {
		t.Fatalf("failed exited %d, want 0 (an accepted failure is not a refusal): %s", code, er)
	}
	// B2: the spent budget is offered to the coordinator; give up takes the
	// declared exhausted edge.
	if !strings.HasPrefix(er, "failed: offered for recovery: retry exhausted: abandoned: no; answer with one of:\n  ") || !strings.Contains(er, "recover "+string(run)+" "+name+" give-up") {
		t.Errorf("failed with retries spent printed %q, want the recovery line with its commands", er)
	}
	if code, _, er = verb("recover", string(run), name, "give-up"); code != 0 {
		t.Fatalf("recover give-up exited %d: %s", code, er)
	}
	if st := e.status(run); st.Phase != wf.RunEnded || st.Terminal != "clGaveUp" {
		t.Errorf("status = %+v, want the run ended at clGaveUp after give-up", st)
	}
}

// TestStartedTranscriptMadeAbsolute: a relative --transcript is resolved
// against the caller's cwd when the claim is recorded.
func TestStartedTranscriptMadeAbsolute(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	code, o, er := e.cli("next", string(run), "1", "--state-dir", e.state, "--json")
	var handed []wf.Instruction
	if err := json.Unmarshal([]byte(o), &handed); err != nil || code != 0 || len(handed) != 1 {
		t.Fatalf("next 1 = %d %q (%v) %s", code, o, err, er)
	}

	cwd := t.TempDir()
	t.Chdir(cwd)
	want := filepath.Join(cwd, "t.jsonl")
	if code, _, er = e.cli("dev", "started", string(run), handed[0].Name, "--state-dir", e.state,
		"--session", "s1", "--agent", "a", "--transcript", "t.jsonl"); code != 0 {
		t.Fatalf("started exited %d: %s", code, er)
	}
	_, journal, _ := e.cli("journal", string(run), "--state-dir", e.state)
	var got string
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		var rec wf.JournalRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Kind != wf.JournalAttemptStarted {
			continue
		}
		var d wf.AttemptStartedData
		if err := json.Unmarshal(rec.Data, &d); err != nil {
			t.Fatal(err)
		}
		got = d.Transcript
	}
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("attempt_started.Transcript = %q, want %q", got, want)
	}
}

// TestHeartbeatVerb: heartbeat died --session loses only that session's units,
// a late done from the orphan is exit 4, and a malformed form is exit 2.
func TestHeartbeatVerb(t *testing.T) {
	e := newCLIEnv(t)
	run := e.start("agent-cli")
	sd := []string{"--state-dir", e.state}
	code, o, er := e.cli(append([]string{"next", string(run), "1", "--json"}, sd...)...)
	var handed []wf.Instruction
	if err := json.Unmarshal([]byte(o), &handed); err != nil || code != 0 || len(handed) != 1 {
		t.Fatalf("next 1 = %d %q (%v) %s", code, o, err, er)
	}
	name := handed[0].Name
	if code, _, er = e.cli(append([]string{"dev", "started", string(run), name, "--session", "X", "--agent", "a"}, sd...)...); code != 0 {
		t.Fatalf("started exited %d: %s", code, er)
	}
	for _, bad := range [][]string{
		{"heartbeat", string(run)},
		{"heartbeat", string(run), "active"},
		{"heartbeat", string(run), "alive", name},
		{"heartbeat", string(run), "active", name, "--session", "X"},
		{"heartbeat", string(run), "active", "--session", "X"},
		{"heartbeat", string(run), "died"},
	} {
		if code, _, _ := e.cli(append(bad, sd...)...); code != 2 {
			t.Errorf("%v exited %d, want 2", bad, code)
		}
	}
	if code, _, er = e.cli(append([]string{"heartbeat", string(run), "active", name}, sd...)...); code != 0 {
		t.Fatalf("heartbeat active exited %d: %s", code, er)
	}
	if code, _, _ = e.cli(append([]string{"heartbeat", string(run), "died", "--session", "Y"}, sd...)...); code != 0 {
		t.Fatalf("heartbeat died --session Y exited %d, want 0 (no orphans)", code)
	}
	if code, _, er = e.cli(append([]string{"heartbeat", string(run), "died", "--session", "X"}, sd...)...); code != 0 {
		t.Fatalf("heartbeat died --session X exited %d: %s", code, er)
	}
	out := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(out, []byte(`{"OK":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = e.cli(append([]string{"done", string(run), name, "--output", out}, sd...)...); code != 4 {
		t.Errorf("done from the orphan exited %d, want 4", code)
	}
}

// singleCLI runs wf.CLI in single-registration mode over def.
func singleCLI(def *wf.Def, args ...string) (int, string, string) {
	cfg := wf.MainConfig{Adapters: clConfig().Adapters, Defs: []wf.Registration{{Def: def}}, Binary: "super"}
	var o, er bytes.Buffer
	code := fixtureCLI(cfg, args, &o, &er)
	return code, o.String(), er.String()
}

// cliCall runs wf.CLI over cfg in this process.
func cliCall(cfg wf.MainConfig, args ...string) (int, string, string) {
	var out, er strings.Builder
	code := fixtureCLI(cfg, args, &out, &er)
	return code, out.String(), er.String()
}

func mustSingle(t *testing.T, def *wf.Def, args ...string) string {
	t.Helper()
	code, out, er := singleCLI(def, args...)
	if code != 0 {
		t.Fatalf("%v exited %d: %s", args, code, er)
	}
	return out
}

// startSingle starts the run of a two-unit definition, hands both units out and
// claims them by sessions s1 and s2; it returns the state dir and run id.
func startSingle(t *testing.T) (def *wf.Def, state, run string) {
	t.Helper()
	def = fanDef("fan-single", chItems(1, 2), 2, wf.AllSettled())
	state = filepath.Join(t.TempDir(), "state")
	run = strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1"))
	return def, state, run
}

func TestSingleModeRunAndVerbs(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	run := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1"))
	if run == "" {
		t.Fatal("run printed no id")
	}
	if raw, err := os.ReadFile(filepath.Join(state, "run-id")); err != nil || strings.TrimSpace(string(raw)) != run {
		t.Fatalf("run-id file = %q, %v; want %q", raw, err, run)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v; want one instruction", ins, err)
	}
	if !strings.Contains(ins[0].Brief, " wf prompt --claim "+claimToken(t, ins[0])+" --state-dir "+state) {
		t.Errorf("brief does not name the wf prompt verb of the binary:\n%s", ins[0].Brief)
	}
	mustSingle(t, def, "wf", "dev", "started", ins[0].Name, "--session", "s1", "--agent", "a1", "--state-dir", state)
	prompt := withBody(t, mustSingle(t, def, "wf", "prompt", "--state-dir", state, "--claim", claimToken(t, ins[0])))
	for _, want := range []string{"background-task tool", "rather than a polling or wait loop"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not ask for long commands on the harness's background tool rather than a wait loop (missing %q):\n%s", want, prompt)
		}
	}
	out := mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil || string(st.Run) != run || len(st.Units) != 1 || st.Units[0].Session != "s1" {
		t.Fatalf("wf status --json = %+v, %v; want run %s with the unit claimed by s1", st, err, run)
	}
	// --run-id wins over the recorded id, and an unknown one is exit 3.
	if code, _, _ := singleCLI(def, "wf", "status", "--run-id", "nope", "--state-dir", state); code != 3 {
		t.Errorf("wf status --run-id nope exited %d, want 3", code)
	}
}

func TestSingleModeRunIDFromRunsDirAndAmbiguity(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	run := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1"))
	if err := os.Remove(filepath.Join(state, "run-id")); err != nil {
		t.Fatal(err)
	}
	if out := mustSingle(t, def, "wf", "status", "--json", "--state-dir", state); !strings.Contains(out, `"Run": "`+run+`"`) {
		t.Errorf("the only run under runs/ was not used:\n%s", out)
	}
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")
	if err := os.Remove(filepath.Join(state, "run-id")); err != nil {
		t.Fatal(err)
	}
	code, _, er := singleCLI(def, "wf", "status", "--state-dir", state)
	if code != 2 || !strings.Contains(er, "--run-id") {
		t.Errorf("two runs: exit %d, stderr %q; want 2 naming --run-id", code, er)
	}
	empty := filepath.Join(t.TempDir(), "none")
	code, _, er = singleCLI(def, "wf", "status", "--state-dir", empty)
	if code != 2 || !strings.Contains(er, "--run-id") {
		t.Errorf("no run: exit %d, stderr %q; want 2 naming --run-id", code, er)
	}
}

// The bootstrap names no claim metadata to supply: the engine finds its own
// children (A1). Its one command line quotes the binary, the state dir and
// the run id for the shell.
func TestBootstrapQuotesItsLoopLine(t *testing.T) {
	for _, h := range []string{"claude", "codex", "generic"} {
		out := wf.Bootstrap(wf.RunStatus{Run: "run 'one", Harness: h}, "./my client", "/state dir/'quoted")
		want := `'./my client' wf next --state-dir '/state dir/'\''quoted' --run-id 'run '\''one'`
		if !strings.Contains(out, want) {
			t.Errorf("%s bootstrap lacks the quoted loop line %s:\n%s", h, want, out)
		}
		for _, gone := range []string{"--session", "--agent", "--transcript", "CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID", "session_meta"} {
			if strings.Contains(out, gone) {
				t.Errorf("%s bootstrap still asks for %q:\n%s", h, gone, out)
			}
		}
	}
}

func TestBootstrapFreshRun(t *testing.T) {
	def, state, run := startSingle(t)
	out := mustSingle(t, def, "next", "--state-dir", state)
	for _, want := range []string{"run: " + run + "\n", "state dir: " + state + "\n", "loop:", " wf next --state-dir " + state, "fresh child agent"} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "session ") && strings.Contains(out, "live claimed") {
		t.Errorf("a fresh run names sessions:\n%s", out)
	}
	if code, _, _ := singleCLI(def, "next", "--run-id", "nope", "--state-dir", state); code != 3 {
		t.Errorf("next --run-id nope exited %d, want 3", code)
	}
}

// A resumed run's bootstrap is the same loop: a new session's next clears
// what an earlier coordinator left (B4), so no session is named.
func TestBootstrapResumedNamesNoSessions(t *testing.T) {
	def, state, run := startSingle(t)
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "2", "--state-dir", state, "--json")), &ins); err != nil || len(ins) == 0 {
		t.Fatalf("wf next 2 = %v, %v", ins, err)
	}
	mustSingle(t, def, "wf", "dev", "started", ins[0].Name, "--session", "s1", "--agent", "a1", "--state-dir", state)
	out := mustSingle(t, def, "next", "--state-dir", state)
	if !strings.Contains(out, "run: "+run) || strings.Contains(out, "session s1") {
		t.Errorf("resumed bootstrap:\n%s", out)
	}
}

func TestSingleModeStatusView(t *testing.T) {
	def, state, _ := startSingle(t)
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	mustSingle(t, def, "wf", "dev", "started", ins[0].Name, "--session", "s1", "--agent", "a1", "--state-dir", state)
	out := mustSingle(t, def, "wf", "status", "--full", "--state-dir", state)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[2], "subagents 2: 1 started, 1 pending") {
		t.Fatalf("status = %q; want two per-unit lines then the totals line", out)
	}
	if !strings.Contains(out, ins[0].Name) || !strings.Contains(out, "session s1") {
		t.Errorf("status lacks the claimed unit %s with its session:\n%s", ins[0].Name, out)
	}
}

// Ignoring the configured default at any parsing boundary sends a verb to a
// different journal; ignoring an override sends it back to the default run.
func TestMainPerEntryDefaultStateDir(t *testing.T) {
	for _, binary := range []string{"entry-client", ""} {
		t.Run("binary="+binary, func(t *testing.T) {
			t.Chdir(t.TempDir())
			for _, entry := range []struct{ name, state string }{
				{"first", ".entries/first"},
				{"second", ".entries/second"},
				{"legacy", ""},
			} {
				t.Run(entry.name, func(t *testing.T) {
					cfg := wf.MainConfig{
						Binary: binary, Defs: []wf.Registration{{Def: clAgentDef(entry.name)}},
						DefaultStateDir: entry.state,
					}
					call := func(args ...string) string {
						t.Helper()
						var out, er bytes.Buffer
						if code := fixtureCLI(cfg, args, &out, &er); code != 0 {
							t.Fatalf("%v exited %d: %s", args, code, er.String())
						}
						return out.String()
					}
					state := entry.state
					if state == "" {
						state = ".workflow"
					}
					check := func(id, dir string, flags []string) {
						t.Helper()
						args := []string{"run", entry.name, "--run-id", id}
						if out := call(append(args, flags...)...); strings.TrimSpace(out) != id {
							t.Fatalf("run printed %q, want %q", out, id)
						}
						if _, err := os.Stat(filepath.Join(dir, "runs", id)); err != nil {
							t.Fatalf("run journal missing from selected directory %s: %v", dir, err)
						}
						if binary != "" {
							abs, err := filepath.Abs(dir)
							if err != nil {
								t.Fatal(err)
							}
							out := call(append([]string{"next"}, flags...)...)
							if !strings.Contains(out, "run: "+id+"\n") || !strings.Contains(out, "state dir: "+abs+"\n") {
								t.Fatalf("bootstrap selected another run or directory: %s", out)
							}
							args = []string{"wf", "status", "--json"}
						} else {
							var ins []wf.Instruction
							out := call(append([]string{"next", id, "all", "--json"}, flags...)...)
							if err := json.Unmarshal([]byte(out), &ins); err != nil || len(ins) != 1 || string(ins[0].Run) != id {
								t.Fatalf("next selected another run: %s (%v)", out, err)
							}
							args = []string{"status", id, "--json"}
						}
						var st wf.RunStatus
						out := call(append(args, flags...)...)
						if err := json.Unmarshal([]byte(out), &st); err != nil || string(st.Run) != id || st.Def != entry.name {
							t.Fatalf("status selected another entry/run: %s (%v)", out, err)
						}
					}
					check(entry.name+"-default", state, nil)
					for _, equals := range []bool{false, true} {
						override := filepath.Join(t.TempDir(), "override")
						flags := []string{"--state-dir", override}
						if equals {
							flags = []string{"--state-dir=" + override}
						}
						check(entry.name+"-override", override, flags)
					}
				})
			}
		})
	}
}

func TestMultiModeUnchanged(t *testing.T) {
	// Without Binary the CLI is the multi-definition one: no wf group, next
	// takes a run id. (A Binary over several definitions is the named binary:
	// TestNamedBinaryServesSeveralEntries.)
	for _, cfg := range []wf.MainConfig{
		{Defs: []wf.Registration{{Def: clAgentDef("a")}}},
		{Defs: []wf.Registration{{Def: clAgentDef("a")}, {Def: clAgentDef("b")}}},
	} {
		var o, er bytes.Buffer
		if code := fixtureCLI(cfg, []string{"wf", "status"}, &o, &er); code != 2 || !strings.Contains(er.String(), "unknown subcommand") {
			t.Errorf("wf in multi mode: exit %d, stderr %q", code, er.String())
		}
		er.Reset()
		if code := fixtureCLI(cfg, []string{"next"}, &o, &er); code != 2 || !strings.Contains(er.String(), "missing run id") {
			t.Errorf("next without a run id in multi mode: exit %d, stderr %q", code, er.String())
		}
		if code := fixtureCLI(cfg, []string{"run", "a", "--state-dir", t.TempDir()}, &o, &er); code == 2 && strings.Contains(er.String(), "unknown subcommand") {
			t.Errorf("run <def> not dispatched in multi mode")
		}
	}
}

// fixtureCLI drives operator commands as a person. Other calls retain the
// suite's pinned Claude environment. Authorization tests use CLIWithEnv directly.
func fixtureCLI(cfg wf.MainConfig, args []string, out, stderr io.Writer) int {
	offset := 0
	if len(args) > 0 && args[0] == "wf" {
		offset = 1
	}
	if len(args) > offset+1 && args[offset] == "dev" && args[offset+1] == "started" {
		return wf.CLIWithEnv(cfg, args, out, stderr, func(k string) string {
			switch k {
			case "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID", "HERMES_SESSION_ID", "HERMES_DELEGATED_CHILD_CONTEXT":
				return ""
			}
			return os.Getenv(k)
		})
	}
	return wf.CLI(cfg, args, out, stderr)
}
