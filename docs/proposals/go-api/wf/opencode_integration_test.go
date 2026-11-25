package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hn "github.com/alepar/super-workflow/docs/proposals/go-api/harness"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// The opencode integration sweep: a wf run coordinated by opencode end to
// end, with no records anywhere. The plugin's variables are the only
// identity; the claim token admits a child and its verbs settle its unit.

const opencodeRoot = "ses_root0000000000000000001"

// opencodeChildID is the n-th child's session id.
func opencodeChildID(n int) string { return fmt.Sprintf("ses_child000000000000000%03d", n) }

// opencodeEnv sets the plugin's environment of a root opencode session, with
// every other harness's variables cleared and HOME a fresh directory.
func opencodeEnv(t *testing.T) {
	t.Helper()
	strictCallers(t)
	for _, k := range []string{"HERMES_SESSION_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCODE", "1")
	opencodeAs(t, opencodeRoot, "")
}

// opencodeAs is the environment the plugin gives session id under parent
// ("" for a root).
func opencodeAs(t *testing.T, id, parent string) {
	t.Setenv("OPENCODE_SESSION_ID", id)
	t.Setenv("OPENCODE_PARENT_SESSION_ID", parent)
}

func opencodeRun(t *testing.T, def *wf.Def, extra ...string) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, append([]string{"run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1"}, extra...)...)
	return state
}

func opencodeNextJSON(t *testing.T, def *wf.Def, state string, n int) []wf.Instruction {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", fmt.Sprint(n), "--json", "--state-dir", state)), &ins); err != nil {
		t.Fatal(err)
	}
	return ins
}

func opencodeJournal(t *testing.T, state string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "runs", "r1", "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Two units handed out, each claimed by its child session and settled by the
// child's own done: no records exist and none are needed.
func TestOpencodeRunEndToEnd(t *testing.T) {
	opencodeEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	if got := hn.Detect(os.Getenv); got != "opencode" {
		t.Fatalf("Detect = %q, want opencode", got)
	}
	def := fanDef("opencode-e2e", chItems(1, 2), 2, wf.AllSettled())
	state := opencodeRun(t, def)
	if j := opencodeJournal(t, state); !strings.Contains(j, `"Harness":"opencode"`) {
		t.Fatalf("run_started does not record opencode:\n%s", j)
	}
	ins := agentsOf(opencodeNextJSON(t, def, state, 2))
	if len(ins) != 2 {
		t.Fatalf("hand-out = %d units, want 2", len(ins))
	}
	for i, in := range ins {
		opencodeAs(t, opencodeChildID(i+1), opencodeRoot)
		tok := claimToken(t, in)
		mustSingle(t, def, "wf", "prompt", "--claim", tok, "--json", "--state-dir", state)
		writeAllocated(t, in.Output, `{"OK":true}`)
		mustSingle(t, def, "wf", "done", "--claim", tok, "--state-dir", state)
	}
	opencodeAs(t, opencodeRoot, "")
	code, out, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state)
	if code != exitEndedCode {
		t.Fatalf("next after both units: exit %d, want %d: %s%s", code, exitEndedCode, out, er)
	}
}

// The offer is opencode's task call on the general agent, with no model.
func TestOpencodeOfferIsTheTaskCall(t *testing.T) {
	opencodeEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("opencode-offer", chItems(1, 2), 2, wf.AllSettled())
	state := opencodeRun(t, def)
	text := mustSingle(t, def, "wf", "next", "2", "--state-dir", state)
	if got := strings.Count(text, `task(description="run unit", subagent_type="general", prompt=`); got != 2 {
		t.Fatalf("offer holds the task call %d times, want 2:\n%s", got, text)
	}
	if strings.Contains(text, "model=") {
		t.Fatalf("offer passes a model:\n%s", text)
	}
}

