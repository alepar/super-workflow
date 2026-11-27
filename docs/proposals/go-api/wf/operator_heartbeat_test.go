package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A run driven from a harness whose coordinator is gone keeps its children's
// claims running. A person releases them from a plain shell with dev
// heartbeat died, by name or by session: each is lost as an orphan, spends
// no retry and never reaches the lost cap, and is offered again to the next
// coordinator. The coordinator's own harness and plain heartbeat from the
// shell are refused, and an operator reports died only.
func TestAnOperatorReleasesAGoneChildsClaimAsAnOrphan(t *testing.T) {
	strictCallers(t)
	root, state := t.TempDir(), filepath.Join(t.TempDir(), "state")
	t.Setenv("HERMES_HOME", root)
	t.Setenv("HERMES_DELEGATED_CHILD_CONTEXT", "")
	coordinator := func() { t.Setenv("HERMES_SESSION_ID", "coordinator") }
	operator := func() {
		for _, key := range []string{"HERMES_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "PI_SESSION_ID"} {
			t.Setenv(key, "")
		}
	}
	def := fanDef("operator-heartbeat", chItems(1, 2, 3), 3, wf.AllSettled())
	coordinator()
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	next := func(n string) []wf.Instruction {
		t.Helper()
		coordinator()
		var ins []wf.Instruction
		if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", n, "--json", "--state-dir", state)), &ins); err != nil {
			t.Fatal(err)
		}
		return agentsOf(ins)
	}
	start := func(in wf.Instruction, agent string) {
		t.Helper()
		operator()
		mustSingle(t, def, "wf", "dev", "started", in.Name, "--session", "coordinator", "--harness", "hermes", "--agent", agent, "--state-dir", state)
	}
	ins := next("3")
	if len(ins) != 3 {
		t.Fatalf("next 3 = %d units, want 3", len(ins))
	}
	for i, in := range ins {
		start(in, "a"+string(rune('1'+i)))
	}
	journal := filepath.Join(state, "runs", "r1", "journal.jsonl")
	unchanged := func(why string, args ...string) {
		t.Helper()
		before, _ := os.ReadFile(journal)
		if code, out, er := singleCLI(def, append(args, "--state-dir", state)...); code == 0 {
			t.Fatalf("%s: exit 0: %s%s", why, out, er)
		}
		if after, _ := os.ReadFile(journal); string(after) != string(before) {
			t.Fatalf("%s: the refused call changed the journal", why)
		}
	}
	coordinator()
	unchanged("dev heartbeat from the coordinator's harness", "wf", "dev", "heartbeat", "died", "--session", "coordinator")
	operator()
	unchanged("plain heartbeat from a plain shell", "wf", "heartbeat", "died", "--session", "coordinator")
	unchanged("an operator's active", "wf", "dev", "heartbeat", "active", ins[0].Name)

	mustSingle(t, def, "wf", "dev", "heartbeat", "died", ins[0].Name, "--state-dir", state)
	mustSingle(t, def, "wf", "dev", "heartbeat", "died", "--session", "coordinator", "--state-dir", state)
	if st := runStatus(t, def, state); st.Running != 0 {
		t.Fatalf("running after the releases = %d, want 0", st.Running)
	}
	lost := lostCauses(t, journal)
	if len(lost) != 3 || strings.Count(strings.Join(lost, ","), wf.LostCauseOrphan) != 3 {
		t.Fatalf("attempt_lost causes = %v, want three %q", lost, wf.LostCauseOrphan)
	}
	// The degraded records say an operator released them, for an audit.
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "an operator reported its child gone"); got != 1 {
		t.Fatalf("by-name operator releases journaled = %d, want 1", got)
	}
	if got := strings.Count(string(data), "an operator reported the children of coordinator session coordinator gone"); got != 2 {
		t.Fatalf("by-session operator releases journaled = %d, want 2", got)
	}

	// Past the lost cap, the same unit is still offered again: no release
	// spends a retry or counts toward it.
	name := strings.TrimSuffix(ins[0].Name, "#1")
	for round := 2; round <= 7; round++ {
		var in wf.Instruction
		for _, re := range next("3") {
			if strings.TrimSuffix(re.Name, "#"+string(rune('0'+round))) == name {
				in = re
			}
		}
		if in.Name == "" || in.Attempt != round {
			t.Fatalf("round %d: %s not offered again at attempt %d", round, name, round)
		}
		start(in, "r"+string(rune('0'+round)))
		mustSingle(t, def, "wf", "dev", "heartbeat", "died", in.Name, "--state-dir", state)
	}
	if st := runStatus(t, def, state); st.Phase == wf.RunEnded {
		t.Fatalf("the run ended on released claims: %+v", st)
	}
}

func runStatus(t *testing.T, def *wf.Def, state string) wf.RunStatus {
	t.Helper()
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--state-dir", state)), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// lostCauses lists each attempt_lost record's cause, in journal order.
func lostCauses(t *testing.T, journal string) []string {
	t.Helper()
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec struct {
			Kind wf.JournalKind
			Data struct{ Cause string }
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Kind == wf.JournalAttemptLost {
			out = append(out, rec.Data.Cause)
		}
	}
	return out
}
