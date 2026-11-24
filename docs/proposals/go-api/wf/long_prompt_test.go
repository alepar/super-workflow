package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// sizedAgentDef is one agent whose prompt is text, then a terminal.
func sizedAgentDef(name, text string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText(text), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	return d
}

func promptOf(t *testing.T, def *wf.Def) (string, wf.Instruction) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	return mustSingle(t, def, "wf", "prompt", ins[0].Name, "--state-dir", state), ins[0]
}

var bodyLine = regexp.MustCompile(`(?m)^  (/.+/prompt\.md)$`)

// bodyOf reads the instruction body the page names, checking the page itself
// is one short page that carries the finishing rules.
func bodyOf(t *testing.T, page string) (string, string) {
	t.Helper()
	if len(page) > 2048 {
		t.Errorf("wf prompt printed %d bytes, over 2 KB:\n%s", len(page), page)
	}
	for _, want := range []string{"You are subagent ", "validate in place of done", "wf done ", "failed in place of done", "--reason <text>", "--class blocked", "file reader"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	m := bodyLine.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page names no body file:\n%s", page)
	}
	raw, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), m[1]
}

// E1: a prompt of any size prints one page under 2 KB with the finishing
// rules; the whole text is the body file, whose last line repeats the
// finishing commands.
func TestPromptIsOnePageAndABodyFile(t *testing.T) {
	for _, text := range []string{"Say hello.", strings.Repeat("Read the inputs carefully.\n", 2000), strings.Repeat("x", 40000), strings.Repeat("a\n", 300)} {
		page, ins := promptOf(t, sizedAgentDef("sized", text))
		body, _ := bodyOf(t, page)
		if !strings.Contains(body, strings.TrimRight(text, "\n")) || !strings.Contains(body, "Write your result as JSON to this allocated output file: "+ins.Output) {
			t.Errorf("the body lacks the prompt text or its submission block")
		}
		// The body ends with the finishing commands again, one per line.
		_, last, ok := strings.Cut(body, "\nFinish: when your result is written, check it until it says ok:\n")
		if !ok || strings.Contains(last, "\nFinish: ") || !strings.Contains(last, "wf validate ") || !strings.Contains(last, "wf done ") || !strings.Contains(last, "wf failed ") {
			t.Errorf("the body does not end with the finishing commands: %q", last)
		}
		if !strings.Contains(page, ins.Output) {
			t.Errorf("the page does not name the allocated output %s", ins.Output)
		}
	}
}