// A child session never drives the run; its own verbs still work.
func TestOpencodeChildCannotDriveTheRun(t *testing.T) {
	opencodeEnv(t)
	def := fanDef("opencode-child", chItems(1), 1, wf.AllSettled())
	state := opencodeRun(t, def)
	in := agentsOf(opencodeNextJSON(t, def, state, 1))[0]
	opencodeAs(t, opencodeChildID(1), opencodeRoot)
	before := opencodeJournal(t, state)
	for _, args := range [][]string{{"wf", "next", "1"}, {"run", "--run-id", "fresh", "--workspace", t.TempDir()}} {
		code, _, er := singleCLI(def, append(args, "--state-dir", state)...)
		if code != 4 || !strings.Contains(er, "you are a child of this run") {
			t.Errorf("%v from an opencode child: exit %d, %q", args, code, er)
		}
	}
	if opencodeJournal(t, state) != before {
		t.Error("a refused verb wrote to the journal")
	}
	if code, out, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state); code != 0 {
		t.Errorf("the child's own prompt: exit %d, %s%s", code, out, er)
	}
}

// Without the plugin a shell has OPENCODE alone: a driving verb on a run an
// opencode session drives is refused, naming the missing session variable.
func TestOpencodeShellWithoutPluginIsRefused(t *testing.T) {
	opencodeEnv(t)
	def := fanDef("opencode-noplugin", chItems(1), 1, wf.AllSettled())
	state := opencodeRun(t, def)
	opencodeNextJSON(t, def, state, 1)
	opencodeAs(t, "", "")
	code, out, er := singleCLI(def, "wf", "next", "1", "--state-dir", state)
	if code == 0 || !strings.Contains(out+er, "OPENCODE_SESSION_ID") {
		t.Fatalf("a plugin-less opencode shell drove the run: exit %d %s%s", code, out, er)
	}
	if strings.Contains(out+er, opencodeRoot) {
		t.Fatalf("the refusal prints a session id: %s%s", out, er)
	}
}

// One opencode session per run: a second root session's next takes the run
// over, and the first session's claimed child is retired with it (its
// children ran inside the first session's process).
func TestOpencodeSecondSessionTakesOver(t *testing.T) {
	opencodeEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("opencode-second", chItems(1), 1, wf.AllSettled())
	state := opencodeRun(t, def)
	in := agentsOf(opencodeNextJSON(t, def, state, 1))[0]
	opencodeAs(t, opencodeChildID(1), opencodeRoot)
	tok := claimToken(t, in)
	mustSingle(t, def, "wf", "prompt", "--claim", tok, "--json", "--state-dir", state)

	const second = "ses_root0000000000000000002"
	opencodeAs(t, second, "")
	re := agentsOf(opencodeNextJSON(t, def, state, 1))
	if len(re) != 1 || re[0].Attempt != 2 {
		t.Fatalf("the second session's next = %+v, want the unit handed out again at attempt 2", re)
	}
	opencodeAs(t, opencodeChildID(1), opencodeRoot)
	writeAllocated(t, in.Output, `{"OK":true}`)
	if code, _, _ := singleCLI(def, "wf", "done", "--claim", tok, "--state-dir", state); code == 0 {
		t.Fatal("the first session's retired child settled its unit")
	}
}

// The task tool takes no model: a tiered unit runs on the coordinator's
// model, and a --model-map, given or inherited, is not applied; the run says
// so once, and never points at a map the harness would ignore.
func TestOpencodeModels(t *testing.T) {
	opencodeEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"no map", nil, "tier(s) light run on the coordinator's model"},
		{"explicit map", []string{"--model-map", "light=openrouter/deepseek/deepseek-v4-flash"}, "the --model-map is not applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := lightDef("opencode-light")
			state := opencodeRun(t, def, tc.extra...)
			if text := mustSingle(t, def, "wf", "next", "1", "--state-dir", state); strings.Contains(text, "model=") || !strings.Contains(text, "task(") {
				t.Fatalf("a light unit names a model:\n%s", text)
			}
			j := opencodeJournal(t, state)
			if strings.Count(j, "opencode's task tool takes no model") != 1 || !strings.Contains(j, tc.want) || strings.Contains(j, "names one") {
				t.Fatalf("want one no-model note saying %q:\n%s", tc.want, j)
			}
		})
	}
}

