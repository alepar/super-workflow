package wf_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// shellShapes are the command shapes a permission checker refuses or flags:
// loops, command substitution, variable expansion, a nested shell, pipes and
// command lists. A printed command is one plain command, after at most one
// "cd <run root> && ".
var shellShapes = []struct {
	why string
	re  *regexp.Regexp
}{
	{"a loop", regexp.MustCompile(`\b(while|until|for)\b[^\n]*;\s*do\b`)},
	{"command substitution", regexp.MustCompile("\\$\\(|`")},
	{"variable expansion", regexp.MustCompile(`\$\{|\$[A-Za-z_]`)},
	{"a nested shell", regexp.MustCompile(`\b(ba|z)?sh -c\b`)},
	{"a pipe or command list", regexp.MustCompile(`[^|]\|[^|]|\|\||;\s`)},
}

// plainCommandProblem is why a printed line holding a command (text naming
// bin) is not one plain command at the start of its line, or "". It sees
// what a real binary prints in both CLI modes; whether anything follows the
// command on its line is held exactly by TestEveryPrintedCommandIsAWholeLine,
// which knows each command the engine built.
func plainCommandProblem(line, bin string) string {
	i := strings.Index(line, bin)
	if i < 0 {
		return ""
	}
	// A loop, a substitution, an expansion or a nested shell may wrap the
	// command, so they are looked for in the whole line; prose around a
	// command uses ";" and so a list or pipe only counts within it.
	for _, s := range shellShapes[:4] {
		if s.re.MatchString(line) {
			return s.why
		}
	}
	cmd := line[i:]
	if lead := strings.TrimLeft(line[:i], " "); lead != "" && !strings.HasPrefix(lead, "cd ") {
		return "prose before a command"
	}
	if j := strings.LastIndex(line[:i], "cd "); j >= 0 {
		cmd = line[j:]
		rest, ok := strings.CutPrefix(cmd, "cd ")
		dir, after, found := strings.Cut(rest, " && ")
		if !ok || !found || !filepath.IsAbs(strings.Trim(dir, "'")) {
			return "a cd that is not \"cd <absolute run root> && <command>\""
		}
		cmd = after
	}
	if strings.Contains(cmd, "&&") {
		return "a command list"
	}
	if s := shellShapes[4]; s.re.MatchString(cmd) {
		return s.why
	}
	return ""
}

func TestPlainCommandProblemCatchesShellShapes(t *testing.T) {
	const bin = "/abs/super"
	for line, want := range map[string]string{
		"cd /ws && /abs/super wf next --run-id r":                            "",
		"cd '/w s' && '/abs/super' wf done --run-id r":                       "",
		"Finish: check it with cd /ws && /abs/super wf validate x until ok.": "prose before a command",
		"while true; do /abs/super wf next; done":                            "a loop",
		"/abs/super wf done --run-id $(cat id)":                              "command substitution",
		"/abs/super wf next --run-id $RUN":                                   "variable expansion",
		"bash -c '/abs/super wf next'":                                       "a nested shell",
		"/abs/super wf status | head":                                        "a pipe or command list",
		"cd ws && /abs/super wf next":                                        "a cd that is not \"cd <absolute run root> && <command>\"",
		"cd /ws && /abs/super wf next && echo done":                          "a command list",
	} {
		if got := plainCommandProblem(line, bin); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

// Every command the engine prints for an agent (the coordinator bootstrap,
// next with its spawn briefs, a child's prompt with prompt, validate, done
// and failed, status, and the recovery after a lost unit) is one plain
// command per line, with absolute paths: what a permission checker reading
// it passes without a person.
func TestEveryPrintedCommandIsOnePlainLine(t *testing.T) {
	// The engine names itself by os.Executable, which on Linux is the real
	// path even when the binary was started through a link.
	bin, err := os.Executable()
	if err != nil || !filepath.IsAbs(bin) {
		t.Skip("the test binary path is not absolute")
	}
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "multi", true: "named"}[named], func(t *testing.T) {
			e := newCLIEnv(t)
			env := []string{"WF_TEST_NAMED=0", "WF_TEST_BOOTSTRAP=1"}
			if named {
				env = []string{"WF_TEST_NAMED=1", "WF_TEST_BOOTSTRAP=1"}
			}
			var printed []string
			proc := func(args ...string) string {
				t.Helper()
				code, out, er := e.proc(e.ws, env, args...)
				if code != 0 {
					t.Fatalf("%v: %d: %s %s", args, code, out, er)
				}
				printed = append(printed, out, er)
				return out
			}
			verb := func(v string, rest ...string) []string {
				if named {
					return append([]string{"wf", v}, append(rest, "--run-id", "plain", "--state-dir", e.state)...)
				}
				return append([]string{v, "plain"}, append(rest, "--state-dir", e.state)...)
			}
			proc("run", "agent-cli", "--run-id", "plain", "--state-dir", e.state, "--workspace", e.ws)
			offer := proc(verb("next", "1", "--wait")...)
			m := regexp.MustCompile(`--claim '?([0-9a-f]+)`).FindStringSubmatch(offer)
			if m == nil {
				t.Fatalf("no claim in the offer: %s", offer)
			}
			prompt := proc(verb("prompt", "--claim", m[1])...)
			printed = append(printed, withBody(t, prompt))
			proc(verb("status")...)
			name := regexp.MustCompile(`subagent (\S+)\.`).FindStringSubmatch(withBody(t, prompt))
			if name != nil {
				proc(verb("heartbeat", "died", name[1])...)
				proc(verb("next", "1", "--wait")...)
			}
			seen := 0
			for _, text := range printed {
				for _, line := range strings.Split(text, "\n") {
					if !strings.Contains(line, bin) {
						continue
					}
					seen++
					if why := plainCommandProblem(line, bin); why != "" {
						t.Errorf("printed command is %s: %q", why, line)
					}
				}
			}
			if seen < 6 {
				t.Fatalf("only %d printed command lines seen; the scan missed the output", seen)
			}
		})
	}
}