// The real brief path: a claimed fetch delivers the page, a re-fetch names the
// same body, and done still reads the unit's allocated output.
func TestPromptClaimedFetchThenDone(t *testing.T) {
	def := sizedAgentDef("long-claimed", strings.Repeat("Read the inputs carefully.\n", 2000))
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	claim := regexp.MustCompile(`--claim ([0-9a-f]+)`).FindStringSubmatch(ins[0].Brief)
	if claim == nil {
		t.Fatalf("brief carries no claim token:\n%s", ins[0].Brief)
	}
	first := mustSingle(t, def, "wf", "prompt", ins[0].Name, "--claim", claim[1], "--state-dir", state)
	_, path := bodyOf(t, first)
	if again := mustSingle(t, def, "wf", "prompt", ins[0].Name, "--claim", claim[1], "--state-dir", state); again != first {
		t.Errorf("a re-fetch named another body")
	}
	// A body deleted after delivery is restored by the next fetch.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mustSingle(t, def, "wf", "prompt", ins[0].Name, "--claim", claim[1], "--state-dir", state)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the deleted body was not restored: %v", err)
	}
	if err := os.WriteFile(ins[0].Output, []byte(`{"OK":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSingle(t, def, "wf", "done", ins[0].Name, "--state-dir", state)
	if j := mustSingle(t, def, "wf", "journal", "--state-dir", state); !strings.Contains(j, `"PromptDir"`) {
		t.Errorf("prompt_fetched does not record the body's directory")
	}
}

// With no run dir there is nowhere to write a body: the prompt is inline.
func TestPromptWithoutRunDirStaysInline(t *testing.T) {
	body := strings.Repeat("x", 20000)
	d := sizedAgentDef("long-norundir", body)
	e := wf.NewEngine(wf.MemJournal(), nil, wf.EngineOptions{Defs: []*wf.Def{d}})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := e.HandOut(run, 1)
	if err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	text, err := e.Prompt(run, ins[0].Name)
	if err != nil || !strings.Contains(text, body) {
		t.Errorf("prompt without a run dir is not inline (%v)", err)
	}
}

// wf prompt --json gives a machine reader the full rendered prompt and every
// input value, whether or not a token consumed it and however the child is
// shown the prompt.
func TestPromptJSONCarriesFullPromptAndInputs(t *testing.T) {
	d := wf.New("prompt-json")
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText(strings.Repeat("Judge carefully.\n", 600)+"Work in [WT].", wf.Bound("WT", "/wt/{run}")),
		wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, d, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, d, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	var f wf.PromptFetch
	if err := json.Unmarshal([]byte(mustSingle(t, d, "wf", "prompt", ins[0].Name, "--json", "--state-dir", state)), &f); err != nil {
		t.Fatal(err)
	}
	if f.Inputs["WT"] != "/wt/r1" || !strings.Contains(f.Prompt, "Work in /wt/r1.") || !strings.Contains(f.Prompt, "Write your result as JSON") {
		t.Errorf("fetch inputs %v, prompt has the bound value %v", f.Inputs, strings.Contains(f.Prompt, "/wt/r1"))
	}
	if !bodyLine.MatchString(f.Delivered) || len(f.Delivered) > 2048 {
		t.Errorf("the prompt was not delivered as a page and a body:\n%s", f.Delivered)
	}
}

// withBody is a fetched page followed by the body file it names: what a child
// reads in all (E1). A page naming no body is returned as it is.
func withBody(t testing.TB, page string) string {
	t.Helper()
	m := bodyLine.FindStringSubmatch(page)
	if m == nil {
		return page
	}
	raw, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return page + "\n" + string(raw)
}

// Through the CLI, done trusts the hand-back whatever the child's transcript
// shows: a Claude child whose resolved transcript shows no read of its body,
// one that fetched the whole prompt (--json), one whose transcript the engine
// cannot resolve, and one whose body was deleted after delivery are all
// accepted, and none is journaled as refused.
func TestDoneNeedsNoTranscriptReadThroughTheCLI(t *testing.T) {
	for _, c := range []string{"unread", "json", "unresolved", "deleted"} {
		t.Run(c, func(t *testing.T) {
			e := newCLIEnv(t)
			root := t.TempDir()
			env := []string{"CLAUDE_CODE_SESSION_ID=sess", "CLAUDE_CONFIG_DIR=" + root, "CODEX_THREAD_ID=", "CODEX_SESSION_ID=", "HERMES_SESSION_ID="}
			call := func(args ...string) (int, string, string) { return e.proc(e.ws, env, args...) }
			run := e.start("agent-cli")
			in := e.nextBy(call, run, "1")[0]
			fetch := []string{"prompt", string(run), "--claim", claimToken(t, in), "--state-dir", e.state}
			if c == "json" {
				fetch = append(fetch, "--json")
			}
			sub := filepath.Join(root, "projects", "-repo", "sess", "subagents")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(root, "projects", "-repo", "sess.jsonl"), []byte("{}\n"), 0o644)
			// unresolved: no subagent of the session started with the token.
			// The claim token admits the fetch and the done all the same.
			kick := `{"type":"user","message":{"role":"user","content":"Run: wf prompt --claim ` + claimToken(t, in) + `"}}` + "\n"
			if c != "unresolved" {
				if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), []byte(kick), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, page, er := call(fetch...)
			if code != 0 {
				t.Fatalf("prompt exited %d: %s", code, er)
			}
			// The child ran wf prompt through Bash and read nothing else.
			line := `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"wf prompt --claim ` + claimToken(t, in) + `"}}]}}` + "\n"
			if c != "unresolved" {
				if err := os.WriteFile(filepath.Join(sub, "agent-abc.jsonl"), []byte(kick+line), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if c == "deleted" {
				m := bodyLine.FindStringSubmatch(page)
				if m == nil {
					t.Fatalf("no body on the page:\n%s", page)
				}
				if err := os.Remove(m[1]); err != nil {
					t.Fatal(err)
				}
			}
			writeAllocated(t, in.Output, `{"OK":true}`)
			if code, _, er = call("validate", string(run), "--claim", claimToken(t, in), "--state-dir", e.state); code != 0 {
				t.Fatalf("%s: validate = %d %s, want accepted", c, code, er)
			}
			if code, _, er = call("done", string(run), "--claim", claimToken(t, in), "--state-dir", e.state); code != 0 {
				t.Fatalf("%s: done = %d %s, want accepted", c, code, er)
			}
			if after := e.must("journal", string(run), "--state-dir", e.state); strings.Contains(after, "child refused") {
				t.Errorf("%s: want no refusal:\n%s", c, after)
			}
		})
	}
}

// R5 review: the page stays under 2 KB at the cross-review's real paths,
// run through a binary at a 111-character path (the commands carry the
// running binary's own path): state dir 117 characters, workspace 88, run
// examplewf-102030405060, and the unit name's length made up to
// Design/LocalDesign/Decompose#1 wherever the page names it.
func TestPageUnderTwoKBWithDeepPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("d", 40))
	ws := root + strings.Repeat("w", max(0, 88-len(root)))
	bin := filepath.Join(ws, ".superpowers", "bin", strings.Repeat("s", max(5, 111-len(ws)-len("/.superpowers/bin/"))))
	state := filepath.Join(ws, ".superpowers", "state", strings.Repeat("a", max(9, 117-len(ws)-len("/.superpowers/state/"))))
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"WF_TEST_NAMED=1"}
	call := func(args ...string) string {
		t.Helper()
		code, out, er := runProc(t, bin, ws, env, args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, er)
		}
		return out
	}
	const run = "examplewf-102030405060"
	call("run", "agent-cli", "--state-dir", state, "--workspace", ws, "--run-id", run)
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(call("wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	page := call("wf", "prompt", ins[0].Name, "--state-dir", state)
	const realName = "Design/LocalDesign/Decompose#1"
	size := len(page) + strings.Count(page, ins[0].Name)*(len(realName)-len(ins[0].Name))
	if size > 2048 || !strings.Contains(page, bin) {
		t.Fatalf("the page would be %d bytes with a %d-character binary, a %d-character state dir and a %d-character workspace, over 2 KB:\n%s", size, len(bin), len(state), len(ws), page)
	}
	if !strings.Contains(page, "this page wins") || !strings.Contains(page, "failed in place of done") {
		t.Errorf("the page lost a rule:\n%s", page)
	}
}
