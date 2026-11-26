package harness_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/harness"
)

func env(m map[string]string) harness.Env { return func(k string) string { return m[k] } }

// A headless codex exec shell (captured on codex-cli 0.172.0) has its own
// thread as its session: without the spawn verb's parent entry it reads as
// a coordinator; with it, as that coordinator's child.
func TestCodexHeadlessChildIsAChildOnlyWithItsParent(t *testing.T) {
	shell := map[string]string{"CODEX_THREAD_ID": "01a2192b", "CODEX_SESSION_ID": "01a2192b", "CODEX_CI": "1", "HOME": "/h"}
	if c := harness.For("codex").Caller(env(shell)); c.Is != harness.IsSelf {
		t.Fatalf("bare headless shell: got %v, want IsSelf (the case the parent entry exists for)", c.Is)
	}
	shell[harness.ParentVar] = "claude:301a48c6"
	c := harness.For("codex").Caller(env(shell))
	if c.Is != harness.IsChild || c.Session != "301a48c6" || c.Agent != "01a2192b" || c.Harness != "codex" {
		t.Fatalf("headless child: %+v", c)
	}
	// A parent entry naming the caller itself, or with no session, changes nothing.
	for _, v := range []string{"codex:01a2192b", "claude", "claude:"} {
		shell[harness.ParentVar] = v
		if c := harness.For("codex").Caller(env(shell)); c.Is != harness.IsSelf {
			t.Errorf("%s=%q: got %v, want IsSelf", harness.ParentVar, v, c.Is)
		}
	}
}

func TestClaudeHeadlessChildIsAChildOnlyWithItsParent(t *testing.T) {
	shell := map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "4ba3398e", "CLAUDE_CONFIG_DIR": "/p/claude-1"}
	if c := harness.For("claude").Caller(env(shell)); c.Is != harness.IsUnknown || c.Session != "4ba3398e" {
		t.Fatalf("plain claude shell: %+v", c)
	}
	shell[harness.ParentVar] = "codex:01a21939"
	c := harness.For("claude").Caller(env(shell))
	if c.Is != harness.IsChild || c.Session != "01a21939" || c.Agent != "4ba3398e" || c.Root != "/p/claude-1" {
		t.Fatalf("headless child: %+v", c)
	}
}

func TestHeadlessArgvAsksForNativeProtections(t *testing.T) {
	s := harness.HeadlessSpawn{Model: "gpt-6.2-astra", Effort: "high", Workspace: "/w", StateDir: "/s", WF: "/bin/workflow"}
	got := strings.Join(harness.HeadlessFor("codex").HeadlessArgv(s), " ")
	want := `codex exec --skip-git-repo-check -s workspace-write -c approval_policy="never" -C /s -m gpt-6.2-astra -c model_reasoning_effort=high --json -`
	if got != want {
		t.Errorf("codex argv\n got %s\nwant %s", got, want)
	}
	s.Model, s.Effort, s.Session = "fable", "high", "u-1"
	got = strings.Join(harness.HeadlessFor("claude").HeadlessArgv(s), " ")
	want = `claude -p --model fable --effort high --session-id u-1 --permission-mode dontAsk --add-dir /s --output-format json --allowedTools Bash(/bin/workflow *) Bash(cd /w *) Read Grep Glob Write(//s/**) Edit(//s/**)`
	if got != want {
		t.Errorf("claude argv\n got %s\nwant %s", got, want)
	}
	for _, bad := range []string{"bypass", "dangerously", "--full-auto", "--bare"} {
		for _, h := range []string{"claude", "codex"} {
			if a := strings.Join(harness.HeadlessFor(h).HeadlessArgv(s), " "); strings.Contains(a, bad) {
				t.Errorf("%s argv holds %q: %s", h, bad, a)
			}
		}
	}
}

func TestHeadlessSessionAndAdapters(t *testing.T) {
	if s := harness.HeadlessFor("codex").HeadlessSession("k"); s != "" {
		t.Errorf("codex picks its own thread id, got %q", s)
	}
	u := harness.HeadlessFor("claude").HeadlessSession("k")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(u) {
		t.Errorf("claude session id %q is not a v4 UUID", u)
	}
	for _, h := range []string{"pi", "agy", "opencode", "hermes", "generic"} {
		if harness.HeadlessFor(h) != nil {
			t.Errorf("%s has no headless launch yet", h)
		}
	}
	if harness.HeadlessFor("Claude-Code") == nil {
		t.Error("an alias resolves to its adapter")
	}
}

func TestHeadlessLoginChecks(t *testing.T) {
	argv, ok := harness.HeadlessFor("claude").HeadlessLogin()
	if strings.Join(argv, " ") != "claude auth status" {
		t.Errorf("claude login argv %v", argv)
	}
	if !ok([]byte(`{"loggedIn": true, "authMethod": "claude.ai"}`+"\n"), 0) || ok([]byte(`{"loggedIn": false}`), 0) || ok([]byte(`{"loggedIn": true}`), 1) || ok([]byte("not json"), 0) {
		t.Error("claude login parse")
	}
	argv, ok = harness.HeadlessFor("codex").HeadlessLogin()
	if strings.Join(argv, " ") != "codex login status" || !ok(nil, 0) || ok(nil, 1) {
		t.Errorf("codex login check %v", argv)
	}
}

// The scrub drops every headless harness's identity, the coordinator's
// tokens and sockets included (the probe's codex child inherited
// CLAUDE_CODE_MESSAGING_TOKEN), and keeps the login homes.
func TestScrubbedEnv(t *testing.T) {
	in := []string{"CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_MESSAGING_TOKEN=t", "CLAUDE_CONFIG_DIR=/p", "CLAUDE_PID=9", "CLAUDE_CODE_OAUTH_TOKEN=o",
		"CODEX_THREAD_ID=x", "CODEX_SESSION_ID=x", "CODEX_SANDBOX_NETWORK_DISABLED=1", "CODEX_HOME=/c", "HERDR_PANE_ID=p", "PI_SESSION_ID=pi", "HERMES_SESSION_ID=h", "PATH=/bin"}
	got := harness.ScrubbedEnv(in, append(harness.HeadlessScrub(), "HERDR_*"))
	want := []string{"CLAUDE_CONFIG_DIR=/p", "CLAUDE_CODE_OAUTH_TOKEN=o", "CODEX_HOME=/c", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A headless claude child's transcript is its own top-level session file,
// found by the agent id (its session) its environment gave.
func TestClaudeResolvesAHeadlessChildBySession(t *testing.T) {
	root := t.TempDir()
	f := root + "/projects/-w/4ba3398e.jsonl"
	if err := os.MkdirAll(root+"/projects/-w", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := harness.Want{Token: "t", Agent: "4ba3398e"}
	got := harness.For("claude").Resolve(root, "", []harness.Want{w})
	if got[w].Transcript != f || got[w].Agent != "4ba3398e" {
		t.Fatalf("got %+v", got)
	}
	if got := harness.For("claude").Resolve(root, "coord", []harness.Want{{Token: "t", Agent: "missing"}}); len(got) != 0 {
		t.Errorf("an unknown session resolved: %+v", got)
	}
}
