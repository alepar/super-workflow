package wf_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// clFamilyDef is clAgentDef's one agent on another harness than the
// coordinator's (OnFamily) at a tier: the cross-harness primitive.
func clFamilyDef(name string, f wf.Family, tier wf.Tier) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	d.OnFailure(wf.Unavailable, gaveUp)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[clAsk](wf.Const(1), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	a.Dispatch(wf.OnFamily(f), wf.ModelTier(tier))
	return d
}

// familyDefs are the cross-harness fixtures: a named codex unit, a named
// claude unit (each at the top tier, as in "a fable class subagent on
// codex"), and one on the other family.
func familyDefs() []wf.Registration {
	return []wf.Registration{
		{Def: clFamilyDef("family-codex", wf.FamilyNamed("codex"), wf.TierTop)},
		{Def: clFamilyDef("family-claude", wf.FamilyNamed("claude"), wf.TierTop)},
		{Def: clFamilyDef("family-other", wf.FamilyOther, wf.TierStrong)},
	}
}

func familyConfig() wf.MainConfig {
	cfg := clConfig()
	cfg.Defs = append(cfg.Defs, familyDefs()...)
	return cfg
}

// fakeHeadlessMain is the test binary as a fake codex or claude CLI
// (WF_TEST_FAKE_NAME), started through the fake's script on PATH. It
// answers the preflight's --version, login and execpolicy checks, and as a
// headless child records its argv and environment, takes the identity its
// CLI gives a fresh session, and runs the brief on stdin: prompt, result,
// done (WF_TEST_FAKE_MODE=exit stops after prompt; hang waits to be killed).
func fakeHeadlessMain() int {
	name, args, dir := os.Getenv("WF_TEST_FAKE_NAME"), os.Args[1:], os.Getenv("WF_TEST_FAKE_DIR")
	switch strings.Join(args, " ") {
	case "--version":
		fmt.Println(name + " 9.9.9 (fake)")
		return 0
	case "login status":
		if os.Getenv("WF_TEST_FAKE_LOGIN") == "no" {
			return 1
		}
		return 0
	case "auth status":
		fmt.Printf("{\"loggedIn\": %v}\n", os.Getenv("WF_TEST_FAKE_LOGIN") != "no")
		return 0
	}
	if len(args) > 2 && args[0] == "execpolicy" && args[1] == "check" {
		return fakeExecpolicyCheck(args[2:], dir)
	}
	rec, _ := json.Marshal(map[string]any{"argv": args, "env": os.Environ(), "cwd": must2(os.Getwd())})
	_ = os.WriteFile(filepath.Join(dir, name+".json"), rec, 0o600)
	switch name {
	case "codex":
		id := fmt.Sprintf("thread-%d", os.Getpid())
		os.Setenv("CODEX_THREAD_ID", id)
		os.Setenv("CODEX_SESSION_ID", id)
	case "claude":
		os.Setenv("CLAUDECODE", "1")
		if i := slices.Index(args, "--session-id"); i >= 0 && i+1 < len(args) {
			os.Setenv("CLAUDE_CODE_SESSION_ID", args[i+1])
		}
	}
	if os.Getenv("WF_TEST_FAKE_MODE") == "exit-before" {
		fmt.Fprintln(os.Stderr, "fake: leaving before the prompt")
		return 0
	}
	if os.Getenv("WF_TEST_FAKE_MODE") == "hang" {
		time.Sleep(time.Minute)
		return 0
	}
	brief, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	_, cmdline, _ := strings.Cut(strings.TrimSpace(brief), "&& ")
	words := strings.Fields(cmdline) // the brief's words carry no quoting in these fixtures
	if len(words) < 2 {
		fmt.Fprintln(os.Stderr, "fake: no command in the brief:", brief)
		return 2
	}
	cli := func(a ...string) (string, error) {
		cmd := exec.Command(words[0], a...)
		cmd.Env = append(os.Environ(), "WF_TEST_CLI=1", "WF_TEST_FAMILY=1", "WF_TEST_FAKE_HEADLESS=0")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	text, err := cli(words[1:]...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: prompt:", err, text)
		return 3
	}
	if p := regexp.MustCompile(`\S+/prompt\.md`).FindString(text); p != "" {
		if b, err := os.ReadFile(strings.Trim(p, "`'\".,")); err == nil {
			text += string(b)
		}
	}
	if os.Getenv("WF_TEST_FAKE_MODE") == "exit" {
		fmt.Fprintln(os.Stderr, "fake: leaving without done")
		return 0
	}
	out := strings.Trim(regexp.MustCompile(`\S+/out/\S+`).FindString(text), "`'\".,")
	if out == "" {
		fmt.Fprintln(os.Stderr, "fake: no result path in the prompt:", text)
		return 4
	}
	if err := os.WriteFile(out, []byte(`{"OK":true}`), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake:", err)
		return 5
	}
	claim := words[slices.Index(words, "--claim")+1]
	doneArgs := []string{"done", words[2], "--claim", claim}
	if i := slices.Index(words, "--state-dir"); i >= 0 {
		doneArgs = append(doneArgs, "--state-dir", words[i+1])
	}
	if o, err := cli(doneArgs...); err != nil {
		fmt.Fprintln(os.Stderr, "fake: done:", err, o)
		return 6
	}
	return 0
}

func must2(s string, _ error) string { return s }

// fakeExecpolicyCheck is `codex execpolicy check --rules F... CMD...` as
// Codex decides it for one command: allow when a prefix_rule pattern of
// plain words in one of the files is a prefix of CMD. It records CMD, the
// argv the check was asked about, in execpolicy.json.
func fakeExecpolicyCheck(args []string, dir string) int {
	var patterns [][]string
	for len(args) > 1 && args[0] == "--rules" {
		b, _ := os.ReadFile(args[1])
		for _, m := range regexp.MustCompile(`prefix_rule\(pattern = (\[[^\]]*\])`).FindAllStringSubmatch(string(b), -1) {
			var p []string
			if json.Unmarshal([]byte(m[1]), &p) == nil {
				patterns = append(patterns, p)
			}
		}
		args = args[2:]
	}
	rec, _ := json.Marshal(args)
	_ = os.WriteFile(filepath.Join(dir, "execpolicy.json"), rec, 0o600)
	for _, p := range patterns {
		if len(p) <= len(args) && slices.Equal(p, args[:len(p)]) {
			fmt.Println(`{"decision":"allow"}`)
			return 0
		}
	}
	fmt.Println(`{"matchedRules":[]}`)
	return 0
}

// headlessE2E drives the CLI as a coordinator of one harness whose unit runs
// on the other, through fake codex and claude CLIs on PATH.
type headlessE2E struct {
	t                      *testing.T
	cfg                    wf.MainConfig
	root, state, ws, fakes string
	coord                  map[string]string
}

func newHeadlessE2E(t *testing.T, coordinator string) *headlessE2E {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake CLIs are shell scripts and the liveness reads /proc")
	}
	root := t.TempDir()
	e := &headlessE2E{t: t, cfg: familyConfig(), root: root, state: filepath.Join(root, "state"), ws: filepath.Join(root, "ws"), fakes: filepath.Join(root, "bin")}
	for _, d := range []string{e.ws, e.fakes} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "claude"} {
		script := fmt.Sprintf("#!/bin/sh\nWF_TEST_FAKE_HEADLESS=1 WF_TEST_FAKE_NAME=%s exec %q \"$@\"\n", name, bin)
		if err := os.WriteFile(filepath.Join(e.fakes, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", e.fakes+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WF_TEST_FAKE_DIR", e.fakes)
	// The coordinator's own identity, also in the process environment the
	// spawn verb hands on, so the scrub has something to remove.
	switch coordinator {
	case "claude":
		e.coord = map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "coord-c", "CLAUDE_CODE_MESSAGING_TOKEN": "secret", "CLAUDE_CONFIG_DIR": filepath.Join(root, "claude"), "HOME": root}
	case "codex":
		e.coord = map[string]string{"CODEX_THREAD_ID": "coord-x", "CODEX_SESSION_ID": "coord-x", "CODEX_HOME": filepath.Join(root, "codex"), "HOME": root}
	}
	for k, v := range e.coord {
		t.Setenv(k, v)
	}
	return e
}

