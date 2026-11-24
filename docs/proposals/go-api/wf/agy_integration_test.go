package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/harness/agytest"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The agy integration sweep: a wf run coordinated by agy, end to
// end, from records built by harness/agytest out of the run's own hand-out.
// Identity is cooperative (agy's records are editable by the same OS user);
// these tests prove the engine accepts a claim only from them.

const agyCoordinator = "11111111-2222-4333-8444-555555555555"

// agyChildID is the n-th child's conversation id.
func agyChildID(n int) string { return fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", n) }

// agyEnv sets the environment of agy's coordinator conversation over a fresh
// records root in t.TempDir() and never the real home.
func agyEnv(t *testing.T) *agytest.Tree {
	t.Helper()
	strictCallers(t)
	for _, k := range []string{"HERMES_SESSION_ID", "PI_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	tr := agytest.New(t)
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", tr.Root)
	tr.Coordinator(agyCoordinator, time.Now().Add(-time.Minute))
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", agyCoordinator)
	return tr
}

// agyAs is the environment of conversation id.
func agyAs(t *testing.T, id string) { t.Setenv("ANTIGRAVITY_CONVERSATION_ID", id) }

// agyRun starts run r1 of def under the agy environment and returns its state dir.
func agyRun(t *testing.T, def *wf.Def, extra ...string) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	args := append([]string{"run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1"}, extra...)
	mustSingle(t, def, args...)
	return state
}

// agyNextJSON is the coordinator's next n as instructions.
func agyNextJSON(t *testing.T, def *wf.Def, state string, n int) []wf.Instruction {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", fmt.Sprint(n), "--json", "--state-dir", state)), &ins); err != nil {
		t.Fatal(err)
	}
	return ins
}

// agyJournal is the run's journal records of kind k.
func agyJournal(t *testing.T, state string, k wf.JournalKind) []wf.JournalRecord {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "runs", "r1", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []wf.JournalRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r wf.JournalRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("journal line %q: %v", l, err)
		}
		if r.Kind == k {
			out = append(out, r)
		}
	}
	return out
}

func TestAgyRunEndToEnd(t *testing.T) {
	tr := agyEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	if got := hn.Detect(os.Getenv); got != "agy" {
		t.Fatalf("Detect = %q, want agy", got)
	}
	def := fanDef("agy-e2e", chItems(1, 2), 2, wf.AllSettled())
	state := agyRun(t, def)
	started := agyJournal(t, state, wf.JournalRunStarted)
	if len(started) != 1 {
		t.Fatalf("run_started records = %d", len(started))
	}
	var rs wf.RunStartedData
	if err := json.Unmarshal(started[0].Data, &rs); err != nil || rs.Harness != "agy" {
		t.Fatalf("run_started harness = %q (%v), want agy", rs.Harness, err)
	}

	// The hand-out: the briefs and claim tokens.
	ins := agentsOf(agyNextJSON(t, def, state, 2))
	if len(ins) != 2 {
		t.Fatalf("hand-out = %d units, want 2", len(ins))
	}
	var children []string
	for i, in := range ins {
		child := agyChildID(i + 1)
		children = append(children, child)
		prompt := tr.Start(agytest.Launch{Coordinator: agyCoordinator, Child: child, Brief: in.Brief, At: time.Now().UTC()})
		if prompt != in.Brief {
			t.Fatalf("the spawn line's Prompt differs from the hand-out's brief:\n%q\n%q", prompt, in.Brief)
		}
	}
	for i, in := range ins {
		agyAs(t, children[i])
		tok := claimToken(t, in)
		mustSingle(t, def, "wf", "prompt", "--claim", tok, "--json", "--state-dir", state)
		now := time.Now().UTC()
		tr.Respond(children[i], now, "gemini-3.8-flash-low", 1200, 80, "", agytest.Call{Name: "view_file", Args: map[string]any{"AbsolutePath": in.Output}})
		tr.Respond(children[i], now, "gemini-3.8-flash-low", 300, 20, "done")
		writeAllocated(t, in.Output, `{"OK":true}`)
		mustSingle(t, def, "wf", "done", "--claim", tok, "--state-dir", state)
	}

	subs := agyJournal(t, state, wf.JournalSubmitted)
	if len(subs) != 2 {
		t.Fatalf("submitted records = %d, want 2", len(subs))
	}
	for _, r := range subs {
		var d wf.SubmittedData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Tokens != 0 { // no --usage: the transcript's usage is never counted
			t.Errorf("submitted tokens=%d, want 0", d.Tokens)
		}
	}

	agyAs(t, agyCoordinator)
	code, out, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state)
	if code != exitEndedCode {
		t.Fatalf("next after both units: exit %d, want %d: %s%s", code, exitEndedCode, out, er)
	}
	var end []wf.Instruction
	if err := json.Unmarshal([]byte(out), &end); err != nil || len(end) != 1 || end[0].Kind != wf.InstructionEnded {
		t.Fatalf("next after both units = %v %s, want the ended instruction", err, out)
	}
}

