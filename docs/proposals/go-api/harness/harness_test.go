package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func envOf(kv map[string]string) harness.Env { return func(k string) string { return kv[k] } }

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Three children of one Claude session are each found by their own claim
// token; the coordinator's transcript, which holds every token, is not a match.
func TestClaudeResolvesThreeChildrenByToken(t *testing.T) {
	root := t.TempDir()
	sess := filepath.Join(root, "projects", "-repo", "sess-1")
	write(t, sess+".jsonl", "dispatch tok-a tok-b tok-c")
	for _, c := range []struct{ id, tok string }{{"a1", "tok-a"}, {"b2", "tok-b"}, {"c3", "tok-c"}} {
		write(t, filepath.Join(sess, "subagents", "agent-"+c.id+".jsonl"), `{"cmd":"wf prompt --claim `+c.tok+`"}`)
	}
	// another session's child with the same token text is out of scope
	write(t, filepath.Join(root, "projects", "-repo", "sess-2", "subagents", "agent-zz.jsonl"), "tok-a")
	wants := []harness.Want{{Token: "tok-a"}, {Token: "tok-b"}, {Token: "tok-c"}, {Token: "tok-unflushed"}}
	got := harness.For("claude").Resolve(root, "sess-1", wants)
	if len(got) != 3 {
		t.Fatalf("resolved %d children, want 3: %v", len(got), got)
	}
	for w, id := range map[harness.Want]string{wants[0]: "a1", wants[1]: "b2", wants[2]: "c3"} {
		c := got[w]
		if c.Agent != id || !strings.HasSuffix(c.Transcript, "agent-"+id+".jsonl") {
			t.Fatalf("%s resolved to %+v, want agent %s", w.Token, c, id)
		}
	}
}