func (e *headlessE2E) call(args ...string) (int, string, string) {
	e.t.Helper()
	var out, errb bytes.Buffer
	code := wf.CLIWithEnv(e.cfg, append(args, "--state-dir", e.state), &out, &errb, func(k string) string { return e.coord[k] })
	return code, out.String(), errb.String()
}

// mustRaw runs the CLI with args exactly as given, adding no --state-dir.
func (e *headlessE2E) mustRaw(args ...string) string {
	e.t.Helper()
	var out, errb bytes.Buffer
	if code := wf.CLIWithEnv(e.cfg, args, &out, &errb, func(k string) string { return e.coord[k] }); code != 0 {
		e.t.Fatalf("%v exited %d:\n%s%s", args, code, out.String(), errb.String())
	}
	return out.String()
}

// setupCodexRule writes the Codex rule into the coordinator's Codex home
// with the product's own setup-codex-rules --write.
func (e *headlessE2E) setupCodexRule() {
	e.t.Helper()
	if out := e.mustRaw("setup-codex-rules", "--write"); !strings.Contains(out, "wrote the rule to "+filepath.Join(e.coord["CODEX_HOME"], "rules", "super-workflow.rules")) {
		e.t.Fatalf("setup-codex-rules: %s", out)
	}
}

func (e *headlessE2E) must(args ...string) string {
	e.t.Helper()
	code, out, errs := e.call(args...)
	if code != 0 {
		e.t.Fatalf("%v exited %d:\n%s%s", args, code, out, errs)
	}
	return out
}

