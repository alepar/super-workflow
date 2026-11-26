package wf_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// Tests of the bootstrap loop and its command edges (bootstrap-and-report-text
// plan, Part B).

// plainAgentDef is one agent with no dispatch hints, then a terminal.
func plainAgentDef(name string) *wf.Def {
	d := wf.New(name)
	a := wf.NewAgent[clAsk, clAnswer](d)
	done := wf.NewTerminal[clDone](d, wf.Success)
	gaveUp := wf.NewTerminal[clGaveUp](d, wf.Stopped)
	d.OnFailure(wf.Blocked, gaveUp)
	a.Spec(wf.PromptText("Say hello."), wf.Retry[clAsk](wf.Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(clAnswer, wf.Ctx) wf.Edge[clAsk] { return next }, next)
	return d
}

func startPlain(t *testing.T, def *wf.Def, harness string) string {
	t.Helper()
	if harness == "codex" { // the environment must not contradict --harness
		t.Setenv("CLAUDECODE", "")
		t.Setenv("CODEX_THREAD_ID", "coord")
	}
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1", "--harness", harness)
	return state
}

func handOne(t *testing.T, def *wf.Def, state string) wf.Instruction {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 = %v, %v", ins, err)
	}
	return ins[0]
}

// B3: every agent instruction names its agent type: general-purpose on
// Claude Code, none on Codex; a definition's own type wins.
func TestInstructionAgentTypePerHarness(t *testing.T) {
	for _, tc := range []struct {
		harness, want string
		def           *wf.Def
	}{
		{"claude", "general-purpose", plainAgentDef("plain-claude")},
		{"codex", "", plainAgentDef("plain-codex")},
		{"claude", "reviewer", clAgentDef("typed-claude")},
	} {
		t.Run(tc.harness+"/"+tc.def.Name(), func(t *testing.T) {
			in := handOne(t, tc.def, startPlain(t, tc.def, tc.harness))
			if in.Dispatch.AgentType != tc.want {
				t.Errorf("agentType %q, want %q", in.Dispatch.AgentType, tc.want)
			}
		})
	}
}

// B4: usage attached by the coordinator after the child's done is journaled
// for that unit and shows in status; a repeat is idempotent, a conflict refused.
func TestStartedAttachesUsageAfterDone(t *testing.T) {
	def := plainAgentDef("usage-late")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	if code, _, _ := singleCLI(def, "wf", "dev", "started", in.Name, "--usage", "tokens=5", "--state-dir", state); code == 0 {
		t.Errorf("usage on a live unit was accepted; it attaches after the child ends")
	}
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", in.Name, "--state-dir", state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--usage", "tokens=1234", "--state-dir", state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--usage", "tokens=1234", "--state-dir", state) // idempotent
	if code, _, _ := singleCLI(def, "wf", "dev", "started", in.Name, "--usage", "tokens=99", "--state-dir", state); code == 0 {
		t.Errorf("conflicting usage was accepted")
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	if st.Tokens != 1234 {
		t.Errorf("status tokens %d, want 1234", st.Tokens)
	}
	if out := mustSingle(t, def, "wf", "status", "--state-dir", state); !strings.Contains(out, "tokens: 1234\n") {
		t.Errorf("status text lacks the tokens line:\n%s", out)
	}
}

// B5: --help on a verb needs no run, and next on an ended run prints only the
// ended record.
func TestVerbHelpNeedsNoRun(t *testing.T) {
	def := plainAgentDef("help-only")
	empty := filepath.Join(t.TempDir(), "none")
	for _, verb := range []string{"next", "done", "status", "heartbeat", "stop"} {
		code, out, er := singleCLI(def, "wf", verb, "--help", "--state-dir", empty)
		if code != 0 || !strings.Contains(out+er, "wf "+verb) || strings.Contains(out+er, "no run in") {
			t.Errorf("wf %s --help: exit %d\n%s%s", verb, code, out, er)
		}
	}
	if code, out, er := singleCLI(def, "next", "--help", "--state-dir", empty); code != 0 || !strings.Contains(out, "usage: super next") {
		t.Errorf("next --help: exit %d\n%s%s", code, out, er)
	}
	// The multi-registration form: help with no run in front.
	if code, out, er := cliCall(clConfig(), "dev", "started", "--help"); code != 0 || !strings.Contains(out, "started <run id> <name>") {
		t.Errorf("started --help: exit %d\n%s%s", code, out, er)
	}
}

// A flag value that reads like help is the value, never a help request: the
// failure is journaled.
func TestHelpLookalikeValueIsNotHelp(t *testing.T) {
	def := plainAgentDef("help-value")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	code, out, er := singleCLI(def, "wf", "failed", in.Name, "--reason", "-h", "--state-dir", state)
	if code != 0 || strings.Contains(out, "usage:") {
		t.Fatalf("wf failed --reason -h: exit %d\n%s%s", code, out, er)
	}
	if j := mustSingle(t, def, "wf", "journal", "--state-dir", state); !strings.Contains(j, "abandoned: -h") {
		t.Errorf("the failure with reason -h was not journaled")
	}
}

// Usage the child passed with done is the unit's usage: the same count again
// is a no-op, another is refused.
func TestStartedUsageAfterDoneUsage(t *testing.T) {
	def := plainAgentDef("usage-both")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", in.Name, "--usage", "tokens=5", "--state-dir", state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--usage", "tokens=5", "--state-dir", state)
	if code, _, _ := singleCLI(def, "wf", "dev", "started", in.Name, "--usage", "tokens=6", "--state-dir", state); code == 0 {
		t.Errorf("a second, different count was accepted")
	}
	if code, _, _ := singleCLI(def, "wf", "dev", "started", in.Name, "--usage", "tokens=5", "--session", "s", "--agent", "a", "--state-dir", state); code != 2 {
		t.Errorf("--usage with claim flags exited %d, want 2", code)
	}
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil || st.Tokens != 5 {
		t.Errorf("status tokens %d (%v), want 5", st.Tokens, err)
	}
}