// The coordinator bootstrap and every spawn brief say what the run does:
// the commands it runs, that next blocks and is not polled, and the work
// the definition describes (Describe), so a permission checker reading only
// that context sees the expected work.
func TestTheRunSaysWhatItDoes(t *testing.T) {
	const work = "read and write the repo's bd tracker as their prompts say"
	d, _ := simple("what-it-does", wf.PromptText("Do the task."))
	d.Describe(work)
	h := newHarness(t, d, nil)
	run, ins := handOneUnit(t, h, d)
	st := h.status(run)
	boot := wf.Bootstrap(st, "/abs/super", "/abs/state")
	if !strings.Contains(boot, "What this run does: an unattended workflow run in "+st.Workspace+"; its units "+work+".") || !strings.Contains(boot, "with no wait loop around it") {
		t.Fatalf("bootstrap does not say what the run does:\n%s", boot)
	}
	if strings.Index(boot, "What this run does") > strings.Index(boot, "Rules for this run") {
		t.Fatalf("the paragraph comes after the rules:\n%s", boot)
	}
	lines := strings.Split(ins.Brief, "\n")
	// Line 1 stays the prompt command (BriefCommand, kickoff clipping) and
	// line 2 its framing; the unit's work follows, ahead of the rules.
	if len(lines) < 3 || !strings.HasPrefix(lines[2], "This unit runs the commands its prompt prints (prompt, validate, done, failed), by default as printed, one per line") || !strings.HasSuffix(lines[2], "; its work: "+work+".") {
		t.Fatalf("brief lines = %q, want line 3 to say what the unit runs and its work", lines)
	}
	if strings.Index(ins.Brief, "This unit runs") > strings.Index(ins.Brief, "Rules for this run") {
		t.Fatalf("the unit's work comes after the rules: %s", ins.Brief)
	}
	if !strings.Contains(wf.RulesForAnyAgent, "with no wait loop around it: the engine's commands wait by themselves") {
		t.Fatalf("the rules do not give the default against wait loops, with its reason: %s", wf.RulesForAnyAgent)
	}
	// Describe is presentation: it leaves the definition's hash alone, so a
	// held run needs no adoption when a definition gains it.
	bare, _ := simple("what-it-does", wf.PromptText("Do the task."))
	if bare.Hash() != d.Hash() {
		t.Fatalf("Describe changed the definition hash: %s vs %s", bare.Hash(), d.Hash())
	}
	// Without Describe the work clause is left out, not left empty.
	plain, _ := simple("what-it-does-plain", wf.PromptText("Do the task."))
	h2 := newHarness(t, plain, nil)
	run2, ins2 := handOneUnit(t, h2, plain)
	if strings.Contains(ins2.Brief, "its work:") || strings.Contains(wf.Bootstrap(h2.status(run2), "/a", "/s"), "its units") {
		t.Fatalf("an undescribed definition prints an empty work clause: %s", ins2.Brief)
	}
}