func (e *headlessE2E) journal() string {
	e.t.Helper()
	return e.must("journal", "r1")
}

// waitJournal polls the journal until it holds want or the deadline passes.
func (e *headlessE2E) waitJournal(want string) string {
	e.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		j := e.journal()
		if strings.Contains(j, want) || time.Now().After(deadline) {
			return j
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// handedModels are the codex models the run's hand-outs spawned with, in
// order, space-separated.
func (e *headlessE2E) handedModels() string {
	e.t.Helper()
	var out []string
	for _, line := range strings.Split(e.journal(), "\n") {
		var r struct {
			KindName string
			Data     struct {
				Spawn *struct{ Model, Harness string }
			}
		}
		if json.Unmarshal([]byte(line), &r) == nil && r.KindName == "handed_out" && r.Data.Spawn != nil && r.Data.Spawn.Harness == "codex" {
			out = append(out, r.Data.Spawn.Model)
		}
	}
	return strings.Join(out, " ")
}

// fake is what the fake CLI named name recorded as a headless child.
func (e *headlessE2E) fake(name string) (argv, env []string) {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.fakes, name+".json"))
	if err != nil {
		e.t.Fatalf("the fake %s was never started as a child: %v", name, err)
	}
	var rec struct{ Argv, Env []string }
	if err := json.Unmarshal(b, &rec); err != nil {
		e.t.Fatal(err)
	}
	return rec.Argv, rec.Env
}

// fakeCwd is the directory the fake CLI named name started in as a child.
func (e *headlessE2E) fakeCwd(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.fakes, name+".json"))
	if err != nil {
		e.t.Fatalf("the fake %s was never started as a child: %v", name, err)
	}
	var rec struct{ Cwd string }
	if err := json.Unmarshal(b, &rec); err != nil {
		e.t.Fatal(err)
	}
	return rec.Cwd
}

// headlessSpawnLine is the spawn offer: one bare command, with no
// `cd <ws> &&` before it.
var headlessSpawnLine = regexp.MustCompile(`(?m)^(\S+ spawn r1 --claim ([0-9a-f]+) --state-dir \S+)$`)

// offer runs next and returns the spawn command's claim.
func (e *headlessE2E) offer(child string) (string, string) {
	e.t.Helper()
	out := e.must("next", "r1", "--timeout", "2s")
	if !strings.Contains(out, "start a "+child+" child for clAsk#1 by running this command") {
		e.t.Fatalf("next did not offer a %s child:\n%s", child, out)
	}
	if strings.Contains(out, "Agent(") || strings.Contains(out, "spawn_agent(") {
		e.t.Fatalf("a cross-harness unit was offered as the coordinator's own subagent:\n%s", out)
	}
	m := headlessSpawnLine.FindStringSubmatch(out)
	if m == nil {
		e.t.Fatalf("no spawn command alone on its line:\n%s", out)
	}
	return m[2], m[1]
}