// A token found in two child transcripts names no child: the engine never
// guesses between them.
func TestClaudeAmbiguousTokenResolvesNothing(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "projects", "-repo", "s", "subagents")
	write(t, filepath.Join(sub, "agent-a.jsonl"), "tok")
	write(t, filepath.Join(sub, "agent-b.jsonl"), "tok")
	if got := harness.For("claude").Resolve(root, "s", []harness.Want{{Token: "tok"}}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestCodexCallerAndRollout(t *testing.T) {
	root := t.TempDir()
	child := harness.For("codex").Caller(envOf(map[string]string{"CODEX_THREAD_ID": "kid", "CODEX_SESSION_ID": "boss", "CODEX_HOME": root}))
	if child.Session != "boss" || child.Agent != "kid" || child.Is != harness.IsChild || child.Root != root {
		t.Fatalf("child caller = %+v", child)
	}
	self := harness.For("codex").Caller(envOf(map[string]string{"CODEX_THREAD_ID": "boss", "CODEX_SESSION_ID": "boss"}))
	if self.Session != "boss" || self.Agent != "" || self.Is != harness.IsSelf {
		t.Fatalf("coordinator caller = %+v", self)
	}
	// Without CODEX_SESSION_ID nothing tells a child from its coordinator:
	// the verdict is unknown, never self, so no child is refused on a guess.
	if bare := harness.For("codex").Caller(envOf(map[string]string{"CODEX_THREAD_ID": "kid"})); bare.Is != harness.IsUnknown || bare.Session != "kid" {
		t.Fatalf("caller without CODEX_SESSION_ID = %+v", bare)
	}
	rollout := filepath.Join(root, "sessions", "2026", "11", "14", "rollout-2026-11-14T10-00-00-kid.jsonl")
	write(t, rollout, "{}")
	w := harness.Want{Token: "t", Agent: "kid"}
	got := harness.For("codex").Resolve(root, "boss", []harness.Want{w})
	if got[w].Transcript != rollout || got[w].Agent != "kid" {
		t.Fatalf("got %v", got)
	}
}

func TestGenericIdentifiesNothing(t *testing.T) {
	g := harness.For("nonesuch")
	if g.Name() != "nonesuch" || g.Tiers() != nil || g.AgentType() != "" {
		t.Fatalf("generic adapter %+v", g)
	}
	if c := g.Caller(envOf(map[string]string{"HERMES_SESSION_ID": "x"})); c.Session != "" || c.Is != harness.IsUnknown {
		t.Fatalf("caller %+v", c)
	}
	if !strings.Contains(g.SpawnLine(harness.Spawn{Brief: "run: x"}), "run: x") {
		t.Fatal("spawn line drops the brief")
	}
}

func TestDetectAndContradicts(t *testing.T) {
	codex := envOf(map[string]string{"CODEX_THREAD_ID": "x"})
	hermes := envOf(map[string]string{"HERMES_SESSION_ID": "h"})
	bare := envOf(nil)
	if harness.Detect(codex) != "codex" || harness.Detect(bare) != "generic" || harness.Detect(hermes) != "hermes" {
		t.Fatal("detect")
	}
	// An explicit harness must agree with the detected harness.
	if msg := harness.Contradicts("codex", hermes); msg == "" || strings.Contains(msg, "claude") {
		t.Fatalf("--harness codex under Hermes = %q, want a refusal suggesting no other harness", msg)
	}
	if harness.Contradicts("claude", codex) == "" {
		t.Fatal("naming another harness inside Codex is a contradiction")
	}
	for _, ok := range []struct {
		named string
		env   harness.Env
	}{{"generic", hermes}, {"generic", codex}, {"codex", codex}, {"codex", bare}, {"smoke", bare}} {
		if msg := harness.Contradicts(ok.named, ok.env); msg != "" {
			t.Fatalf("%s refused: %s", ok.named, msg)
		}
	}
}

// Usage reads what a child spent from its own transcript: Claude counts each
// assistant message once (its last line wins: output grows while it streams),
// Codex the last cumulative token_count. Cache reads are not counted.
func TestUsageFromTranscripts(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "agent-a1.jsonl")
	write(t, claude, `{"type":"user","message":{"role":"user","content":"x"}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":1}}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":5000,"output_tokens":40}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":6000,"output_tokens":7}}}
not json
`)
	if n, ok := harness.For("claude").(harness.Telemetry).Usage(claude); !ok || n != 10+100+40+3+7 {
		t.Errorf("claude usage = %d, %v; want 160, true", n, ok)
	}
	codex := filepath.Join(dir, "rollout-x.jsonl")
	write(t, codex, `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":5}}}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":3000,"cached_input_tokens":2500,"output_tokens":20,"reasoning_output_tokens":4}}}}
`)
	if n, ok := harness.For("codex").(harness.Telemetry).Usage(codex); !ok || n != 3000-2500+20 {
		t.Errorf("codex usage = %d, %v; want 520, true", n, ok)
	}
	if _, ok := harness.For("codex").(harness.Telemetry).Usage(filepath.Join(dir, "missing.jsonl")); ok {
		t.Error("a missing transcript has usage")
	}
	if _, ok := harness.For("nonesuch").(harness.Telemetry); ok {
		t.Error("the generic harness reads telemetry")
	}
	empty := filepath.Join(dir, "empty.jsonl")
	write(t, empty, `{"type":"user"}`+"\n")
	if _, ok := harness.For("claude").(harness.Telemetry).Usage(empty); ok {
		t.Error("a transcript with no usage has usage")
	}
}

// Claude subagents and Codex threads die with their coordinator; nothing is
// known of a generic child.
func TestChildrenDieWithCoordinator(t *testing.T) {
	for name, want := range map[string]bool{"claude": true, "codex": true, "hermes": false, "nonesuch": false, "generic": false} {
		if got := harness.For(name).ChildrenDieWithCoordinator(); got != want {
			t.Errorf("%s: children die with coordinator = %v, want %v", name, got, want)
		}
	}
}