func TestNextOnEndedRunPrintsOnlyTheEndedRecord(t *testing.T) {
	def := plainAgentDef("ended-next")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "s", "--agent", "a", "--state-dir", state)
	writeAllocated(t, in.Output, `{"OK":true}`)
	mustSingle(t, def, "wf", "done", in.Name, "--state-dir", state)
	code, out, er := singleCLI(def, "next", "--state-dir", state)
	if code != 7 {
		t.Fatalf("next on an ended run exited %d, want 7: %s", code, er)
	}
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(out), &ins); err != nil || len(ins) != 1 || ins[0].Kind != wf.InstructionEnded || ins[0].Terminal == "" {
		t.Fatalf("next on an ended run printed %q (%v), want one ended instruction", out, err)
	}
	if strings.Contains(out, "loop:") {
		t.Errorf("an ended run printed the loop:\n%s", out)
	}
}

// B6: allocated paths are literal: no percent-encoding to decode, and a
// shell round-trip leaves them unchanged.
func TestAllocatedPathIsLiteral(t *testing.T) {
	e := newCLIEnv(t)
	d := wf.New("literal-allocated")
	keys := []string{"a#b", "a-b"} // slug alike: only the hash keeps them apart
	m := wf.NewMap[chFan, chItem, string, chRes](d, chItems(0, 1), func(i chItem) string { return keys[i.N] }, chChild())
	ok := wf.NewTerminal[chMerged](d, wf.Success)
	bad := wf.NewTerminal[chPartial](d, wf.Stopped)
	m.Parallel(2).Join(wf.AllSettled(), m.To(ok, "met"), m.To(bad, "partial"))
	cfg := clConfig()
	cfg.Defs = append(cfg.Defs, wf.Registration{Def: d})
	must := func(args ...string) string {
		t.Helper()
		code, out, er := cliCall(cfg, args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, er)
		}
		return out
	}
	run := strings.TrimSpace(must("run", d.Name(), "--run-id", "lit", "--state-dir", e.state, "--workspace", e.ws))
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(must("next", run, "2", "--state-dir", e.state, "--json")), &ins); err != nil || len(ins) != 2 {
		t.Fatalf("next 2: %v %+v", err, ins)
	}
	if ins[0].Output == ins[1].Output {
		t.Fatalf("names that slug alike share an output: %s", ins[0].Output)
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	for _, in := range ins {
		if !strings.ContainsAny(in.Name, "#./") {
			t.Fatalf("test unit name %q lacks #, . or /", in.Name)
		}
		base := filepath.Base(in.Output)
		if filepath.Dir(in.Output) != filepath.Join(e.state, "runs", run, "out") || !safe.MatchString(base) || strings.Contains(in.Output, "%") {
			t.Errorf("output %s is not a literal file under out/", in.Output)
		}
		// An unquoted shell word round-trips unchanged.
		got, err := exec.Command("sh", "-c", "printf %s "+in.Output).Output()
		if err != nil || string(got) != in.Output {
			t.Errorf("shell round-trip of %s gave %q (%v)", in.Output, got, err)
		}
		must("dev", "started", run, in.Name, "--session", "s", "--agent", "a"+in.Name, "--state-dir", e.state)
		if p := must("prompt", run, in.Name, "--state-dir", e.state); !strings.Contains(p, in.Output) {
			t.Errorf("prompt does not name %s:\n%s", in.Output, p)
		}
		if err := os.WriteFile(in.Output, []byte(`{"N":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
		must("done", run, in.Name, "--state-dir", e.state)
	}
}

// B1: an expired hand-out's re-offerable event is shown once to a follower
// that carries the cursor, not on every wait until next re-offers it.
func TestWaitSinceDoesNotRepeatExpiredHandOut(t *testing.T) {
	d := lsDef("wt-window-once", true)
	w := newWaiter(t, d, wf.EngineOptions{HandOutWindow: 150 * time.Millisecond})
	run := w.start(d)
	if ins, err := w.e.HandOut(run, 1); err != nil || len(ins) != 1 {
		t.Fatalf("HandOut = %+v, %v", ins, err)
	}
	res := w.wait(run, -1, 10*time.Second)
	oneEvent(t, res, wf.EventWindow)
	oneEvent(t, w.wait(run, res.Seq, 200*time.Millisecond), wf.EventTimeout)
}

// B1: a wait that times out before its first cycle still prints a cursor
// that skips what came before, never seq 0.
func TestWaitTimeoutCarriesACursor(t *testing.T) {
	h, run := eiHaltWorld(t, nil)
	res, err := h.e.Wait(run, -1, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq == 0 {
		t.Fatalf("an immediate timeout printed seq 0: %+v", res)
	}
}

// C1, C2c: the standing text is the rules for the run and the loop, under
// 3 KB on every harness (2 KB before the rules; Codex, the tightest tool
// output, holds 10 KiB), and its last line is the reporting rule. The rules
// carry the user's unit-fanout sentences verbatim.
func TestBootstrapIsTheRulesAndTheLoopUnderThreeKB(t *testing.T) {
	const workspace = "/example/projects/workflow-client"
	rules := trackerFreeAgentRules + strings.TrimPrefix(wf.RulesForThisRun, wf.RulesForAnyAgent)
	for _, h := range []string{"agy", "claude", "codex", "generic", "hermes", "pi", "unchained-test"} {
		// The longest Describe phrase a definition may set.
		work := strings.Repeat("w", 128)
		out := wf.Bootstrap(wf.RunStatus{Run: "superdesign-f1f1e04f1b22", Harness: h, Detach: wf.DetachNone, Workspace: workspace, Work: work}, workspace+"/.superpowers/bin/super", workspace+"/.superpowers/state")
		if len(out) >= 3072 {
			t.Errorf("%s bootstrap is %d bytes, want under 3 KB:\n%s", h, len(out), out)
		}
		for _, want := range []string{
			rules,
			"When you hand out this run's units, start each sub-agent with your own harness's sub-agent tool. Do not start another agent CLI (codex, claude, hermes) to do a unit, except through the engine's child dispatch (the call or the spawn command next prints), unless the workflow or the person who started the run explicitly asks you to.",
			"a coordinator goes on with next, and reports to the person who started the run when a command asks for a person.",
			"End your turn only if your child is running, the run ends, or next says to wait or stop.",
		} {
			if strings.Count(out, want) != 1 {
				t.Errorf("%s bootstrap does not state %q once:\n%s", h, want, out)
			}
		}
		if strings.Index(out, rules) > strings.Index(out, "loop:") {
			t.Errorf("%s bootstrap states the rules after the loop:\n%s", h, out)
		}
		if strings.Contains(out, "--wait") {
			t.Errorf("%s bootstrap still names --wait (next always blocks):\n%s", h, out)
		}
		for _, want := range []string{"as many as your harness allows", "wf next --state-dir", "harness: " + h} {
			if !strings.Contains(out, want) {
				t.Errorf("%s bootstrap lacks %q:\n%s", h, want, out)
			}
		}
		// Codex's exec_command yields with next still running: keep that call.
		if got := strings.Contains(out, "wait on that same session with write_stdin"); got != (h == "codex") {
			t.Errorf("%s bootstrap: Codex wait advice present = %v:\n%s", h, got, out)
		}
		if got := strings.Contains(out, "smart approvals may deny a delegated child without prompting"); got != (h == "hermes") {
			t.Errorf("%s bootstrap: Hermes approval advice present = %v:\n%s", h, got, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if last := lines[len(lines)-1]; !strings.HasPrefix(last, "Report when next prints the ended instruction or asks for a person") {
			t.Errorf("%s bootstrap's last line is %q", h, last)
		}
		for _, gone := range []string{"wf wait", "wf started", "dev started", "started --", "--since", "session_meta"} {
			if strings.Contains(out, gone) {
				t.Errorf("%s bootstrap still carries %q:\n%s", h, gone, out)
			}
		}
	}
}

const idleTurnReminder = "continue driving: if no child you started is running, run next before ending your turn.\n"

// C2: each event prints its rule; a unit is one line in the caller's own
// spawn form carrying the one-line brief.
func TestNextWaitEventsCarryTheirRules(t *testing.T) {
	l := func(verb string, args ...string) string { return "wf " + verb + " " + strings.Join(args, " ") }
	unit := wf.Instruction{Kind: wf.InstructionAgent, Name: "a#1", Brief: "You are a child agent for one unit of work. Run: wf prompt --claim t\nThen do what that prints.",
		Dispatch: wf.DispatchHints{Model: "opus", Effort: "high", AgentType: "general-purpose"}}
	claude := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{unit}}, "claude", l)
	// I1: the offer addresses the coordinator, the brief between the lines
	// the child, and no line begins with a command.
	if want := `start a child agent for a#1 with Agent(subagent_type="general-purpose", model="opus", effort="high", prompt=<the text between the lines>). Its whole task is the text between the ----- lines, passed verbatim with its command first; the unit is a child's to run, which keeps its work out of your context:` + "\n-----\nYou are a child agent for one unit of work. Run: wf prompt --claim t\nThen do what that prints.\n-----\n"; !strings.HasPrefix(claude, want) || strings.TrimPrefix(claude, want) != idleTurnReminder {
		t.Errorf("claude offer = %q, want %q followed by the coordinator's reminder", claude, want)
	}
	// E3: with n left out the offer says how many to start and why.
	limited := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{unit}, Offer: &wf.OfferView{Limit: 6, Source: "max_threads in /r/.codex/config.toml", Busy: 5}}, "codex", l)
	if !strings.HasPrefix(limited, "offered 1: your harness runs 6 subagents at once (max_threads in /r/.codex/config.toml), 5 of yours running or starting") {
		t.Errorf("limited offer = %q", limited)
	}
	if open := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{unit}, Offer: &wf.OfferView{}}, "claude", l); !strings.HasPrefix(open, wf.NextStartAll+"\n") {
		t.Errorf("open offer = %q", open)
	}
	// A stall is the same question on every harness (rule 5): look with your
	// own harness, without stopping the child, and answer.
	stalled := wf.NextResult{Stalled: []wf.StalledView{{Name: "a#1"}}}
	for _, h := range []string{"claude", "codex", "hermes", "pi", "nonesuch"} {
		if got := wf.RenderNext(stalled, h, l); !strings.Contains(got, "Look with your own harness, without stopping it") || !strings.Contains(got, "wf heartbeat died a#1") {
			t.Errorf("%s stall = %q", h, got)
		}
	}
	// A run stepped down to detach none mid-run: its advice prints once.
	adv := []wf.Instruction{unit, unit}
	adv[0].Advice, adv[1].Advice = "detach: none", "detach: none"
	if got := wf.RenderNext(wf.NextResult{Instructions: adv}, "claude", l); strings.Count(got, "detach: none\n") != 1 {
		t.Errorf("advice printed %d times in %q, want once", strings.Count(got, "detach: none\n"), got)
	}
	unit.Dispatch.AgentType = ""
	if codex := wf.RenderNext(wf.NextResult{Instructions: []wf.Instruction{unit}}, "codex", l); !strings.Contains(codex, `with spawn_agent(message=<the text between the lines>, model="opus", reasoning_effort="high", fork_turns="none"). Its whole task`) {
		t.Errorf("codex unit line = %q", codex)
	}
	for _, c := range []struct {
		r    wf.NextResult
		want []string
	}{
		{wf.NextResult{Timeout: true}, []string{wf.NextNothing}},
		{wf.NextResult{Stalled: []wf.StalledView{{Name: "a#1", Since: time.Unix(0, 0)}}}, []string{"stalled a#1", "wf heartbeat active a#1", "wf heartbeat died a#1"}},
		{wf.NextResult{Blocked: &wf.BlockedView{State: "s", Child: "c", Reason: "retries exhausted"}}, []string{"blocked s: retries exhausted", "report this cause and end the session", "wf retry --child c --stop"}},
		{wf.NextResult{Instructions: []wf.Instruction{{Kind: wf.InstructionEnded, Terminal: "done", Outcome: wf.Success}}}, []string{"ended: done, outcome success", "final report now, quoting this line"}},
		{wf.NextResult{Instructions: []wf.Instruction{{Kind: wf.InstructionStopped, Prompt: "The run is stopped"}}}, []string{"stopped:", "The run is stopped"}},
	} {
		got := wf.RenderNext(c.r, "claude", l)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("render lacks %q:\n%s", w, got)
			}
		}
	}
}