// killStarted kills the process group of every headless child a journal
// recorded, so no fake outlives its test.
func killStarted(journal string) {
	for _, m := range regexp.MustCompile(`"Pid":(\d+)`).FindAllStringSubmatch(journal, -1) {
		var pid int
		fmt.Sscan(m[1], &pid)
		if pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

func hasEnv(env []string, kv string) bool { return slices.Contains(env, kv) }

func envNamed(env []string, name string) bool {
	return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
}

// The primitive, Claude to Codex: a unit named for codex at the top tier
// runs as a headless codex child at codex's top model, with codex's native
// protections, no Claude identity, and the coordinator as its parent; it
// is no caller mismatch, and the run ends.
func TestHeadlessPrimitiveClaudeToCodex(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws)
	claim, _ := e.offer("codex")
	out := e.must("spawn", "r1", "--claim", claim)
	if !strings.Contains(out, "started clAsk#1 on codex: pid ") {
		t.Fatalf("spawn: %s", out)
	}
	j := e.waitJournal(`"run_ended"`)
	if !strings.Contains(j, `"Outcome":"success"`) {
		t.Fatalf("the run did not end in success:\n%s", j)
	}
	argv, env := e.fake("codex")
	got := strings.Join(argv, " ")
	for _, want := range []string{"exec", "-s workspace-write", `approval_policy="never"`, "-C " + e.state, "-m gpt-6.2-astra", "model_reasoning_effort=xhigh", "--json -"} {
		if !strings.Contains(got, want) {
			t.Errorf("codex argv lacks %q: %s", want, got)
		}
	}
	if !hasEnv(env, "SUPER_WF_PARENT=claude:coord-c") || !hasEnv(env, "SUPER_WF_CLAIM="+claim) {
		t.Errorf("the child lacks its parent and claim entries")
	}
	for _, gone := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_MESSAGING_TOKEN"} {
		if envNamed(env, gone) {
			t.Errorf("the codex child inherited the coordinator's %s", gone)
		}
	}
	if !hasEnv(env, "CLAUDE_CONFIG_DIR="+e.coord["CLAUDE_CONFIG_DIR"]) {
		t.Errorf("the scrub removed a login home it must keep")
	}
	for _, want := range []string{`"headless_started"`, `"harness_checked"`, `"Available":true`, `"Version":"codex 9.9.9 (fake)"`, `"Harness":"codex"`} {
		if !strings.Contains(j, want) {
			t.Errorf("journal lacks %s", want)
		}
	}
	if strings.Contains(j, "child caller mismatch") {
		t.Errorf("a correct headless child was noted as a caller mismatch:\n%s", j)
	}
}

// The primitive, Codex to Claude: the same through a codex coordinator,
// whose launch needs the Codex rule (the fake execpolicy allows it here).
func TestHeadlessPrimitiveCodexToClaude(t *testing.T) {
	e := newHeadlessE2E(t, "codex")
	e.setupCodexRule()
	e.must("run", "family-claude", "--run-id", "r1", "--workspace", e.ws)
	claim, line := e.offer("claude")
	// The preflight asked Codex about the command exactly as next printed
	// it, with the claim still a slot: a form the rule does not match
	// would have failed it.
	var checked []string
	if b, err := os.ReadFile(filepath.Join(e.fakes, "execpolicy.json")); err != nil || json.Unmarshal(b, &checked) != nil {
		t.Fatalf("the preflight never ran codex execpolicy check: %v", err)
	}
	if want := strings.Fields(strings.Replace(line, claim, "<claim>", 1)); !slices.Equal(checked, want) {
		t.Errorf("the preflight checked %q, not the printed command %q", checked, want)
	}
	// Run as printed from another directory: the child still starts in the
	// run's recorded workspace.
	words := strings.Fields(line)
	t.Chdir(t.TempDir())
	e.mustRaw(words[1:]...)
	j := e.waitJournal(`"run_ended"`)
	if !strings.Contains(j, `"Outcome":"success"`) {
		t.Fatalf("the run did not end in success:\n%s", j)
	}
	argv, env := e.fake("claude")
	if cwd := e.fakeCwd("claude"); cwd != e.ws {
		t.Errorf("the child started in %s, not the run's workspace %s", cwd, e.ws)
	}
	got := strings.Join(argv, " ")
	for _, want := range []string{"-p", "--model fable", "--effort high", "--session-id ", "--permission-mode dontAsk", "--add-dir " + e.state, "Write(/" + e.state + "/**)"} {
		if !strings.Contains(got, want) {
			t.Errorf("claude argv lacks %q: %s", want, got)
		}
	}
	if !hasEnv(env, "SUPER_WF_PARENT=codex:coord-x") {
		t.Errorf("the child lacks its parent entry")
	}
	for _, gone := range []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		if envNamed(env, gone) {
			t.Errorf("the claude child inherited the coordinator's %s", gone)
		}
	}
	if strings.Contains(j, "child caller mismatch") {
		t.Errorf("a correct headless child was noted as a caller mismatch:\n%s", j)
	}
}

