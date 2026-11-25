package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOpencodeIsRegistered(t *testing.T) {
	if !Known("opencode") {
		t.Fatal("opencode is a registered built-in adapter")
	}
	if _, ok := For("opencode").(opencode); !ok {
		t.Fatalf("For(opencode) = %T, want the opencode adapter", For("opencode"))
	}
}

func TestOpencodeCallerFromEnvironment(t *testing.T) {
	a := For("opencode")
	cases := []struct {
		name    string
		env     map[string]string
		session string
		agent   string
		is      Verdict
	}{
		{"no plugin variables", map[string]string{"OPENCODE": "1"}, "", "", IsUnknown},
		{"parent without session", map[string]string{"OPENCODE": "1", "OPENCODE_PARENT_SESSION_ID": "ses_p"}, "", "", IsUnknown},
		{"root", map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_r", "OPENCODE_PARENT_SESSION_ID": ""}, "ses_r", "", IsSelf},
		{"child", map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p"}, "ses_p", "ses_c", IsChild},
		// A failed lookup may be a child's: no session, so it never drives.
		{"parent lookup failed", map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_u", "OPENCODE_PARENT_SESSION_ID": "unknown"}, "", "ses_u", IsUnknown},
	}
	for _, c := range cases {
		c.env["HOME"] = "/example/h"
		got := a.Caller(envMap(c.env))
		if got.Harness != "opencode" || got.Session != c.session || got.Agent != c.agent || got.Is != c.is || got.Root != "/example/h/.local/share/opencode" || !got.FromEnv {
			t.Errorf("%s: Caller = %+v, want session %q agent %q verdict %v, root under HOME", c.name, got, c.session, c.agent, c.is)
		}
	}
	if r := a.Caller(envMap(map[string]string{"XDG_DATA_HOME": "/example/d", "OPENCODE_SESSION_ID": "s"})).Root; r != "/example/d/opencode" {
		t.Errorf("Root with XDG_DATA_HOME = %q", r)
	}
}

func TestOpencodeSameChild(t *testing.T) {
	a := For("opencode")
	owner := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	same := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	sibling := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_d", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	coord := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_p", "OPENCODE_PARENT_SESSION_ID": ""}))
	if !SameChild(owner, same) || SameChild(owner, sibling) || SameChild(owner, coord) {
		t.Fatal("an opencode child is its own session under the same coordinator")
	}
}