// C3, G2: the removed verbs are refused, each naming what replaced it, so a
// remembered command fails with its fix; a bare next asks for n and names
// the loop.
func TestRemovedVerbsAreRefused(t *testing.T) {
	def := plainAgentDef("removed-verbs")
	state := startPlain(t, def, "claude")
	for verb, now := range map[string]string{"submit": "wf done", "fail": "wf failed", "wait": "wf next;"} {
		if code, _, er := singleCLI(def, "wf", verb, "--state-dir", state); code != 2 || !strings.Contains(er, "removed") || !strings.Contains(er, now) {
			t.Errorf("wf %s: exit %d %q, want a refusal naming %s", verb, code, er, now)
		}
	}
	if code, _, er := singleCLI(def, "wf", "next", "--state-dir", state, "--json"); code != 2 || !strings.Contains(er, "<n>") || !strings.Contains(er, "the loop is next") {
		t.Errorf("wf next --json without n: exit %d %q, want a usage error naming <n> and the loop", code, er)
	}
	if code, _, er := cliCall(wf.MainConfig{}, "wait"); code != 2 || !strings.Contains(er, "workflow next;") || strings.Contains(er, " wf next") {
		t.Errorf("workflow wait: exit %d %q, want the refusal naming workflow next", code, er)
	}
	if code, _, er := singleCLI(def, "wait"); code != 2 || !strings.Contains(er, "super wf next;") {
		t.Errorf("super wait: exit %d %q, want the refusal naming super wf next", code, er)
	}
	if code, _, er := singleCLI(def, "wf", "dev", "wait", "--state-dir", state); code != 2 || strings.Contains(er, "removed:") {
		t.Errorf("super wf dev wait: exit %d %q, want an unknown verb, not the removed wait", code, er)
	}
}