// No silent fallback: the target harness is not logged in, so the unit is
// never offered to the coordinator's own harness; its attempt fails with
// class Unavailable naming the fix (and backs off, as that class does).
func TestHeadlessUnavailableNeverFallsBack(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	t.Setenv("WF_TEST_FAKE_LOGIN", "no")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws)
	out := e.must("next", "r1", "--timeout", "2s")
	if strings.Contains(out, "Agent(") || strings.Contains(out, "spawn r1") {
		t.Fatalf("an unavailable codex unit was offered:\n%s", out)
	}
	j := e.journal()
	for _, want := range []string{`"Available":false`, "codex is not logged in", `"ClassName":"unavailable"`, "harness codex is unavailable", "fix: log in to codex"} {
		if !strings.Contains(j, want) {
			t.Errorf("journal lacks %s:\n%s", want, j)
		}
	}
}

// A Codex coordinator without the rule: the codex execpolicy check says no,
// so the claude target is unavailable, with the setup command as its fix.
func TestHeadlessCodexCoordinatorNeedsTheRule(t *testing.T) {
	e := newHeadlessE2E(t, "codex")
	e.must("run", "family-claude", "--run-id", "r1", "--workspace", e.ws)
	e.must("next", "r1", "--timeout", "2s")
	j := e.journal()
	for _, want := range []string{`"Available":false`, "no Codex rule lets the spawn verb run outside", "setup-codex-rules"} {
		if !strings.Contains(j, want) {
			t.Errorf("journal lacks %q", want)
		}
	}
}

// A headless child that exits after claiming, with no done: the recovery
// offer comes at once, quotes its stderr, and offers no resume (its
// coordinator cannot message a separate process).
func TestHeadlessChildExitWithoutDoneOffersRecovery(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	t.Setenv("WF_TEST_FAKE_MODE", "exit")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws)
	claim, _ := e.offer("codex")
	e.must("spawn", "r1", "--claim", claim)
	deadline := time.Now().Add(60 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = e.must("next", "r1", "--timeout", "1s")
		if strings.Contains(out, "exited with no done or failed") {
			break
		}
	}
	if !strings.Contains(out, "the headless codex child of clAsk#1") || !strings.Contains(out, "fake: leaving without done") {
		t.Fatalf("no recovery offer quoting the child's stderr:\n%s", out)
	}
	j := e.journal()
	if !strings.Contains(j, `"recovery_offered"`) || strings.Contains(j, `"Resume":"`) {
		t.Errorf("want a recovery offer with no resume line:\n%s", j)
	}
}

// spawn refuses a second start while the child runs, a caller that is
// itself a headless child, and a unit of the coordinator's own harness.
func TestSpawnRefusals(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	t.Setenv("WF_TEST_FAKE_MODE", "hang")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws)
	claim, _ := e.offer("codex")
	e.must("spawn", "r1", "--claim", claim)
	t.Cleanup(func() { killStarted(e.journal()) })
	if code, _, errs := e.call("spawn", "r1", "--claim", claim); code == 0 || !strings.Contains(errs, "already running") {
		t.Errorf("a second spawn was not refused (%d): %s", code, errs)
	}
	e.coord["SUPER_WF_PARENT"] = "claude:elsewhere"
	if code, _, errs := e.call("spawn", "r1", "--claim", claim); code == 0 || !strings.Contains(errs, "child") {
		t.Errorf("a headless child's spawn was not refused (%d): %s", code, errs)
	}
	delete(e.coord, "SUPER_WF_PARENT")
	// Losing the unit kills its child.
	e.must("stop", "r1", "--reason", "test over")
}