// Describe is one short phrase: every brief carries it.
func TestALongDescribeIsAMisuse(t *testing.T) {
	d, _ := simple("long-describe", wf.PromptText("Do the task."))
	d.Describe(strings.Repeat("w", 129))
	if !strings.Contains(fmt.Sprint(d.Lint()), "Describe: 128 characters") {
		t.Fatalf("lint = %v, want the long Describe named", d.Lint())
	}
	ok, _ := simple("short-describe", wf.PromptText("Do the task."))
	ok.Describe(strings.Repeat("w", 128))
	if strings.Contains(fmt.Sprint(ok.Lint()), "Describe") {
		t.Fatalf("lint = %v, want a 128-character Describe accepted", ok.Lint())
	}
}

// A first offer is measured too: when even one unit with its "This unit
// runs" line would cross the harness's output limit, the briefs drop that
// line rather than exceed it. Claude's smallest limit (4000 characters) and
// a workspace long enough that only the bare offer fits.
func TestAFirstOfferNearTheLimitDropsItsDescribeLine(t *testing.T) {
	const limit = 4000 // Claude Code clamps bashOutputMaxChars to at least this
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(fmt.Sprintf(`{"bashOutputMaxChars": %d}`, limit)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	root := t.TempDir()
	workspace := func(pad int) string {
		dir := filepath.Join(root, fmt.Sprintf("p%d", pad))
		for ; pad > 0; pad -= 60 {
			dir = filepath.Join(dir, strings.Repeat("w", min(pad, 60)))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	runs := 0
	next := func(def *wf.Def, ws string) string {
		t.Helper()
		runs++
		state := filepath.Join(t.TempDir(), "state")
		mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--run-id", fmt.Sprintf("r%d", runs), "--harness", "claude")
		return mustSingle(t, def, "wf", "next", "--state-dir", state)
	}
	room := limit - wf.OutputReserveBytes // what one offer may take
	offerSize := func(out string) int {
		t.Helper()
		start := strings.Index(out, "start a child agent for ")
		end := strings.LastIndex(out, "\n-----\n")
		if start < 0 || end < start {
			t.Fatalf("next has no framed offer:\n%s", out)
		}
		return end + len("\n-----\n") - start // the header and footer use the reserve
	}
	pad := -1
	for p := 0; p <= 900; p += 6 {
		if offerSize(next(sizedAgentDef("near-limit", "Do the task."), workspace(p))) > room {
			break
		}
		pad = p
	}
	if pad < 0 {
		t.Fatal("even a short workspace's bare offer passes the limit")
	}
	described := sizedAgentDef("near-limit", "Do the task.")
	described.Describe(strings.Repeat("w", 128))
	if short := next(described, workspace(0)); !strings.Contains(short, "This unit runs") {
		t.Fatalf("with room the brief keeps its line:\n%s", short)
	}
	out := next(described, workspace(pad))
	if n := offerSize(out); strings.Count(out, "start a child agent for ") != 1 ||
		strings.Contains(out, "This unit runs") || n > room || len(out) > limit || !strings.HasSuffix(out, idleTurnReminder) {
		t.Fatalf("near the limit: %d/%d offer bytes, %d/%d total bytes; want one bare offer followed by the reminder:\n%s", n, room, len(out), limit, out)
	}
}

// The bootstrap verb again from the session that already coordinates the
// run prints its loop: the label on a line of its own, then the bootstrap's
// own loop command, whole.
func TestARepeatBootstrapPrintsItsLoopAsAWholeLine(t *testing.T) {
	def := fanDef("repeat-boot", chItems(1), 1, wf.AllSettled())
	state := startPlain(t, def, "claude")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "coord")
	t.Setenv("BASH_DEFAULT_TIMEOUT_MS", "1000")
	boot := mustSingle(t, def, "next", "--state-dir", state)
	_, loop, _ := strings.Cut(boot, "\nloop:\n")
	loop, _, _ = strings.Cut(loop, "\n")
	mustSingle(t, def, "wf", "next", "--state-dir", state)
	again := mustSingle(t, def, "next", "--state-dir", state)
	if !strings.Contains(again, "you already coordinate run r1; your loop is:\n"+loop+"\n") {
		t.Fatalf("a repeat bootstrap = %q, want the label, then the loop line %q", again, loop)
	}
	bin, _ := os.Executable()
	for _, text := range []string{boot, again} {
		for _, line := range strings.Split(text, "\n") {
			if why := plainCommandProblem(line, bin); why != "" {
				t.Errorf("printed command is %s: %q", why, line)
			}
		}
	}
}