// G2: the bootstrap's first line is the protocol version and says this text
// replaces anything remembered about the tool; R5: the child's page says the
// page wins over the task text it was started with.
func TestThePrintedTextOutranksMemory(t *testing.T) {
	boot := wf.Bootstrap(wf.RunStatus{Run: "r1", Harness: "claude"}, "./super", "/state")
	if first, _, _ := strings.Cut(boot, "\n"); !strings.HasPrefix(first, wf.ProtocolVersion+": ") || !strings.Contains(first, "replaces anything you remember or were told elsewhere about this tool") {
		t.Errorf("bootstrap's first line = %q", first)
	}
	def := plainAgentDef("page-wins")
	state := startPlain(t, def, "claude")
	in := handOne(t, def, state)
	page := mustSingle(t, def, "wf", "prompt", "--state-dir", state, "--claim", claimToken(t, in))
	if !strings.Contains(page, "where your task text disagrees") || !strings.Contains(page, "this page wins") || !strings.Contains(page, "- Work in ") {
		t.Errorf("the page does not say it outranks the task text:\n%s", page)
	}
}

// A stop is cleared by an operator, in any harness, never by the run's own
// coordinator session (guard-audit class 2; formerly G1, "only a person"):
// resume from the coordinator's session is refused and journals nothing;
// from another agent session that names itself and why, it clears the stop.
// The stopped instruction names no command to run.
func TestTheRunsCoordinatorDoesNotClearItsOwnStop(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coord-session")
	def := plainAgentDef("operator-clears")
	state := startPlain(t, def, "claude")
	handOne(t, def, state)
	mustSingle(t, def, "wf", "stop", "--state-dir", state, "--reason", "operator")
	code, out, _ := singleCLI(def, "wf", "next", "1", "--timeout", "100ms", "--state-dir", state)
	if code != 7 || !strings.Contains(out, "run nothing further") || strings.Contains(out, "resume") {
		t.Errorf("stopped next = %d %q, want the stopped text naming no command", code, out)
	}
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "me", "--reason", "go on"); code != 4 || !strings.Contains(er, "does not clear its own stop") || strings.Contains(er, "CLAUDE") || strings.Contains(er, "env -u") {
		t.Fatalf("resume from the run's coordinator session = %d %q, want refused, naming no variable and no way around it", code, er)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "operator-session") // another agent session, same harness
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state); code != 4 || !strings.Contains(er, "--operator <name> --reason <why>") {
		t.Fatalf("an unnamed resume = %d %q, want refused naming --operator and --reason", code, er)
	}
	if code, _, er := singleCLI(def, "wf", "resume", "--state-dir", state, "--operator", "the run supervisor", "--reason", "lane repaired"); code != 0 {
		t.Fatalf("resume from an operator session = %d %s, want the stop cleared", code, er)
	}
}