// A successor run inherits its predecessor's map; under opencode it is not
// applied rather than refused, so the successor starts.
func TestOpencodeSuccessorInheritsMapWithoutRefusal(t *testing.T) {
	opencodeEnv(t)
	t.Setenv(wf.NextWaitCapEnv, "2s")
	def := fanDef("opencode-succ", chItems(1), 1, wf.AllSettled())
	state, ws := filepath.Join(t.TempDir(), "state"), t.TempDir()
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1", "--model-map", "all=some-model")
	in := agentsOf(opencodeNextJSON(t, def, state, 1))[0]
	opencodeAs(t, opencodeChildID(1), opencodeRoot)
	tok := claimToken(t, in)
	mustSingle(t, def, "wf", "prompt", "--claim", tok, "--json", "--state-dir", state)
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", "--claim", tok, "--state-dir", state)
	opencodeAs(t, opencodeRoot, "")
	if code, out, er := singleCLI(def, "wf", "next", "1", "--json", "--state-dir", state); code != exitEndedCode {
		t.Fatalf("the first run did not end: %d %s%s", code, out, er)
	}
	if code, out, er := singleCLI(def, "run", "--state-dir", state, "--workspace", ws, "--run-id", "r1"); code != 0 {
		t.Fatalf("the successor run was refused: %d %s%s", code, out, er)
	}
}

// A child whose plugin could not look up its parent has no session: its
// next is refused, never taken for a newer coordinator.
func TestOpencodeUnknownParentNeverDrives(t *testing.T) {
	opencodeEnv(t)
	def := fanDef("opencode-unknown", chItems(1), 1, wf.AllSettled())
	state := opencodeRun(t, def)
	in := agentsOf(opencodeNextJSON(t, def, state, 1))[0]
	opencodeAs(t, opencodeChildID(1), "unknown")
	before := opencodeJournal(t, state)
	if code, out, er := singleCLI(def, "wf", "next", "1", "--state-dir", state); code == 0 {
		t.Fatalf("an unknown-parent caller drove the run: %s%s", out, er)
	}
	if opencodeJournal(t, state) != before {
		t.Fatal("the refused next wrote to the journal")
	}
	if code, out, er := singleCLI(def, "wf", "prompt", "--claim", claimToken(t, in), "--json", "--state-dir", state); code != 0 {
		t.Fatalf("its own prompt by token: exit %d %s%s", code, out, er)
	}
}

// A caller with an agent and no session (opencode's failed parent lookup)
// never drives, not even the first next of a segment no coordinator has
// bound yet; the refusal says to retry.
func TestOpencodeUnknownParentNeverDrivesAnUnboundSegment(t *testing.T) {
	opencodeEnv(t)
	def := fanDef("opencode-unbound", chItems(1), 1, wf.AllSettled())
	state := opencodeRun(t, def)
	opencodeAs(t, opencodeChildID(1), "unknown")
	before := opencodeJournal(t, state)
	code, out, er := singleCLI(def, "wf", "next", "1", "--state-dir", state)
	if code == 0 || !strings.Contains(out+er, "could not look up this session's parent") {
		t.Fatalf("an unknown-parent caller drove an unbound segment: exit %d %s%s", code, out, er)
	}
	if opencodeJournal(t, state) != before {
		t.Fatal("the refused next wrote to the journal")
	}
}

// A plugin-less opencode inside a Claude tab reads as Claude: the run says
// so once, on stderr and in the journal, and goes on.
func TestPluginlessOpencodeInClaudeTabIsNoted(t *testing.T) {
	opencodeEnv(t)
	opencodeAs(t, "", "")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "claude-tab")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	def := fanDef("opencode-hint", chItems(1), 1, wf.AllSettled())
	state := filepath.Join(t.TempDir(), "state")
	code, out, er := singleCLI(def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	if code != 0 || !strings.Contains(er, "note: OPENCODE is set with no OPENCODE_SESSION_ID") {
		t.Fatalf("run: exit %d %s%s", code, out, er)
	}
	j := opencodeJournal(t, state)
	if strings.Count(j, `"Kind":"harness hint"`) != 1 || !strings.Contains(j, `"Harness":"claude"`) {
		t.Fatalf("want one harness hint on a claude run:\n%s", j)
	}
}