// status shows a unit's child harness, its pid, and why a child that never
// fetched its prompt did not start; and each child harness's preflight.
func TestHeadlessStatusShowsChildHarness(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	t.Setenv("WF_TEST_FAKE_MODE", "exit-before")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws)
	claim, _ := e.offer("codex")
	e.must("spawn", "r1", "--claim", claim)
	deadline := time.Now().Add(30 * time.Second)
	var st string
	for time.Now().Before(deadline) {
		st = e.must("status", "r1", "--full")
		if strings.Contains(st, "did not start: ") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, want := range []string{"on codex (pid ", "did not start: fake: leaving before the prompt", "children on codex: available (codex 9.9.9 (fake))"} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	var rs wf.RunStatus
	if err := json.Unmarshal([]byte(e.must("status", "r1", "--json")), &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs.Children) != 1 || rs.Children[0].Harness != "codex" || len(rs.Units) == 0 || rs.Units[0].Harness != "codex" || rs.Units[0].Pid == 0 {
		t.Errorf("status --json: children %+v, units %+v", rs.Children, rs.Units)
	}
}

// The child harness's tier table is journaled with each segment and holds
// across a stop and an operator's resume: a resume's --model-map key
// qualified with the child harness changes it, and a resume without one
// carries it on.
func TestChildTablesSurviveResume(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	e.must("run", "family-codex", "--run-id", "r1", "--workspace", e.ws, "--model-map", "codex.top=gpt-x,codex.top.effort=high")
	e.must("next", "r1", "--timeout", "1s")
	operator := func(args ...string) {
		t.Helper()
		var out, errb bytes.Buffer
		if code := wf.CLIWithEnv(e.cfg, append(args, "--state-dir", e.state), &out, &errb, func(string) string { return "" }); code != 0 {
			t.Fatalf("%v exited %d: %s%s", args, code, out.String(), errb.String())
		}
	}
	tables := func() []string {
		var out []string
		for _, line := range strings.Split(e.journal(), "\n") {
			var r struct {
				KindName string
				Data     struct {
					Models *struct {
						Children map[string]struct{ Models, Efforts map[string]string }
					}
				}
			}
			if json.Unmarshal([]byte(line), &r) == nil && r.KindName == "coordinator" && r.Data.Models != nil {
				c := r.Data.Models.Children["codex"]
				out = append(out, c.Models["top"]+"/"+c.Efforts["top"])
			}
		}
		return out
	}
	operator("stop", "r1", "--reason", "switch models")
	operator("resume", "r1", "--operator", "op", "--reason", "new model", "--model-map", "codex.top=gpt-y")
	e.must("next", "r1", "--timeout", "1s")
	operator("stop", "r1", "--reason", "again")
	operator("resume", "r1", "--operator", "op", "--reason", "carry")
	e.must("next", "r1", "--timeout", "1s")
	if got := strings.Join(tables(), " "); got != "gpt-x/high gpt-y/xhigh gpt-y/xhigh" {
		t.Errorf("child tables per segment: %s, want gpt-x/high gpt-y/xhigh gpt-y/xhigh", got)
	}
	if got := e.handedModels(); !strings.HasPrefix(got, "gpt-x") || !strings.HasSuffix(got, "gpt-y") {
		t.Errorf("hand-outs spawned with %s: want gpt-x first, gpt-y after the resume", got)
	}
}

// FamilyOther is a convenience on the primitive: claude's other family is
// codex by default, run --family changes it, and a coordinator with no other
// family fails the unit as unavailable rather than running it at home.
func TestFamilyOtherResolves(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	e.must("run", "family-other", "--run-id", "r1", "--workspace", e.ws)
	_, out := e.offer("codex")
	if !strings.Contains(out, "spawn r1") {
		t.Fatalf("no spawn offer:\n%s", out)
	}
	if got := e.handedModels(); got != "gpt-6.2-astra" {
		t.Errorf("the other family's strong tier spawned with %q, want codex's gpt-6.2-astra", got)
	}
	e2 := newHeadlessE2E(t, "claude")
	e2.must("run", "family-other", "--run-id", "r1", "--workspace", e2.ws, "--family", "claude=claude")
	out = e2.must("next", "r1", "--timeout", "1s")
	if strings.Contains(out, "Agent(") || strings.Contains(out, "spawn r1") {
		t.Fatalf("a unit with no other family was offered:\n%s", out)
	}
	if j := e2.journal(); !strings.Contains(j, "coordinator harness claude has none") || !strings.Contains(j, `"ClassName":"unavailable"`) {
		t.Errorf("want an unavailable failure naming the missing family:\n%s", j)
	}
}