// C1: next --wait returns at once with the units to spawn, and blocks to
// its timeout with the nothing-yet line when the run has nothing for the
// coordinator.
func TestNextWaitThroughCLI(t *testing.T) {
	def := plainAgentDef("next-wait")
	state := startPlain(t, def, "claude")
	out := mustSingle(t, def, "wf", "next", "2", "--state-dir", state)
	if !strings.HasPrefix(out, "start a child agent for ") || strings.Count(out, "start a child agent for ") != 1 || !strings.Contains(out, "wf prompt --claim") {
		t.Fatalf("next = %q, want one spawn line", out)
	}
	if !strings.HasSuffix(out, idleTurnReminder) {
		t.Fatalf("an offer with no running child needs the reminder after its brief: %q", out)
	}
	start := time.Now()
	out = mustSingle(t, def, "wf", "next", "2", "--timeout", "200ms", "--state-dir", state)
	if out != "nothing yet after 200ms: "+wf.NextNothing+"\n"+idleTurnReminder || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("next with nothing to do = %q after %s", out, time.Since(start))
	}
	// H2: the line says sleeping or polling in between gains nothing.
	if !strings.Contains(out, "sleeping or checking status in between gains nothing") {
		t.Errorf("the nothing-yet line does not say polling gains nothing: %q", out)
	}
	// H1: the earlier protocol's --wait still runs (it is what next does).
	if code, _, er := singleCLI(def, "wf", "next", "2", "--wait", "--timeout", "100ms", "--state-dir", state); code != 0 {
		t.Errorf("next --wait (remembered) = %d %s, want it accepted", code, er)
	}
	mustSingle(t, def, "wf", "dev", "started", "clAsk#1", "--state-dir", state, "--session", "coordinator", "--agent", "child")
	for _, args := range [][]string{
		{"wf", "next", "0", "--state-dir", state},
		{"wf", "next", "2", "--timeout", "100ms", "--state-dir", state},
	} {
		if out := mustSingle(t, def, args...); !strings.HasSuffix(out, idleTurnReminder) {
			t.Errorf("%v from a caller with no session = %q, want the conditional reminder", args, out)
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coordinator")
	out = mustSingle(t, def, "wf", "next", "2", "--timeout", "100ms", "--state-dir", state)
	if out != "nothing yet after 100ms: "+wf.NextNothing+"\n" {
		t.Fatalf("next with a running child = %q, want only the timeout line", out)
	}
}

// The state dir's env sidecar is for the run's scripts: it may unset the
// harness variables for them, and the caller's identity is still read from
// the environment the caller started with.
func TestCallerIdentityIsReadBeforeTheEnvSidecar(t *testing.T) {
	t.Setenv("CLAUDECODE", "1") // restored after the sidecar unsets it
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coord")
	def := clAgentDef("sidecar-identity")
	state := startPlain(t, def, "claude")
	if err := os.WriteFile(filepath.Join(state, "env"), []byte("-CLAUDECODE\n-CLAUDE_CODE_SESSION_ID\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustSingle(t, def, "wf", "next", "1", "--state-dir", state)
	if !strings.Contains(out, `Agent(subagent_type="reviewer", model="opus"`) {
		t.Fatalf("next under a scrubbing sidecar = %q, want the claude spawn line with the tier's model", out)
	}
	var ho wf.HandedOutData
	j := wf.FileJournal(state)
	unlock := lock(t, j, "r1")
	recs, err := j.Records("r1")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == wf.JournalHandedOut {
			_ = json.Unmarshal(r.Data, &ho)
		}
	}
	if ho.Session != "coord" || ho.Harness != "claude" {
		t.Fatalf("handed_out = %+v, want session coord under claude", ho)
	}
}

// H3, the plan's test: a coordinator that sleeps and polls status 20 times
// during a 40-unit run adds under 2 KB to its context from those calls: each
// poll from an agent's shell is one line that sends it back to next. A
// person's status --full still shows every unit.
func TestStatusPollsCostALine(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i + 1
	}
	def := fanDef("poll-forty", chItems(items...), 40, wf.AllSettled())
	state := startPlain(t, def, "claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coord") // the coordinator's own shell
	// All 40 handed out; those past one call's output limit come with the
	// next call ("more are ready").
	offered := 0
	for call := 0; call < 5 && offered < 40; call++ {
		offered += strings.Count(mustSingle(t, def, "wf", "next", "--state-dir", state), "start a child agent for ")
	}
	if offered != 40 {
		t.Fatalf("next offered %d units, want 40", offered)
	}
	total := 0
	for range 20 {
		out := mustSingle(t, def, "wf", "status", "--state-dir", state)
		if out != "running: 0 running, 40 handed out not started, 0 ready; coordinator: run next; subagent: carry on\n" {
			t.Fatalf("status from the coordinator = %q", out)
		}
		total += len(out)
	}
	if total >= 2048 {
		t.Errorf("20 status polls added %d bytes, want under 2 KB", total)
	}
	full := mustSingle(t, def, "wf", "status", "--full", "--state-dir", state)
	if n := unitRows(full, "chWork#1"); n != 40 {
		t.Errorf("status --full lists %d units, want 40:\n%.400s", n, full)
	}
	if js := mustSingle(t, def, "wf", "status", "--json", "--state-dir", state); !strings.HasPrefix(js, "{") || !strings.Contains(js, `"Handed": 40`) {
		t.Errorf("status --json from the coordinator is not the RunStatus:\n%.600s", js)
	}
	// A stopped run is not one to wait on: the whole view, from any shell.
	mustSingle(t, def, "wf", "stop", "--state-dir", state, "--reason", "operator")
	if out := mustSingle(t, def, "wf", "status", "--state-dir", state); unitRows(out, "chWork#1") != 40 {
		t.Errorf("status of a stopped run from the coordinator is not the whole view:\n%.600s", out)
	}
}

// unitRows counts the unit rows of a status view naming unit: the rows that
// start with a unit name, not the due lines that name it too.
func unitRows(view, unit string) int {
	n := 0
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, unit) && !strings.HasPrefix(line, "due: ") {
			n++
		}
	}
	return n
}