// A child has ended only on its harness's explicit end marker. These
// fixtures cover Claude's turn-ending hand-back result and Codex's
// task_complete event. A transcript cut
// mid-turn, a Claude end_turn message (it may be waiting on a background
// command), a child resumed after its hand-back, or a Codex thread killed
// mid-turn, has not.
func TestEndedOnlyOnTheEndMarker(t *testing.T) {
	for _, c := range []struct {
		harness, file string
		want          bool
	}{
		{"claude", "claude-handback.jsonl", true},
		{"claude", "claude-end-turn.jsonl", false},
		{"claude", "claude-resumed-after-handback.jsonl", false},
		{"claude", "claude-mid-tool-use.jsonl", false},
		{"claude", "claude-mid-tool-result.jsonl", false},
		{"codex", "codex-task-complete.jsonl", true},
		{"codex", "codex-killed-mid-turn.jsonl", false},
		{"nonesuch", "claude-handback.jsonl", false},
	} {
		if got := harness.For(c.harness).Ended(filepath.Join("testdata", "ended", c.file)); got != c.want {
			t.Errorf("%s %s: ended = %v, want %v", c.harness, c.file, got, c.want)
		}
	}
	if harness.For("claude").Ended(filepath.Join(t.TempDir(), "missing.jsonl")) {
		t.Error("a missing transcript has ended")
	}
	// A turn that resumes after the end marker has not ended.
	dir := t.TempDir()
	resumed := filepath.Join(dir, "rollout.jsonl")
	raw, err := os.ReadFile(filepath.Join("testdata", "ended", "codex-task-complete.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, resumed, string(raw)+`{"type":"event_msg","payload":{"type":"task_started"}}`+"\n")
	if harness.For("codex").Ended(resumed) {
		t.Error("a Codex thread that started a new task after task_complete has ended")
	}
}

// E3: Codex's own subagent limit is max_threads under [agents], read from
// the nearest .codex/config.toml above the working directory, else
// $CODEX_HOME/config.toml; no other harness has one the adapter can read.
func TestCodexLimitFromConfig(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	env := envOf(map[string]string{"CODEX_HOME": home})
	if n, _ := harness.For("codex").Limit(env, repo); n != 0 {
		t.Errorf("no config: limit %d, want 0 (unknown)", n)
	}
	write(t, filepath.Join(home, "config.toml"), "model = \"x\"\n\n[agents]\n# the cap\nmax_threads = 20\n")
	if n, src := harness.For("codex").Limit(env, repo); n != 20 || !strings.Contains(src, filepath.Join(home, "config.toml")) {
		t.Errorf("home config: limit %d from %q, want 20 from it", n, src)
	}
	write(t, filepath.Join(repo, ".codex", "config.toml"), "[agents]\nmax_threads = 6 # repo cap\n[other]\nmax_threads = 99\n")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if n, src := harness.For("codex").Limit(env, sub); n != 6 || !strings.Contains(src, filepath.Join(repo, ".codex", "config.toml")) {
		t.Errorf("repo config: limit %d from %q, want 6 from it", n, src)
	}
	write(t, filepath.Join(repo, ".codex", "config.toml"), "agents.max_threads = 4\n")
	if n, _ := harness.For("codex").Limit(env, repo); n != 4 {
		t.Errorf("dotted key: limit %d, want 4", n)
	}
	if n, _ := harness.For("claude").Limit(env, repo); n != 0 {
		t.Errorf("claude limit %d, want 0", n)
	}
}

// A coordinator's own transcript is found by its session.
func TestCoordinatorTranscript(t *testing.T) {
	root := t.TempDir()
	cl := filepath.Join(root, "projects", "-repo", "sess.jsonl")
	write(t, cl, "{}\n")
	write(t, filepath.Join(root, "projects", "-repo", "sess", "subagents", "agent-a.jsonl"), "{}\n")
	if got := harness.For("claude").(harness.Telemetry).CoordinatorTranscript(root, "sess"); got != cl {
		t.Errorf("claude coordinator transcript = %q, want %q", got, cl)
	}
	cx := filepath.Join(root, "sessions", "2026", "11", "14", "rollout-2026-11-14T20-00-00-thr.jsonl")
	write(t, cx, "{}\n")
	if got := harness.For("codex").(harness.Telemetry).CoordinatorTranscript(root, "thr"); got != cx {
		t.Errorf("codex coordinator transcript = %q, want %q", got, cx)
	}
	if got := harness.For("claude").(harness.Telemetry).CoordinatorTranscript(root, "other"); got != "" {
		t.Errorf("an unknown session has transcript %q", got)
	}
}

// A child can read its prompt with Claude's Read tool or a shell command,
// or with Codex's exec tool. The fixtures include calls to read another file.
func TestReadOfTheBody(t *testing.T) {
	const body = "/state/runs/r1/prompt/0123456789abcdef/prompt.md"
	for _, c := range []struct {
		harness, file string
		read, known   bool
	}{
		{"claude", "claude-read-tool.jsonl", true, true},
		{"claude", "claude-bash-cat.jsonl", true, true},
		{"claude", "claude-read-other.jsonl", false, true},
		{"codex", "codex-exec-cat.jsonl", true, true},
		{"codex", "codex-exec-other.jsonl", false, true},
		{"nonesuch", "claude-read-tool.jsonl", false, false},
	} {
		read, known := harness.For(c.harness).ReadAny(filepath.Join("testdata", "read", c.file), []string{"/elsewhere/prompt.md", body})
		if read != c.read || known != c.known {
			t.Errorf("%s %s: read %v known %v, want %v %v", c.harness, c.file, read, known, c.read, c.known)
		}
	}
	if read, known := harness.For("claude").ReadAny(filepath.Join(t.TempDir(), "missing.jsonl"), []string{body}); read || known {
		t.Error("a missing transcript shows a read")
	}
	// A read relative to the body's directory names its unique directory
	// and file name; a transcript with no tool call recognised is unknown,
	// never unread.
	dir := t.TempDir()
	rel := filepath.Join(dir, "rel.jsonl")
	write(t, rel, `{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"text(await tools.exec_command({cmd:\"sed -n 1,200p prompt.md\",workdir:\"/state/runs/r1/prompt/0123456789abcdef\"}));"}}`+"\n")
	ctx := filepath.Join(dir, "ctx.jsonl")
	write(t, ctx, `{"type":"turn_context","payload":{"cwd":"/state/runs/r1/prompt/0123456789abcdef"}}`+"\n"+`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"cat prompt.md\"}"}}`+"\n")
	if read, known := harness.For("codex").ReadAny(ctx, []string{body}); !read || !known {
		t.Errorf("a Codex exec with no workdir runs in the turn's cwd: read %v known %v", read, known)
	}
	other := filepath.Join(dir, "other.jsonl")
	write(t, other, `{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"text(await tools.exec_command({cmd:\"cat other-prompt.md\",workdir:\"/state/runs/r1/prompt/0123456789abcdef\"}));"}}`+"\n")
	if read, _ := harness.For("codex").ReadAny(other, []string{body}); read {
		t.Error("another file in the body's directory was taken for the body")
	}
	if read, known := harness.For("codex").ReadAny(rel, []string{body}); !read || !known {
		t.Errorf("a read in the body's directory: read %v known %v, want both", read, known)
	}
	// cross-review: a relative read resolves against the record's own cwd
	// (Claude) or the exec's workdir (Codex), which the command does not
	// name. A path that only shares the file name is not the body.
	const bodyDir = "/state/runs/r1/prompt/0123456789abcdef"
	for _, c := range []struct {
		harness, line string
		read          bool
	}{
		{"claude", `{"type":"assistant","cwd":"` + bodyDir + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cat prompt.md"}}]}}`, true},
		{"claude", `{"type":"assistant","cwd":"/state/runs/r1","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"sed -n 1,200p prompt/0123456789abcdef/prompt.md"}}]}}`, true},
		{"claude", `{"type":"assistant","cwd":"/repo","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cat prompt.md"}}]}}`, false},
		{"claude", `{"type":"assistant","cwd":"` + bodyDir + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cat other-prompt.md"}}]}}`, false},
		{"codex", `{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"cat prompt.md\",\"workdir\":\"` + bodyDir + `\"}"}}`, true},
		{"codex", `{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"cat prompt.md\",\"workdir\":\"/repo\"}"}}`, false},
		{"claude", `{"type":"assistant","cwd":"/state/runs/r1","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cd prompt/0123456789abcdef && cat prompt.md"}}]}}`, true},
		{"claude", `{"type":"assistant","cwd":"/repo","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"pushd \"` + bodyDir + `\" && sed -n 1,200p prompt.md"}}]}}`, true},
		{"claude", `{"type":"assistant","cwd":"` + bodyDir + `","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"cat ../prompt.md"}}]}}`, false},
		{"codex", `{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"cat prompt.md\\nwc -l prompt.md\",\"workdir\":\"` + bodyDir + `\"}"}}`, true},
	} {
		p := filepath.Join(dir, "cwd.jsonl")
		write(t, p, c.line+"\n")
		if read, known := harness.For(c.harness).ReadAny(p, []string{body}); read != c.read || !known {
			t.Errorf("%s %s: read %v known %v, want read %v", c.harness, c.line, read, known, c.read)
		}
	}
	bare := filepath.Join(dir, "bare.jsonl")
	write(t, bare, `{"type":"user","message":{"content":"x"}}`+"\n")
	for _, h := range []string{"claude", "codex"} {
		if read, known := harness.For(h).ReadAny(bare, []string{body}); read || known {
			t.Errorf("%s: a transcript with no tool call: read %v known %v, want unknown", h, read, known)
		}
	}
}

// R6: a Hermes child is resolved from its delegation log by the claim token
// in its goal line. The log's final end
// line is its end marker; a read_file of prompt.md, as the page tells it, is a
// read of the body; Hermes reads no usage and does not claim its children
// die with their coordinator.
func TestHermesAdapter(t *testing.T) {
	root := t.TempDir()
	ended, err := os.ReadFile(filepath.Join("testdata", "hermes", "task-ended.log"))
	if err != nil {
		t.Fatal(err)
	}
	logA := filepath.Join(root, "cache", "delegation", "live", "deleg_fixture", "task-0.log")
	write(t, logA, string(ended))
	write(t, filepath.Join(root, "cache", "delegation", "live", "deleg_other", "task-0.log"), "goal: Run: super wf prompt --claim ffffffffffffffff, then do what it prints\n")
	h := harness.For("hermes")
	if h.Name() != "hermes" {
		t.Fatalf("name %q", h.Name())
	}
	c := h.Caller(envOf(map[string]string{"HERMES_SESSION_ID": "fixture-hermes-parent", "HERMES_HOME": root}))
	if c.Harness != "hermes" || c.Session != "fixture-hermes-parent" || c.Root != root || !c.FromEnv {
		t.Fatalf("caller = %+v", c)
	}
	want := harness.Want{Token: "1111222233334444"}
	got := h.Resolve(root, c.Session, []harness.Want{want, {Token: "0000000000000000"}})
	if len(got) != 1 || got[want].Agent != "deleg_fixture/task-0" || got[want].Transcript != logA {
		t.Fatalf("resolve = %+v", got)
	}
	if !h.Ended(logA) || h.Ended(filepath.Join("testdata", "hermes", "task-midrun.log")) {
		t.Error("ended: want the final end line to end it, and a log without it not")
	}
	body := "/state/runs/sample/prompt/1111222233334444/prompt.md"
	// Hermes previews a read_file by base name alone: prompt.md could be any
	// unit's body, so it is unknown (context unverified), never a pass.
	if read, known := h.ReadAny(logA, []string{body}); read || known {
		t.Errorf("read_file(prompt.md): read %v known %v, want unknown", read, known)
	}
	full := filepath.Join(root, "full.log")
	write(t, full, "01:00:28 tool     | -> terminal(cat "+body+")\n")
	if read, known := h.ReadAny(full, []string{body}); !read || !known {
		t.Errorf("a tool line naming the full path: read %v known %v, want a read", read, known)
	}
	paged := filepath.Join(root, "paged.log")
	write(t, paged, "01:00:30 tool     | -> read_file(prompt.md L1-200)\n01:00:31 tool     | -> read_file(prompt.md L201-400)\n")
	if read, known := h.ReadAny(paged, []string{body}); read || known {
		t.Errorf("a paged read_file of prompt.md: read %v known %v, want unknown like any base-name read", read, known)
	}
	// Cross-review: a claim token quoted outside the goal or kickoff line (a
	// tool result showing another unit's brief) does not make a log that
	// unit's child.
	other := t.TempDir()
	write(t, filepath.Join(other, "cache", "delegation", "live", "deleg_b", "task-0.log"),
		"goal: Run: super wf prompt --claim bbbbbbbbbbbbbbbb, then do what it prints\n01:00:28 result   | terminal ok: run: super wf prompt --claim 1111222233334444, then do what it prints\n")
	if got := h.Resolve(other, "", []harness.Want{want}); len(got) != 0 {
		t.Errorf("a log quoting the token in a tool result resolved: %+v", got)
	}
	if read, known := h.ReadAny(logA, []string{"/x/other.md"}); read || !known {
		t.Errorf("another file: read %v known %v, want unread and known", read, known)
	}
	if _, ok := h.(harness.Telemetry); ok || h.ChildrenDieWithCoordinator() {
		t.Error("hermes reads no telemetry and does not claim its children die with the coordinator")
	}
	if !strings.Contains(h.SpawnLine(harness.Spawn{Brief: "run: x"}), "run: x") {
		t.Errorf("spawn line lost the brief: %q", h.SpawnLine(harness.Spawn{Brief: "run: x"}))
	}
}

// H2: the tool limits the adapters read, which a blocking next stays under.
func TestToolLimits(t *testing.T) {
	none := envOf(nil)
	for _, c := range []struct {
		harness string
		env     harness.Env
		want    time.Duration
	}{
		{"claude", none, 120 * time.Second},
		{"claude", envOf(map[string]string{"BASH_DEFAULT_TIMEOUT_MS": "300000"}), 300 * time.Second},
		// The model sets each terminal call's timeout; 30 s is what models
		// pass, whatever the default.
		{"hermes", none, 30 * time.Second},
		{"hermes", envOf(map[string]string{"TERMINAL_TIMEOUT": "600"}), 30 * time.Second},
		{"hermes", envOf(map[string]string{"TERMINAL_TIMEOUT": "20"}), 20 * time.Second},
		{"codex", none, 0},
		{"nonesuch", none, 0},
	} {
		if got := harness.For(c.harness).ToolLimit(c.env); got != c.want {
			t.Errorf("%s: tool limit %s, want %s", c.harness, got, c.want)
		}
	}
}

// G5: Hermes previews read_file by base name, but logs the result's first
// 400 characters: a result that is the start of this unit's body (at least
// 200 characters of it) is a read; another body's, or a short one, is not.
func TestHermesBaseNameReadIsVerifiedByItsResult(t *testing.T) {
	h := harness.For("hermes")
	log := filepath.Join("testdata", "hermes", "task-basename-read.log")
	body := filepath.Join("testdata", "hermes", "body", "prompt.md")
	if read, known := h.ReadAny(log, []string{body}); !read || !known {
		t.Errorf("the base-name read of this body = (%v, %v), want a read", read, known)
	}
	raw, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	other := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(other, []byte(strings.Replace(string(raw), "GCD", "Gcd", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if read, known := h.ReadAny(log, []string{other}); read || known {
		t.Errorf("a base-name read of another body = (%v, %v), want unknown", read, known)
	}
	// A preview shorter than 200 characters proves nothing.
	short := filepath.Join(dir, "short", "prompt.md")
	_ = os.MkdirAll(filepath.Dir(short), 0o755)
	_ = os.WriteFile(short, []byte("tiny body\n"), 0o644)
	shortLog := filepath.Join(dir, "short.log")
	_ = os.WriteFile(shortLog, []byte("10:07:00 tool     | -> read_file(prompt.md)\n10:07:00 result   | read_file ok 0.0s: {\"content\": \"1|tiny body\", \"total_lines\": 1}\n"), 0o644)
	if read, known := h.ReadAny(shortLog, []string{short}); read || known {
		t.Errorf("a short base-name read = (%v, %v), want unknown", read, known)
	}
}

// A Hermes delegate_task child carries HERMES_DELEGATED_CHILD_CONTEXT
// and its own session id; without the marker the session is the
// coordinator's own.
func TestHermesCallerTellsChildFromCoordinator(t *testing.T) {
	h := harness.For("hermes")
	child := h.Caller(envOf(map[string]string{"HERMES_SESSION_ID": "fixture-hermes-child", "HERMES_DELEGATED_CHILD_CONTEXT": "/example/u/.hermes"}))
	coord := h.Caller(envOf(map[string]string{"HERMES_SESSION_ID": "fixture-hermes-parent"}))
	if child.Is != harness.IsChild || coord.Is != harness.IsSelf {
		t.Errorf("child %v, coordinator %v", child.Is, coord.Is)
	}
	if none := h.Caller(envOf(map[string]string{})); none.Is != harness.IsUnknown {
		t.Errorf("no session: %v", none.Is)
	}
	// A kanban worker's terminal carries the marker too, and may coordinate;
	// another harness inside a Hermes terminal shares its session: unknown.
	for name, env := range map[string]map[string]string{
		"kanban worker":            {"HERMES_SESSION_ID": "w", "HERMES_DELEGATED_CHILD_CONTEXT": "/r", "HERMES_KANBAN_TASK": "t1"},
		"claude in hermes":         {"HERMES_SESSION_ID": "outer", "CLAUDECODE": "1"},
		"claude child in hermes":   {"HERMES_SESSION_ID": "outer", "CLAUDE_CODE_SESSION_ID": "c"},
		"codex in a hermes window": {"HERMES_SESSION_ID": "outer", "CODEX_THREAD_ID": "t"},
	} {
		if c := h.Caller(envOf(env)); c.Is != harness.IsUnknown {
			t.Errorf("%s: %v, want unknown", name, c.Is)
		}
	}
	// The delegated-child marker is positive evidence even with another
	// harness's variables inherited.
	if c := h.Caller(envOf(map[string]string{"HERMES_SESSION_ID": "kid", "HERMES_DELEGATED_CHILD_CONTEXT": "/p", "CODEX_THREAD_ID": "outer", "CODEX_SESSION_ID": "outer"})); c.Is != harness.IsChild {
		t.Errorf("a Hermes delegate with inherited Codex variables: %v, want child", c.Is)
	}
}

// A failed Hermes child turn ends its log on "final | end status=failed".
// A log that stops mid-reply is still open.
func TestHermesFailedTurnEnds(t *testing.T) {
	h := harness.For("hermes")
	if !h.Ended(filepath.Join("testdata", "hermes", "task-failed-turn.log")) {
		t.Error("a failed child turn's log has not ended")
	}
	p := filepath.Join(t.TempDir(), "mid.log")
	_ = os.WriteFile(p, []byte("13:47:00 assistant| such? that? such?\n"), 0o644)
	if h.Ended(p) {
		t.Error("a log mid-reply has ended")
	}
}

// The tool-output limits the adapters know, which a blocking next's offer
// stays under.
func TestOutputLimits(t *testing.T) {
	for _, c := range []struct {
		harness      string
		env          map[string]string
		bytes, lines int
	}{
		{"claude", nil, 30000, 0},
		{"claude", map[string]string{"BASH_MAX_OUTPUT_LENGTH": "100000"}, 30000, 0}, // the read-back window only
		{"codex", nil, 10 << 10, 256},
		{"hermes", nil, 50000, 0},
		{"nonesuch", nil, 0, 0},
	} {
		env := map[string]string{"CLAUDE_CONFIG_DIR": t.TempDir()}
		for k, v := range c.env {
			env[k] = v
		}
		if b, l := harness.For(c.harness).OutputLimit(envOf(env)); b != c.bytes || l != c.lines {
			t.Errorf("%s: output limit %d bytes %d lines, want %d, %d", c.harness, b, l, c.bytes, c.lines)
		}
	}
	// Claude Code's bashOutputMaxChars setting, clamped to 4000..128000.
	for set, want := range map[string]int{`{"bashOutputMaxChars": 8000}`: 8000, `{"bashOutputMaxChars": 100}`: 4000, `{"bashOutputMaxChars": 900000}`: 128000, `{}`: 30000} {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "settings.json"), []byte(set), 0o644)
		if b, _ := harness.For("claude").OutputLimit(envOf(map[string]string{"CLAUDE_CONFIG_DIR": dir})); b != want {
			t.Errorf("settings %s: %d, want %d", set, b, want)
		}
	}
}

func TestCodexHomeIsNotShadowedByAncestorHomeConfig(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "projects", "repo")
	selected := t.TempDir()
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".codex", "config.toml"), "[agents]\nmax_threads = 20\n")
	write(t, filepath.Join(selected, "config.toml"), "[agents]\nmax_threads = 7\n")
	env := envOf(map[string]string{"HOME": home, "CODEX_HOME": selected})
	if n, src := harness.For("codex").Limit(env, repo); n != 7 || !strings.Contains(src, selected) {
		t.Fatalf("run home shadowed: %d from %s", n, src)
	}
	write(t, filepath.Join(repo, ".codex", "config.toml"), "[agents]\nmax_threads = 3\n")
	if n, src := harness.For("codex").Limit(env, repo); n != 3 || !strings.Contains(src, repo) {
		t.Fatalf("project override: %d from %s", n, src)
	}
}

func TestCodexHomeBoundaryResolvesSymlinks(t *testing.T) {
	home := t.TempDir()
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "projects", "repo")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	selected := t.TempDir()
	write(t, filepath.Join(home, ".codex", "config.toml"), "[agents]\nmax_threads = 20\n")
	write(t, filepath.Join(selected, "config.toml"), "[agents]\nmax_threads = 7\n")
	for _, paths := range [][2]string{{alias, repo}, {home, filepath.Join(alias, "projects", "repo")}} {
		env := envOf(map[string]string{"HOME": paths[0], "CODEX_HOME": selected})
		if n, src := harness.For("codex").Limit(env, paths[1]); n != 7 || !strings.Contains(src, selected) {
			t.Fatalf("aliased home shadowed: %d from %s", n, src)
		}
	}
}

func TestCodexResumeLine(t *testing.T) {
	adapter := harness.For("codex")
	const id = "00000000-0000-7000-8000-000000000003"
	const message = "Continue unit \"work\".\nKeep its context."
	const want = `collaboration.followup_task({"target":"00000000-0000-7000-8000-000000000003","message":"Continue unit \"work\".\nKeep its context."})`
	if got := adapter.ResumeLine(id, message); got != want {
		t.Errorf("ResumeLine = %q, want %q", got, want)
	}
	if got := adapter.ResumeLine("", message); got != "" {
		t.Errorf("ResumeLine with no thread ID = %q, want empty", got)
	}
}