// Detection: opencode's session variable outranks Codex and Claude (an
// opencode started from their tabs inherits their variables), but a bare
// OPENCODE, which anything started from an opencode shell inherits, counts
// only when no other harness's marker is set.
func TestOpencodeDetectOrder(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OPENCODE": "1"}, "opencode"},
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r"}, "opencode"},
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r", "OPENCODE": "1", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDECODE": "1"}, "opencode"},
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r", "OPENCODE": "1", "CODEX_THREAD_ID": "t"}, "opencode"},
		// A Claude or Codex coordinator in a shell that leaked a bare OPENCODE.
		{map[string]string{"OPENCODE": "1", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDECODE": "1"}, "claude"},
		{map[string]string{"OPENCODE": "1", "CODEX_THREAD_ID": "t", "CODEX_SESSION_ID": "t"}, "codex"},
		// Inner harnesses that start from an opencode shell are named first.
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r", "OPENCODE": "1", "HERMES_SESSION_ID": "h"}, "hermes"},
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r", "OPENCODE": "1", "PI_SESSION_ID": "p"}, "pi"},
		{map[string]string{"OPENCODE_SESSION_ID": "ses_r", "OPENCODE": "1", "ANTIGRAVITY_CONVERSATION_ID": "a"}, "agy"},
		{map[string]string{"OPENCODE": "1", "ANTIGRAVITY_CONVERSATION_ID": "a"}, "agy"},
	}
	for _, c := range cases {
		if got := Detect(envMap(c.env)); got != c.want {
			t.Errorf("Detect(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

// A Claude coordinator whose shell leaked a bare OPENCODE is Claude's
// coordinator: opencode reads no identity from it and calls nothing a child.
func TestClaudeCoordinatorWithLeakedOpencodeMarker(t *testing.T) {
	env := envMap(map[string]string{"OPENCODE": "1", "OPENCODE_PID": "42", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDECODE": "1"})
	if got := Detect(env); got != "claude" {
		t.Fatalf("Detect = %q, want claude", got)
	}
	if c := For("opencode").Caller(env); c.Is != IsUnknown || c.Session != "" {
		t.Fatalf("opencode reads no identity from a bare OPENCODE: %+v", c)
	}
}

// A Claude started from an opencode bash shell inherits the plugin's
// variables: the environment cannot tell that from an opencode started from
// a Claude tab, and the innermost-harness rule names opencode (the accepted
// tradeoff COORDINATOR.md states). With a parent set the caller is a child.
func TestClaudeWithLeakedOpencodePluginVariables(t *testing.T) {
	env := envMap(map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDECODE": "1"})
	if got := Detect(env); got != "opencode" {
		t.Fatalf("Detect = %q, want opencode", got)
	}
	if c := For("opencode").Caller(env); c.Is != IsChild {
		t.Fatalf("Caller = %+v, want a child", c)
	}
}

// A plugin-less opencode inside a Claude tab has only the weak OPENCODE: it
// reads as the Claude tab's session (COORDINATOR.md states it).
func TestPluginlessOpencodeInClaudeTabReadsAsClaude(t *testing.T) {
	env := envMap(map[string]string{"OPENCODE": "1", "CLAUDE_CODE_SESSION_ID": "s", "CLAUDECODE": "1"})
	if got := Detect(env); got != "claude" {
		t.Fatalf("Detect = %q, want claude", got)
	}
}

func TestOpencodeMarkers(t *testing.T) {
	env := envMap(map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_r"})
	if got := Markers(env); strings.Join(got, ",") != "OPENCODE_SESSION_ID,OPENCODE" {
		t.Fatalf("Markers = %v", got)
	}
	names := MarkerNames()
	for _, want := range []string{"OPENCODE_SESSION_ID", "OPENCODE"} {
		if !contains(names, want) {
			t.Errorf("MarkerNames() lacks %s", want)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// taskCall parses `task(k="v", ...)` into its JSON string values.
func taskCall(t *testing.T, line string) map[string]string {
	t.Helper()
	if !strings.HasPrefix(line, "task(") || !strings.HasSuffix(line, ")") {
		t.Fatalf("not a task call: %s", line)
	}
	out := map[string]string{}
	re := regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*")`)
	for _, m := range re.FindAllStringSubmatch(line[len("task("):len(line)-1], -1) {
		var v string
		if err := json.Unmarshal([]byte(m[2]), &v); err != nil {
			t.Fatalf("value %s: %v", m[2], err)
		}
		out[m[1]] = v
	}
	return out
}

func TestOpencodeSpawnLine(t *testing.T) {
	a := For("opencode")
	brief := `workflow prompt --claim 0a1b2c3d "quoted" <x> & y`
	line := a.SpawnLine(Spawn{Brief: brief, AgentType: a.AgentType(), Model: "ignored", Effort: "high"})
	got := taskCall(t, line)
	if got["prompt"] != brief || got["subagent_type"] != "general" || got["description"] == "" {
		t.Fatalf("SpawnLine = %s", line)
	}
	for _, k := range []string{"model", "effort", "background", "task_id"} {
		if _, ok := got[k]; ok {
			t.Errorf("SpawnLine passes %s: %s", k, line)
		}
	}
	if a.AgentType() != "general" {
		t.Fatalf("AgentType = %q", a.AgentType())
	}
}

func TestOpencodeResumeLine(t *testing.T) {
	a := For("opencode")
	if a.ResumeLine("", "go on") != "" {
		t.Fatal("no agent, no resume")
	}
	line := a.ResumeLine("ses_c", "finish with done")
	got := taskCall(t, line)
	if got["task_id"] != "ses_c" || got["prompt"] != "finish with done" || got["subagent_type"] != "general" || got["description"] == "" {
		t.Fatalf("ResumeLine = %s", line)
	}
	n, ok := a.(ResumeNote)
	if !ok || !strings.Contains(n.ResumeNote(), "task_id") {
		t.Fatal("opencode says what an unknown task_id does")
	}
}

func TestOpencodeModelsAndLimits(t *testing.T) {
	a := For("opencode")
	if a.Tiers() != nil {
		t.Fatal("opencode has no tier table: the task tool takes no model")
	}
	if _, ok := a.(ModelChecker); ok {
		t.Fatal("opencode refuses no model value: none is applied")
	}
	if nm, ok := a.(NoModelSpawn); !ok || !strings.Contains(nm.NoModel(), "coordinator's model") {
		t.Fatal("opencode says its children run on the coordinator's model")
	}
	if b, l := a.OutputLimit(envMap(nil)); b != 51200 || l != 2000 {
		t.Fatalf("OutputLimit = %d, %d", b, l)
	}
	if d := a.ToolLimit(envMap(nil)); d != 120*time.Second {
		t.Fatalf("ToolLimit = %v", d)
	}
	if d := a.ToolLimit(envMap(map[string]string{"OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS": "600000"})); d != 10*time.Minute {
		t.Fatalf("ToolLimit with the variable = %v", d)
	}
	if d := a.ToolLimit(envMap(map[string]string{"OPENCODE_EXPERIMENTAL_BASH_DEFAULT_TIMEOUT_MS": "soon"})); d != 120*time.Second {
		t.Fatalf("ToolLimit with a bad value = %v", d)
	}
	if !a.ChildrenDieWithCoordinator() {
		t.Fatal("opencode children are sessions inside the coordinator's process")
	}
	if n, src := a.Limit(envMap(nil), t.TempDir()); n != 0 || src != "" {
		t.Fatalf("Limit = %d %q", n, src)
	}
}

// opencode keeps no records the engine reads: every record question has the
// no-records answer, and no adapter path runs a program. A stub opencode on
// PATH fails the test if anything executes it.
func TestOpencodeReadsNoRecordsAndRunsNothing(t *testing.T) {
	dir := t.TempDir()
	tripped := filepath.Join(dir, "ran")
	stub := "#!/bin/sh\ntouch " + tripped + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	env := envMap(map[string]string{"HOME": home, "XDG_DATA_HOME": filepath.Join(home, "data"), "OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_r"})

	a := For(Detect(env))
	c := a.Caller(env)
	want := Want{Token: "0a1b2c3d", After: time.Now().Add(-time.Hour)}
	if got := a.Resolve(c.Root, c.Session, []Want{want}); len(got) != 0 {
		t.Fatalf("Resolve = %v", got)
	}
	if a.Ended(filepath.Join(home, "x")) {
		t.Fatal("Ended reads nothing")
	}
	if _, known := a.ReadAny(filepath.Join(home, "x"), []string{"/etc/hosts"}); known {
		t.Fatal("ReadAny reads nothing")
	}
	if _, ok := a.(Telemetry); ok {
		t.Fatal("opencode reads no telemetry")
	}
	_ = a.SpawnLine(Spawn{Brief: "b", AgentType: a.AgentType()})
	_ = a.ResumeLine("ses_c", "m")
	a.OutputLimit(env)
	a.ToolLimit(env)
	a.Limit(env, home)
	if _, err := os.Stat(tripped); err == nil {
		t.Fatal("an adapter path ran opencode")
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("an adapter path wrote under HOME: %v", entries)
	}
}

// The thin path a coordinator's call takes: Detect names opencode, For
// returns its adapter, and the spawn line is opencode's task call.
func TestOpencodeThinPath(t *testing.T) {
	env := envMap(map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_r", "OPENCODE_PARENT_SESSION_ID": ""})
	a := For(Detect(env))
	if a.Name() != "opencode" || a.Caller(env).Is != IsSelf {
		t.Fatalf("thin path: %s %+v", a.Name(), a.Caller(env))
	}
	if got := taskCall(t, a.SpawnLine(Spawn{Brief: "workflow prompt --claim t", AgentType: a.AgentType()})); got["prompt"] != "workflow prompt --claim t" {
		t.Fatalf("spawn = %v", got)
	}
}

// A Hermes coordinator started from an opencode shell inherits a bare
// OPENCODE; a weak marker is no other agent, so Hermes still knows itself.
func TestHermesIgnoresWeakOpencodeMarker(t *testing.T) {
	c := For("hermes").Caller(envMap(map[string]string{"HERMES_SESSION_ID": "h", "OPENCODE": "1"}))
	if c.Is != IsSelf {
		t.Fatalf("Caller = %+v, want IsSelf", c)
	}
	if !otherAgent(envMap(map[string]string{"OPENCODE_SESSION_ID": "s"})) {
		t.Fatal("opencode's session variable is another agent's")
	}
}

// A Hermes started from an opencode bash command inherits the plugin's
// session variable: another agent's, so Hermes cannot call itself the
// coordinator (the accepted reverse leak).
func TestHermesFromOpencodeBashIsUnknown(t *testing.T) {
	c := For("hermes").Caller(envMap(map[string]string{"HERMES_SESSION_ID": "h", "OPENCODE": "1", "OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	if c.Is != IsUnknown {
		t.Fatalf("Caller = %+v, want unknown", c)
	}
}

// A first fetch made while the plugin's parent lookup failed recorded the
// child's agent and no session; the same child, identified later, matches.
func TestOpencodeSameChildAcrossALookupFailure(t *testing.T) {
	a := For("opencode")
	early := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "unknown"}))
	later := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_c", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	other := a.Caller(envMap(map[string]string{"OPENCODE_SESSION_ID": "ses_d", "OPENCODE_PARENT_SESSION_ID": "ses_p"}))
	if !SameChild(early, later) || SameChild(early, other) {
		t.Fatal("the agent alone matches a fetch with no session")
	}
}

func TestOpencodeHints(t *testing.T) {
	if got := WeakHints(envMap(map[string]string{"OPENCODE": "1", "CLAUDECODE": "1"})); len(got) != 1 || !strings.Contains(got[0], "opencode/super-workflow.js") {
		t.Fatalf("WeakHints = %v", got)
	}
	if got := WeakHints(envMap(map[string]string{"OPENCODE": "1", "OPENCODE_SESSION_ID": "s"})); len(got) != 0 {
		t.Fatalf("WeakHints with the plugin = %v", got)
	}
	sh := For("opencode").(SessionHinter)
	if sh.SessionHint(envMap(map[string]string{"OPENCODE_SESSION_ID": "s", "OPENCODE_PARENT_SESSION_ID": "unknown"})) == "" || sh.SessionHint(envMap(map[string]string{"OPENCODE_SESSION_ID": "s"})) != "" {
		t.Fatal("SessionHint fires only for a failed parent lookup")
	}
}

// The plugin's own tests (opencode/super-workflow.test.mjs), where node is
// installed.
func TestOpencodePluginScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "--test", filepath.Join("..", "opencode", "super-workflow.test.mjs")).CombinedOutput()
	if err != nil {
		t.Fatalf("plugin tests: %v\n%s", err, out)
	}
}