// setup-codex-rules prints the one rule for the spawn verb's command, and
// with --write appends it to the Codex home's rules file once.
func TestSetupCodexRules(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"CODEX_HOME": home, "HOME": t.TempDir()}
	run := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := wf.CLIWithEnv(familyConfig(), args, &out, &errb, func(k string) string { return env[k] }); code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, errb.String())
		}
		return out.String()
	}
	bin, _ := os.Executable()
	rule := fmt.Sprintf(`prefix_rule(pattern = [%q,"spawn"], decision = "allow"`, bin)
	file := filepath.Join(home, "rules", "super-workflow.rules")
	if out := run("setup-codex-rules"); !strings.Contains(out, rule) || !strings.Contains(out, file) {
		t.Errorf("print: %s", out)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("printing wrote the file")
	}
	run("setup-codex-rules", "--write")
	run("setup-codex-rules", "--write")
	b, err := os.ReadFile(file)
	if err != nil || strings.Count(string(b), rule) != 1 {
		t.Errorf("the rules file holds the rule %d times:\n%s", strings.Count(string(b), rule), b)
	}
}

// Lint: a named family must be a canonical harness with a headless launch.
func TestFamilyLint(t *testing.T) {
	for _, c := range []struct {
		f    wf.Family
		want string
	}{
		{wf.FamilyNamed("pi"), "has no headless launch"},
		{wf.Family("Codex"), "not a canonical harness name"},
		{wf.Family(""), "empty family hint"},
	} {
		d := clFamilyDef("lint", c.f, wf.TierTop)
		var got []string
		for _, p := range d.Lint() {
			got = append(got, p.String())
		}
		if !strings.Contains(strings.Join(got, "\n"), c.want) {
			t.Errorf("%q: lint %v, want %q", c.f, got, c.want)
		}
	}
	if ps := clFamilyDef("ok", wf.FamilyNamed("Claude-Code"), wf.TierTop).Lint(); len(ps) != 0 {
		t.Errorf("an alias names its canonical harness: %v", ps)
	}
}

// A family naming the coordinator's own harness is an ordinary subagent at
// that harness's model for the tier.
func TestFamilyNamingTheCoordinatorKeepsItsTier(t *testing.T) {
	e := newHeadlessE2E(t, "claude")
	e.must("run", "family-claude", "--run-id", "r1", "--workspace", e.ws)
	out := e.must("next", "r1", "--timeout", "1s")
	if !strings.Contains(out, "Agent(") || !strings.Contains(out, `model="fable"`) || strings.Contains(out, "spawn r1") {
		t.Fatalf("want an ordinary subagent on fable:\n%s", out)
	}
}

// fakeProc is a HeadlessProcess whose child runs until the test says it
// exited, and which records kills.
type fakeProc struct {
	live    bool
	kills   int
	sandbox string // Sandboxed's answer: pid 1's name inside a per-command sandbox
}

func (p *fakeProc) Start(_, _ []string, _, _, _, _ string) (int, uint64, error) {
	p.live = true
	return 4242, 1, nil
}
func (p *fakeProc) Live(int, uint64) bool { return p.live }
func (p *fakeProc) Kill(int, uint64) error {
	p.kills++
	return nil
}
func (p *fakeProc) Sandboxed() string { return p.sandbox }