// H1: a session that already coordinates the run and calls the bootstrap
// verb again is polling: it gets its loop line and blocks as wf next does,
// never the bootstrap at once.
func TestBootstrapVerbBlocksForItsCoordinator(t *testing.T) {
	def := fanDef("again", chItems(1), 1, wf.AllSettled())
	state := startPlain(t, def, "claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coord")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "1000") // next waits half of it
	boot := mustSingle(t, def, "next", "--state-dir", state)
	if !strings.Contains(boot, "You are the coordinator") {
		t.Fatalf("first next is not the bootstrap:\n%s", boot)
	}
	_, loop, _ := strings.Cut(boot, "\nloop:\n")
	loop, _, _ = strings.Cut(loop, "\n")
	mustSingle(t, def, "wf", "next", "--state-dir", state) // takes the run; hands out the unit
	start := time.Now()
	out := mustSingle(t, def, "next", "--state-dir", state)
	// The label and the loop command are lines of their own: the command
	// line is the bootstrap's loop line, whole.
	if !strings.HasPrefix(out, "you already coordinate run r1; your loop is:\n"+loop+"\n") || !strings.HasPrefix(strings.TrimSpace(loop), "cd ") || !strings.Contains(out, wf.NextRelisted) || strings.Contains(out, "You are the coordinator") {
		t.Errorf("a repeat next from the coordinator = %q", out)
	}
	if took := time.Since(start); took < 400*time.Millisecond {
		t.Errorf("a repeat next returned after %s; it should block", took)
	}
}