// The offer next prints for an agy run: the flat invoke_subagent line, once
// per unit, its Prompt naming the brief below it, and no Model.
func TestAgyOfferIsTheFlatSpawnLine(t *testing.T) {
	agyEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("agy-offer", chItems(1, 2), 2, wf.AllSettled())
	state := agyRun(t, def)
	text := mustSingle(t, def, "wf", "next", "2", "--state-dir", state)
	const call = `invoke_subagent {"TypeName":"self","Role":"wf-unit","Prompt":<the text between the lines>}`
	if got := strings.Count(text, call); got != 2 {
		t.Fatalf("offer holds the spawn line %d times, want 2:\n%s", got, text)
	}
	if strings.Contains(text, `"Model"`) || strings.Contains(text, `\u0000`) {
		t.Fatalf("offer carries a Model or an unreplaced slot:\n%s", text)
	}
}

// A caller holding the unit's claim token whose agy records do not show it as
// the coordinator's child (no conversation at all, or one that only looks
// like the child's) is served: the records admit nothing, the token does.
func TestAgyRunServesACallerItsRecordsDoNotShow(t *testing.T) {
	type refusal struct {
		name  string
		setup func(tr *agytest.Tree, in wf.Instruction) string // the child id to claim as
	}
	launch := func(in wf.Instruction) agytest.Launch {
		return agytest.Launch{Coordinator: agyCoordinator, Child: agyChildID(1), Brief: in.Brief, At: time.Now().UTC()}
	}
	for _, tc := range []refusal{
		{"unknown caller", func(tr *agytest.Tree, in wf.Instruction) string { return "99999999-0000-4000-8000-000000000000" }},
		{"sibling with the same brief", func(tr *agytest.Tree, in wf.Instruction) string {
			return tr.StartSpoofed(launch(in), agytest.SiblingSameBrief)
		}},
		{"task sender", func(tr *agytest.Tree, in wf.Instruction) string {
			return tr.StartSpoofed(launch(in), agytest.TaskSender)
		}},
		{"auto mode view", func(tr *agytest.Tree, in wf.Instruction) string {
			return tr.StartSpoofed(launch(in), agytest.AutoMode)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := agyEnv(t)
			def := fanDef("agy-spoof", chItems(1), 1, wf.AllSettled())
			state := agyRun(t, def)
			in := agentsOf(agyNextJSON(t, def, state, 1))[0]
			who := tc.setup(tr, in)
			agyAs(t, who)
			code, out, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state)
			if code != 0 {
				t.Fatalf("%s: wf prompt exit %d, want served: %s%s", tc.name, code, out, er)
			}
			if got := agyJournal(t, state, wf.JournalPromptFetched); len(got) != 1 {
				t.Fatalf("%s: %d prompt_fetched records, want the unit bound once", tc.name, len(got))
			}
			for _, r := range agyJournal(t, state, wf.JournalDegraded) {
				if strings.Contains(string(r.Data), `"Kind":"child refused"`) {
					t.Fatalf("%s: the journal holds a refusal: %s", tc.name, r.Data)
				}
			}
		})
	}
}

// lightDef is one agent on the light tier.
func lightDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	a.Dispatch(wf.ModelTier(wf.TierLight))
	return d
}