// A spawn that finds itself inside Codex's sandbox: when the segment's
// preflight found a rule for the printed command, the refusal says the
// command was most likely run chained or altered, not to install the rule
// again; with no rule checked (no preflight) it says to install it.
func TestHeadlessSpawnSandboxedRefusal(t *testing.T) {
	for _, tc := range []struct {
		ruled      bool
		want, lack string
	}{
		{false, "install the Codex rule", "chained"},
		{true, "most likely run chained or altered", "install the Codex rule"},
	} {
		p := &fakeProc{sandbox: "codex"}
		d := clFamilyDef("family-claude", wf.FamilyNamed("claude"), wf.TierTop)
		opts := wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "r1", StateRoot: t.TempDir(), HeadlessProcess: p, Harness: "codex"}
		if tc.ruled {
			opts.HeadlessCheck = func(_, _ string, _ *wf.HeadlessConfig, _ string) wf.HarnessCheckedData {
				return wf.HarnessCheckedData{Available: true}
			}
		}
		e := wf.NewEngine(wf.MemJournal(), wf.NewAdapters(), opts).(*wf.Engine)
		id, err := e.Run(d, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		co := wf.Coordinator{Harness: "codex", Session: "s1"}
		ins, err := e.HandOutAs(id, 1, co)
		if err != nil || len(ins) != 1 || ins[0].Spawn == "" {
			t.Fatalf("ruled=%v: hand-out: %v %+v", tc.ruled, err, ins)
		}
		claim := strings.Fields(ins[0].Spawn[strings.Index(ins[0].Spawn, "--claim ")+8:])[0]
		_, err = e.Spawn(id, claim, co, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.lack) {
			t.Errorf("ruled=%v: refusal %v, want %q and not %q", tc.ruled, err, tc.want, tc.lack)
		}
		if p.live {
			t.Errorf("ruled=%v: a sandboxed spawn started a child", tc.ruled)
		}
	}
}

// A headless child that is still starting is not passed over, however long
// the unstarted grace; one that runs past the fetch bound without fetching
// is stopped, and its unit is offered again once it is gone.
func TestHeadlessFetchBoundAndNoReofferWhileStarting(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	p := &fakeProc{}
	d := clFamilyDef("family-codex", wf.FamilyNamed("codex"), wf.TierTop)
	e := wf.NewEngine(wf.MemJournal(), wf.NewAdapters(), wf.EngineOptions{
		Defs: []*wf.Def{d}, RunID: "r1", Clock: func() time.Time { return now }, StateRoot: t.TempDir(),
		HeadlessProcess: p, Harness: "claude",
		HeadlessCheck: func(_, _ string, _ *wf.HeadlessConfig, spawn string) wf.HarnessCheckedData {
			if !strings.Contains(spawn, " spawn ") || !strings.Contains(spawn, "--claim <claim>") || strings.Contains(spawn, "&&") {
				t.Errorf("the preflight was not given the spawn command as next prints it: %q", spawn)
			}
			return wf.HarnessCheckedData{Available: true}
		},
	}).(*wf.Engine)
	id, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	co := wf.Coordinator{Harness: "claude", Session: "s1"}
	ins, err := e.HandOutAs(id, 1, co)
	if err != nil || len(ins) != 1 || ins[0].Spawn == "" {
		t.Fatalf("hand-out: %v %+v", err, ins)
	}
	tok := ins[0].Spawn[strings.Index(ins[0].Spawn, "--claim ")+8:]
	tok = strings.Fields(tok)[0]
	if _, err := e.Spawn(id, tok, co, nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(110 * time.Second)
	if again, _ := e.HandOutAs(id, 1, co); len(again) != 0 || p.kills != 0 {
		t.Fatalf("within the bound: offers %d, kills %d", len(again), p.kills)
	}
	now = now.Add(15 * time.Second) // past the fetch bound
	if _, err := e.HandOutAs(id, 1, co); err != nil {
		t.Fatal(err)
	}
	if p.kills == 0 {
		t.Fatal("a child past the fetch bound was not stopped")
	}
	now = now.Add(10 * time.Minute) // past the unstarted grace, still running
	if again, _ := e.HandOutAs(id, 1, co); len(again) != 0 {
		t.Fatalf("a unit was re-offered while its child still ran: %+v", again)
	}
	p.live = false
	again, err := e.HandOutAs(id, 1, co)
	if err != nil || len(again) != 1 || again[0].Spawn == "" || !again[0].NotStarted {
		t.Fatalf("once the child is gone, want the unit offered again as not started: %v %+v", err, again)
	}
}