// next's flag refusals: a negative wait, and --json (which never blocks) with
// a wait flag.
func TestNextRefusesWaitFlagsItCannotHonour(t *testing.T) {
	def := fanDef("flags", chItems(1), 1, wf.AllSettled())
	state := startPlain(t, def, "claude")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"wf", "next", "--timeout", "-1s"}, "--timeout must be 0"},
		{[]string{"wf", "next", "1", "--json", "--timeout", "1s"}, "--json hands out at once and never blocks"},
		{[]string{"wf", "next", "1", "--json", "--wait"}, "--json hands out at once and never blocks"},
	} {
		code, _, er := singleCLI(def, append(c.args, "--state-dir", state)...)
		if code != 2 || !strings.Contains(er, c.want) {
			t.Errorf("%v: exit %d, %q; want 2 and %q", c.args, code, er, c.want)
		}
	}
}

// Vision's ruling on next's size: one next offers as many units as fit under
// the harness's own tool-output limit (Codex: about 10 KiB or 256 lines) and
// says how many more are ready; the next call offers them. The JSON
// hand-out is not capped.
func TestNextOffersWhatFitsTheHarnessOutput(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i + 1
	}
	def := fanDef("forty-capped", chItems(items...), 40, wf.AllSettled())
	state := startPlain(t, def, "claude")
	cfg := t.TempDir() // this harness's output limit, made small
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"bashOutputMaxChars": 4000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	out := mustSingle(t, def, "wf", "next", "--state-dir", state)
	offered := strings.Count(out, "start a child agent for ")
	ready := 40
	if len(out) > 4000 || offered == 0 || offered >= ready {
		t.Fatalf("next offered %d units in %d bytes, %d lines", offered, len(out), strings.Count(out, "\n"))
	}
	if want := fmt.Sprintf("%d more are ready: %s\n", ready-offered, wf.NextMore); !strings.Contains(out, want) {
		t.Errorf("next lacks %q:\n%.300s", want, out[len(out)-300:])
	}
	again := mustSingle(t, def, "wf", "next", "--state-dir", state)
	n := strings.Count(again, "start a child agent for ")
	if n == 0 || offered+n > ready || len(again) > 4000 {
		t.Errorf("the next call offered %d more in %d bytes:\n%.600s", n, len(again), again)
	}
	for _, name := range regexp.MustCompile(`start a child agent for (\S+) `).FindAllStringSubmatch(again, -1) {
		if strings.Contains(out, "start a child agent for "+name[1]+" ") {
			t.Errorf("%s offered twice", name[1])
		}
	}
	// The JSON hand-out is a program's, not capped: every unit still ready.
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "all", "--json", "--state-dir", state)), &ins); err != nil {
		t.Fatal(err)
	}
	if left := ready - offered - n; len(agentsOf(ins)) != left {
		t.Errorf("next --json all handed out %d, want the %d still ready", len(agentsOf(ins)), left)
	}
}

