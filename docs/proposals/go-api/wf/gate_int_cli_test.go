package wf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestGateAnswerCLICompatibility(t *testing.T) {
	for _, args := range [][]string{
		{"--gate", "lgAsk", "--value", "done"},
		{"lgAsk", "done"},
		{"lgAsk", "done", "--gate", "lgAsk", "--value", "done"},
	} {
		t.Run(args[0]+args[1], func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			gatesMust(t, "run", "loop-gate", "--state-dir", state, "--run-id", "gate-cli")
			gatesMust(t, "next", "gate-cli", "all", "--json", "--state-dir", state)
			gatesMust(t, append(append([]string{"answer", "gate-cli"}, args...), "--state-dir", state)...)
			var st wf.RunStatus
			if err := json.Unmarshal([]byte(gatesMust(t, "status", "gate-cli", "--json", "--state-dir", state)), &st); err != nil {
				t.Fatal(err)
			}
			if st.Phase != wf.RunEnded {
				t.Fatalf("answer did not end run: %+v", st)
			}
		})
	}
}

func TestGateAnswerCLIRejectsConflictsAndBadNumbers(t *testing.T) {
	for _, args := range [][]string{
		{"lgAsk", "done", "--gate", "other"},
		{"lgAsk", "done", "--value", "again"},
		{"--gate", "lgAsk", "--value", ""},
		{"--gate", "lgAsk", "--value", "done", "--number", ""},
		{"--gate", "lgAsk", "--value", "done", "--number", "3.5"},
		{"--gate", "lgAsk", "--value", "done", "--number", "9999999999999999999999999999"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			gatesMust(t, "run", "loop-gate", "--state-dir", state, "--run-id", "gate-cli")
			gatesMust(t, "next", "gate-cli", "all", "--json", "--state-dir", state)
			if code, _, err := gatesCLI(append(append([]string{"answer", "gate-cli"}, args...), "--state-dir", state)...); code != 2 {
				t.Fatalf("invalid answer exited %d: %s", code, err)
			}
			gatesMust(t, "answer", "gate-cli", "--gate", "lgAsk", "--value", "done", "--state-dir", state)
		})
	}
}

func TestGateAnswerCLIIntegerValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"proposal", []string{"--gate", "intCLIAsk", "--value", "raise"}, 4},
		{"override", []string{"--gate", "intCLIAsk", "--value", "raise", "--number", "15"}, 15},
		{"zero", []string{"intCLIAsk", "raise", "--number", "0"}, 0},
		{"visit", []string{"intCLIAsk", "raise", "--number", "9", "--visit", "1"}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			cfg := wf.MainConfig{Defs: []wf.Registration{{Def: intGateCLIDef()}}}
			call := func(args ...string) string {
				t.Helper()
				code, out, err := cliCall(cfg, append(args, "--state-dir", state)...)
				if code != 0 && !(code == 7 && args[0] == "next") {
					t.Fatalf("%v exited %d: %s", args, code, err)
				}
				return out
			}
			call("run", "integer-gate-cli", "--run-id", "integer-cli")
			call("next", "integer-cli", "all", "--json")
			call(append([]string{"answer", "integer-cli"}, tc.args...)...)
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(call("journal", "integer-cli")), "\n") {
				var rec wf.JournalRecord
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatal(err)
				}
				if rec.Kind != wf.JournalGateAnswered {
					continue
				}
				var data wf.GateAnsweredData
				if err := json.Unmarshal(rec.Data, &data); err != nil {
					t.Fatal(err)
				}
				if data.Answer != "raise" || data.IntValue == nil || *data.IntValue != tc.want {
					t.Fatalf("accepted answer = %+v, want raise with %d", data, tc.want)
				}
				count++
			}
			if count != 1 {
				t.Fatalf("accepted %d answers, want 1", count)
			}
		})
	}
}

type intCLIAsk struct{}
type intCLIDone struct{}
type intCLIChoice string

func intGateCLIDef() *wf.Def {
	d := wf.New("integer-gate-cli")
	choice := intCLIChoice("raise")
	choices := wf.NewEnum(d, "integer-cli-choice", choice)
	gate := wf.NewGate[intCLIAsk, intCLIChoice](d, "Choose a limit", choices, wf.Reserved)
	done := wf.NewTerminal[intCLIDone](d, wf.Success)
	gate.IntValue(choice, 0, 20, func(wf.Ctx) int { return 4 })
	gate.Answers(wf.On(choice, gate.To(done, "raised")))
	return d
}

func TestGateAnswerCLIReservedInstructionCommand(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfg := wf.MainConfig{Defs: []wf.Registration{{Def: intGateCLIDef()}}}
	code, _, err := cliCall(cfg, "run", "integer-gate-cli", "--run-id", "reserved-cli", "--state-dir", state)
	if code != 0 {
		t.Fatalf("run: %d %s", code, err)
	}
	code, out, err := cliCall(cfg, "next", "reserved-cli", "--state-dir", state)
	if code != 7 {
		t.Fatalf("reserved next: %d %s %s", code, out, err)
	}
	_, command, ok := strings.Cut(out, "to use the person's number, run:\n")
	if !ok {
		t.Fatalf("reserved instruction lacks numeric command: %s", out)
	}
	command, _, ok = strings.Cut(command, " --number <integer>")
	if !ok {
		t.Fatalf("numeric command lacks placeholder: %s", out)
	}
	// Strip the workspace prefix before calling the CLI in-process.
	_, command, ok = strings.Cut(command, " && ")
	if !ok {
		t.Fatalf("numeric command lacks workspace prefix: %s", out)
	}
	// This fixture's command uses only whitespace-free paths and identifiers.
	args := strings.Fields(command)
	for i := range args {
		args[i] = strings.Trim(args[i], "'")
	}
	args = append(args[1:], "--number", "0")
	code, _, err = cliCall(cfg, args...)
	if code != 0 {
		t.Fatalf("rendered numeric command %v: %d %s", args, code, err)
	}
	code, out, err = cliCall(cfg, "status", "reserved-cli", "--json", "--state-dir", state)
	var status wf.RunStatus
	if code != 0 || json.Unmarshal([]byte(out), &status) != nil || status.Phase != wf.RunEnded {
		t.Fatalf("rendered answer did not end run: %d %s %s", code, out, err)
	}
}
