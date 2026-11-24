package wf_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestNamedNextReservedGatePrecedesEveryBootstrapWithoutHandouts(t *testing.T) {
	def := hoGateFan(2)
	h := newHarness(t, def, nil)
	state, workspace := t.TempDir(), t.TempDir()
	h.j = wf.FileJournal(state)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "reserved-bootstrap", Workspace: workspace})
	run := h.run(def, nil)
	engine := h.e.(*wf.Engine)
	ins, err := engine.HandOutAs(run, 1, wf.Coordinator{Harness: "codex", Session: "known"})
	if err != nil || len(ins) != 1 {
		t.Fatalf("setup handout: %+v %v", ins, err)
	}
	if err := engine.Started(run, ins[0].Name, claim("child")); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Done(run, ins[0].Name, wf.Submission{Output: json.RawMessage(`{"OK":true}`)}); err != nil {
		t.Fatal(err)
	}
	cfg := wf.MainConfig{Binary: "super", Defs: []wf.Registration{{Def: def}}}
	call := func(session string, args ...string) (int, string, string) {
		t.Helper()
		var out, stderr strings.Builder
		env := func(key string) string {
			if key == "CODEX_SESSION_ID" || key == "CODEX_THREAD_ID" {
				return session
			}
			return ""
		}
		args = append(args, "--state-dir", state, "--run-id", string(run), "--workspace", workspace)
		code := wf.CLIWithEnv(cfg, args, &out, &stderr, env)
		return code, out.String(), stderr.String()
	}
	code, raw, stderr := call("known", "wf", "next", "0", "--json")
	if code != 7 {
		t.Fatalf("reserved wf next: %d %s %s", code, raw, stderr)
	}
	var expected []wf.Instruction
	if err := json.Unmarshal([]byte(raw), &expected); err != nil || len(expected) != 1 || expected[0].Question != "Keep?" {
		t.Fatalf("reserved fixture: %s %v", raw, err)
	}
	want := expected[0].Prompt + "\n"
	before := len(h.kind(run, wf.JournalHandedOut))
	if before != 1 {
		t.Fatalf("fixture handed %d units", before)
	}
	for _, session := range []string{"", "new", "known"} {
		t.Run("session="+session, func(t *testing.T) {
			for repeat := 0; repeat < 2; repeat++ {
				code, out, stderr := call(session, "next")
				if code != 7 || out != want || stderr != "" {
					t.Errorf("named next: exit %d\nstdout=%q\nstderr=%q\nwant=%q", code, out, stderr, want)
				}
			}
		})
	}
	if after := len(h.kind(run, wf.JournalHandedOut)); after != before {
		t.Fatalf("bootstrap discovery handed hidden units: before=%d after=%d", before, after)
	}
}

// Parse the printed command with the shell, but dispatch its argv through the
// in-process CLI fixture; the generated binary itself is never executed.
func TestReservedPrintedCommandCannotAnswerLaterChildOrVisit(t *testing.T) {
	for _, which := range []string{"root-loop", "map-children"} {
		t.Run(which, func(t *testing.T) {
			def := loopGateDef()
			if which == "map-children" {
				def = hoGateFan(2)
			}
			state := t.TempDir()
			h := newHarness(t, def, nil)
			h.j = wf.FileJournal(state)
			h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "bound-answer", AnswerLine: func(run wf.RunID, gate string) string {
				return fmt.Sprintf("super wf answer --state-dir %s --run-id %s --gate %s --value <value>", quoteForSh(state), quoteForSh(string(run)), quoteForSh(gate))
			}})
			run := h.run(def, nil)
			engine := h.e.(*wf.Engine)
			if which == "map-children" {
				agents, err := engine.Next(run, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, agent := range agents {
					if _, err := engine.Submit(run, wf.Submission{Child: agent.Child, Token: agent.Token, Output: json.RawMessage(`{"OK":true}`)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			gates, err := engine.HandOutAs(run, 0, wf.Coordinator{})
			if err != nil || len(gates) != 1 {
				t.Fatalf("gate offer: %+v %v", gates, err)
			}
			if !strings.Contains(gates[0].Submit, "--child ") || !strings.Contains(gates[0].Submit, "--visit 1") {
				t.Fatalf("unbound printed command: %s", gates[0].Submit)
			}
			choice := "again"
			if which == "map-children" {
				choice = "yes"
			}
			line := strings.Replace(gates[0].Submit, "<value>", quoteForSh(choice), 1)
			raw, err := exec.Command("sh", "-c", "set -- "+line+`; printf '%s\000' "$@"`).Output()
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
			cfg := wf.MainConfig{Binary: "super", Defs: []wf.Registration{{Def: def}}}
			call := func() (int, string) {
				var out, stderr strings.Builder
				code := wf.CLIWithEnv(cfg, argv[1:], &out, &stderr, func(string) string { return "" })
				return code, stderr.String()
			}
			if code, stderr := call(); code != 0 {
				t.Fatalf("printed command failed: %d %s", code, stderr)
			}
			before := len(h.records(run))
			if code, stderr := call(); code != 4 {
				t.Fatalf("stale printed command accepted: %d %s", code, stderr)
			}
			if len(h.records(run)) != before {
				t.Fatal("stale answer changed later gate")
			}
		})
	}
}

func TestNamedBootstrapDoesNotDrivePendingCommand(t *testing.T) {
	def := clCommandDef()
	world := newWorld()
	h := newHarness(t, def, world)
	state := t.TempDir()
	h.j = wf.FileJournal(state)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "bootstrap-read"})
	run := h.run(def, nil)
	before := len(h.records(run))
	var out, stderr strings.Builder
	cfg := wf.MainConfig{Binary: "super", Adapters: h.reg, Defs: []wf.Registration{{Def: def}}}
	code := wf.CLIWithEnv(cfg, []string{"next", "--state-dir", state, "--run-id", string(run)}, &out, &stderr, func(string) string { return "" })
	if code != 0 {
		t.Fatalf("bootstrap: %d %s %s", code, out.String(), stderr.String())
	}
	if n := world.appliedCount(); n != 0 {
		t.Errorf("bootstrap executed %d effects", n)
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("bootstrap appended journal records: %d -> %d", before, after)
	}
}

func TestNamedBootstrapShowsInitialReservedGateWithoutDriving(t *testing.T) {
	def := clGateDef()
	h := newHarness(t, def, nil)
	state := t.TempDir()
	h.j = wf.FileJournal(state)
	h.e = wf.NewEngine(h.j, h.reg, wf.EngineOptions{Defs: []*wf.Def{def}, RunID: "initial-gate-bootstrap"})
	run := h.run(def, nil)
	before := len(h.records(run))
	var out, stderr strings.Builder
	cfg := wf.MainConfig{Binary: "super", Defs: []wf.Registration{{Def: def}}}
	code := wf.CLIWithEnv(cfg, []string{"next", "--state-dir", state, "--run-id", string(run)}, &out, &stderr, func(string) string { return "" })
	if code != 7 || !strings.HasPrefix(out.String(), "This run is waiting for a person at clGate: Proceed?") || !strings.Contains(out.String(), "--child '' --visit 1") {
		t.Fatalf("gate bootstrap: %d %s %s", code, out.String(), stderr.String())
	}
	if after := len(h.records(run)); after != before {
		t.Errorf("gate inspection appended records: %d -> %d", before, after)
	}
}