// an earlier revision: next --adopt only adopts. An operator's adopt, from any harness,
// carries the run onto a changed binary and does nothing else: no
// coordinator is met, so no harness is bound, and no unit is handed out.
// The run's own coordinator then drives it.
func TestNextAdoptOnlyAdopts(t *testing.T) {
	v1, v2 := sizedAgentDef("adopt-only", "Old prompt."), sizedAgentDef("adopt-only", "New prompt.")
	state := startPlain(t, v1, "codex")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "operator") // an operator's agent of another harness
	code, out, er := singleCLI(v2, "wf", "next", "--adopt", "--state-dir", state)
	if code != 0 || !strings.Contains(out, "adopted: ") || !strings.Contains(out, "binds no harness") {
		t.Fatalf("next --adopt = exit %d %q %q", code, out, er)
	}
	journal := mustSingle(t, v2, "wf", "journal", "--state-dir", state)
	if !strings.Contains(journal, `"migrated"`) {
		t.Fatalf("next --adopt did not adopt:\n%s", journal)
	}
	for _, kind := range []string{`"coordinator"`, `"handed_out"`} {
		if strings.Contains(journal, kind) {
			t.Fatalf("next --adopt journaled %s:\n%s", kind, journal)
		}
	}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "coord")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, v2, "wf", "next", "1", "--json", "--state-dir", state)), &ins); err != nil || len(agentsOf(ins)) != 1 {
		t.Fatalf("the run's coordinator after the adopt: %v %v", ins, err)
	}
}

// The rules reach a child that never fetches its prompt (fork an earlier revision):
// every spawn brief ends with the short rules for any agent, the claim still
// leading its first line, and without the coordinator's own fan-out rules.
func TestEveryBriefCarriesTheRules(t *testing.T) {
	d := fanDef("brief-rules", chItems(1, 2), 2, wf.AllSettled())
	h := newHarness(t, d, nil)
	run := h.run(d, nil)
	rules := strings.Replace(wf.RulesForSpawnBrief, " The workflow moves the tracker items of this run: change one only if the prompt of your task says so.", "", 1)
	for _, in := range handOutAs(t, h, run, 2, "boss") {
		first, _, _ := strings.Cut(in.Brief, "\n")
		if !strings.Contains(first, "--claim "+claimToken(t, in)) {
			t.Errorf("%s brief's first line lost the claim: %q", in.Name, first)
		}
		if !strings.HasSuffix(in.Brief, rules+"\n") || strings.Contains(in.Brief, wf.RulesForCoordinators) {
			t.Errorf("%s brief does not end with the spawn brief's rules alone:\n%s", in.Name, in.Brief)
		}
	}
	// The short form restricts how the run's workflow state changes, never
	// the child's own task: a unit that asks the child to inspect, test and
	// commit (examples/feature's Implement) leaves it its ordinary tools.
	for _, want := range []string{"do your assigned task with ordinary tools", "workflow state only through the workflow commands", "lock, journal or state files"} {
		if !strings.Contains(wf.RulesForSpawnBrief, want) {
			t.Errorf("the spawn brief's rules lack %q: %s", want, wf.RulesForSpawnBrief)
		}
	}
	for _, gone := range []string{"run only the commands", "use only the commands"} {
		if strings.Contains(wf.RulesForSpawnBrief, gone) {
			t.Errorf("the spawn brief's rules forbid the task's own commands (%q): %s", gone, wf.RulesForSpawnBrief)
		}
	}
}