func TestAgyModelMapAndResume(t *testing.T) {
	t.Run("model map", func(t *testing.T) {
		agyEnv(t)
		def := lightDef("agy-light")
		state := agyRun(t, def, "--model-map", "light=flash")
		if text := mustSingle(t, def, "wf", "next", "1", "--state-dir", state); !strings.Contains(text, `"Role":"wf-unit","Prompt":<the text between the lines>,"Model":"flash"}`) {
			t.Fatalf("a light unit under light=flash has no Model flash:\n%s", text)
		}
		def2 := lightDef("agy-light")
		state2 := agyRun(t, def2)
		if text := mustSingle(t, def2, "wf", "next", "1", "--state-dir", state2); strings.Contains(text, `"Model"`) || !strings.Contains(text, "invoke_subagent") {
			t.Fatalf("a light unit with no map names a Model:\n%s", text)
		}
		code, out, er := singleCLI(lightDef("agy-light"), "run", "--state-dir", filepath.Join(t.TempDir(), "s"), "--workspace", t.TempDir(), "--run-id", "r1", "--model-map", "light=gemini-3.8-flash-high")
		if code == 0 || !strings.Contains(out+er, "light=gemini-3.8-flash-high") || !strings.Contains(out+er, "inherit, flash_lite, flash, pro") {
			t.Fatalf("a model id was not refused naming the four values: %d %s%s", code, out, er)
		}
	})
	t.Run("resume line", func(t *testing.T) {
		tr := agyEnv(t)
		def := fanDef("agy-resume", chItems(1), 1, wf.AllSettled())
		state := agyRun(t, def)
		in := agentsOf(agyNextJSON(t, def, state, 1))[0]
		child := agyChildID(1)
		tr.Start(agytest.Launch{Coordinator: agyCoordinator, Child: child, Brief: in.Brief, At: time.Now().UTC()})
		agyAs(t, child)
		mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state)
		// The child ends its turn with no done: the engine offers a resume.
		tr.Respond(child, time.Now().UTC(), "gemini-3.8-flash-low", 500, 40, "finished")
		agyAs(t, agyCoordinator)
		_, out, er := singleCLI(def, "wf", "next", "1", "--state-dir", state)
		if want := `send_message {"Recipient":"` + child + `","Message":"`; !strings.Contains(out+er, want) {
			t.Fatalf("the recovery offer holds no %q:\n%s%s", want, out, er)
		}
	})
}

func TestAgyBlockingNextWaitsWithoutToolLimit(t *testing.T) {
	tr := agyEnv(t)
	if got := hn.For("agy").ToolLimit(os.Getenv); got != 0 {
		t.Fatalf("agy ToolLimit = %v, want 0 (no cut-off)", got)
	}
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("agy-wait", chItems(1), 1, wf.AllSettled())
	state := agyRun(t, def)
	in := agentsOf(agyNextJSON(t, def, state, 1))[0]
	child := agyChildID(1)
	tr.Start(agytest.Launch{Coordinator: agyCoordinator, Child: child, Brief: in.Brief, At: time.Now().UTC()})
	agyAs(t, child)
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state)
	tr.Respond(child, time.Now().UTC(), "gemini-3.8-flash-low", 100, 10, "", agytest.Call{Name: "view_file", Args: map[string]any{"AbsolutePath": in.Output}})
	agyAs(t, agyCoordinator)
	start := time.Now()
	code, out, er := singleCLI(def, "wf", "next", "1", "--state-dir", state)
	took := time.Since(start)
	if code != 0 {
		t.Fatalf("a blocking next on a running unit exited %d: %s%s", code, out, er)
	}
	// The event asserted is the engine's own timeout report: the blocking next
	// returned on its configured timer ("nothing yet after 2s"), not on a hand-out,
	// a stall or an end (an immediate return) and not on a tool limit (agy has none).
	// Only a lower bound on the elapsed time is kept; an upper bound would measure
	// the host's load, not the engine.
	if want := "nothing yet after 2s: " + wf.NextNothing; strings.TrimSpace(out) != want {
		t.Fatalf("a blocking next with a child running did not report the configured timeout %q:\n%s%s", want, out, er)
	}
	if took < 1900*time.Millisecond {
		t.Fatalf("next returned after %v with a child running: agy's ToolLimit 0 means it waits (capped at 2s here)", took)
	}
}

func TestAgyFixturesScanClean(t *testing.T) {
	out, err := exec.Command("bash", filepath.Join("..", "scripts", "agy-scan-secrets.sh"), filepath.Join("..", "harness", "testdata", "agy")).CombinedOutput()
	if err != nil {
		t.Fatalf("agy-scan-secrets.sh over harness/testdata/agy: %v\n%s", err, out)
	}
}

// A coordinator may launch several children in one response (the fork
// reference allows it) and agy writes their results one at a time, about 11 s
// per launch: the first child must verify while a sibling's result is still
// pending, and the sibling once its result is written.
func TestAgyFirstChildVerifiesWhileASiblingLaunchIsPending(t *testing.T) {
	tr := agyEnv(t)
	def := fanDef("agy-pending", chItems(1, 2), 2, wf.AllSettled())
	state := agyRun(t, def)
	ins := agentsOf(agyNextJSON(t, def, state, 2))
	if len(ins) != 2 {
		t.Fatalf("hand-out = %d units, want 2", len(ins))
	}
	now := time.Now().UTC()
	la := agytest.Launch{Coordinator: agyCoordinator, Child: agyChildID(1), Brief: ins[0].Brief, At: now}
	lb := agytest.Launch{Coordinator: agyCoordinator, Child: agyChildID(2), Brief: ins[1].Brief, At: now}
	tr.StartPartial(agyCoordinator, now, 1, la, lb)

	agyAs(t, la.Child)
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, ins[0]), "--json", "--state-dir", state)
	if got := agyJournal(t, state, wf.JournalPromptFetched); len(got) != 1 {
		t.Fatalf("prompt_fetched records = %d after the first child, want 1", len(got))
	}

	tr.LaunchResult(agyCoordinator, now, lb.Child)
	agyAs(t, lb.Child)
	mustSingle(t, def, "wf", "prompt", "--claim", claimToken(t, ins[1]), "--json", "--state-dir", state)
	if got := agyJournal(t, state, wf.JournalPromptFetched); len(got) != 2 {
		t.Fatalf("prompt_fetched records = %d after both children, want 2", len(got))
	}
}

// An agy child is refused the coordinator's verbs through the CLI: its
// environment names a conversation that is a verified child of the run's
// coordinator, so childCaller refuses `next` and `run` (an existing run id and
// a fresh one) with the child refusal and writes nothing to the journal. Its
// own verbs still work (cluster 7).
func TestAgyChildCannotRunOrDriveTheRun(t *testing.T) {
	tr := agyEnv(t)
	def := fanDef("agy-child-verbs", chItems(1), 1, wf.AllSettled())
	state := agyRun(t, def)
	in := agentsOf(agyNextJSON(t, def, state, 1))[0]
	tr.Start(agytest.Launch{Coordinator: agyCoordinator, Child: agyChildID(1), Brief: in.Brief, At: time.Now().UTC()})
	agyAs(t, agyChildID(1))

	journal := filepath.Join(state, "runs", "r1", "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	for _, args := range [][]string{
		{"wf", "next", "1"},
		{"next"},
		{"run", "--run-id", "r1", "--workspace", ws},
		{"run", "--run-id", "fresh", "--workspace", ws},
	} {
		code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
		if code != 4 || !strings.Contains(er, "you are a child of this run") {
			t.Errorf("%v from an agy child: exit %d, %q", args, code, er)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused verb wrote to the journal:\n%s", after[len(before):])
	}
	if _, err := os.Stat(filepath.Join(state, "runs", "fresh")); err == nil {
		t.Error("a child's `run --run-id fresh` created a run")
	}
	if code, out, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state); code != 0 {
		t.Errorf("the child's own wf prompt: exit %d, %s%s", code, out, er)
	}
}

// An agy caller's printed commands are single engine commands: no workspace
// anchor, no '&&', and each starts with the engine path (the claude contrast
// is TestCLIPrintsCommandsForTheCallingHarness).
func TestAgyCommandsAreUnchained(t *testing.T) {
	agyEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("agy-unchained", chItems(1), 1, wf.AllSettled())
	state := agyRun(t, def)
	out := mustSingle(t, def, "wf", "next", "1", "--state-dir", state)
	brief := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, " wf prompt ") {
			brief = strings.TrimSpace(l)
		}
	}
	if !strings.HasPrefix(brief, "/") || strings.Contains(out, "&&") {
		t.Errorf("agy child brief is chained or does not start with the engine path:\n%s", out)
	}
	// The printed next line of the bootstrap.
	boot := wf.Bootstrap(wf.RunStatus{Run: "r1", Harness: "agy", Detach: wf.DetachNone, Workspace: "/example/user/project"}, "/bin/super", "/state")
	next := ""
	for _, l := range strings.Split(boot, "\n") {
		if strings.Contains(l, " wf next") {
			next = strings.TrimSpace(l)
		}
	}
	if !strings.HasPrefix(next, "/bin/super") && !strings.HasPrefix(next, "'/bin/super") || strings.Contains(boot, "&&") {
		t.Errorf("agy bootstrap next line is chained or does not start with the engine path (%q):\n%s", next, boot)
	}
	claude := wf.Bootstrap(wf.RunStatus{Run: "r1", Harness: "claude", Detach: wf.DetachNone, Workspace: "/example/user/project"}, "/bin/super", "/state")
	if !strings.Contains(claude, "cd /example/user/project && ") {
		t.Errorf("claude bootstrap lost the workspace anchor:\n%s", claude)
	}
}
