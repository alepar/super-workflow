package wf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// handOneIn hands out the one unit of run and returns its name.
func handOneIn(t *testing.T, def *wf.Def, state, run string) string {
	t.Helper()
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--run-id", run, "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("wf next 1 --run-id %s = %v, %v; want one instruction", run, ins, err)
	}
	return ins[0].Name
}

func sessionOf(t *testing.T, def *wf.Def, state, run string) string {
	t.Helper()
	var st wf.RunStatus
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "status", "--json", "--run-id", run, "--state-dir", state)), &st); err != nil || len(st.Units) != 1 {
		t.Fatalf("status of %s: %+v, %v", run, st, err)
	}
	return st.Units[0].Session
}

// A unit verb names its run by its unit (item 70): with two runs under one
// state dir, `wf started|done|failed <name>` acts on the run that holds the
// unit, never on the recorded default run; an explicit run that does not hold
// it, while another run does, is refused; a name live in several runs needs
// --run-id.
func TestUnitVerbResolvesItsOwnRun(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	name := handOneIn(t, def, state, "r1")
	// r2 starts last, so the run-id file now names r2: the default run.
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")

	mustSingle(t, def, "wf", "dev", "started", name, "--session", "s1", "--agent", "a1", "--state-dir", state)
	if got := sessionOf(t, def, state, "r1"); got != "s1" {
		t.Errorf("r1's unit session %q, want s1: started acted on another run", got)
	}

	code, _, er := singleCLI(def, "wf", "failed", name, "--reason", "x", "--run-id", "r2", "--state-dir", state)
	if code != 2 || !strings.Contains(er, "token belongs to run r1; use the command in your instruction") {
		t.Errorf("failed --run-id r2: exit %d, stderr %q; want 2 naming r1", code, er)
	}

	if other := handOneIn(t, def, state, "r2"); other != name {
		t.Fatalf("r2's unit is %q, want the same name %q for the ambiguity case", other, name)
	}
	code, _, er = singleCLI(def, "wf", "failed", name, "--reason", "x", "--state-dir", state)
	if code != 2 || !strings.Contains(er, "r1") || !strings.Contains(er, "r2") || !strings.Contains(er, "--run-id") {
		t.Errorf("name live in two runs: exit %d, stderr %q; want 2 naming both runs and --run-id", code, er)
	}
}

// The multi-registration form names its run positionally; a run that does not
// hold the unit, while another does, is refused the same way.
func TestExplicitRunVerbRefusesAForeignUnit(t *testing.T) {
	def := clAgentDef("one-agent-multi")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	name := handOneIn(t, def, state, "r1")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")

	cfg := wf.MainConfig{Adapters: clConfig().Adapters, Defs: []wf.Registration{{Def: def}}}
	var o, er bytes.Buffer
	code := fixtureCLI(cfg, []string{"dev", "started", "r2", name, "--session", "s1", "--agent", "a1", "--state-dir", state}, &o, &er)
	if code != 2 || !strings.Contains(er.String(), "token belongs to run r1; use the command in your instruction") {
		t.Errorf("started r2 <r1's unit>: exit %d, stderr %q; want 2 naming r1", code, er.String())
	}
}

// The root cause: every line the named binary hands out names its run, so a
// child of r1 acts on r1 even while r2 is the recorded default. The brief's
// prompt command is run exactly as printed, and the done and failed lines of
// the prompt it prints name r1 too.
func TestBriefLinesNameTheirRun(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	var ins []wf.Instruction
	if err := json.Unmarshal([]byte(mustSingle(t, def, "wf", "next", "1", "--state-dir", state, "--json")), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("next: %v, %v", ins, err)
	}
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")
	cmd, _ := wf.BriefCommand(ins[0].Brief)
	if !strings.Contains(cmd, " wf prompt ") || !strings.Contains(cmd, "--run-id r1") {
		t.Fatalf("brief's prompt command does not name run r1:\n%s", ins[0].Brief)
	}
	// The page names the done command; the failed one is in its body.
	_, cmd, ok := strings.Cut(cmd, " && ")
	if !ok {
		t.Fatal("brief command lacks workspace prefix")
	}
	fetched := withBody(t, mustSingle(t, def, strings.Fields(cmd)[1:]...))
	for _, verb := range []string{" wf done ", " wf failed "} {
		i := strings.Index(fetched, verb)
		line := ""
		if i >= 0 {
			line, _, _ = strings.Cut(fetched[i:], ",")
		}
		if !strings.Contains(line, "--run-id r1") {
			t.Errorf("prompt's%sline does not name run r1:\n%s", verb, fetched)
		}
	}
	mustSingle(t, def, "wf", "dev", "started", ins[0].Name, "--session", "s1", "--agent", "a1", "--state-dir", state, "--run-id", "r1")
	prompt := withBody(t, mustSingle(t, def, "wf", "prompt", ins[0].Name, "--state-dir", state))
	if !strings.Contains(prompt, "Say hello.") {
		t.Errorf("prompt without --run-id did not resolve r1's unit:\n%s", prompt)
	}
}

// endRun appends a run_ended record to run's journal, as a halted or stopped
// run leaves it with a unit still handed out.
func endRun(t *testing.T, state, run string) {
	t.Helper()
	path := filepath.Join(state, "runs", run, "journal.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var last wf.JournalRecord
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatal(err)
	}
	ended, _ := json.Marshal(wf.RunEndedData{Outcome: wf.Stopped, Cause: "test"})
	rec, _ := json.Marshal(wf.JournalRecord{Seq: last.Seq + 1, Kind: wf.JournalRunEnded, Run: wf.RunID(run), DefHash: last.DefHash, Data: ended})
	if err := os.WriteFile(path, append(data, append(rec, '\n')...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An ended run holds no unit: a successor (or any later run) that re-issued
// the same name is the one a verb without --run-id acts on, never an
// ambiguity with the ended run.
func TestEndedRunIsNotAUnitOwner(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	name := handOneIn(t, def, state, "r1")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")
	if other := handOneIn(t, def, state, "r2"); other != name {
		t.Fatalf("r2's unit %q, want %q", other, name)
	}
	endRun(t, state, "r1")
	// Point the default at the ended run: resolution must still pick r2.
	if err := os.WriteFile(filepath.Join(state, "run-id"), []byte("r1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSingle(t, def, "wf", "dev", "started", name, "--session", "s2", "--agent", "a2", "--state-dir", state)
	if got := sessionOf(t, def, state, "r2"); got != "s2" {
		t.Errorf("r2's unit session %q, want s2", got)
	}
}

// The ownership peek takes no lock: a coordinator holding another run's lock
// does not stall a verb on this one, and the verb still lands on its own run.
func TestOwnerPeekTakesNoLock(t *testing.T) {
	def := clAgentDef("one-agent")
	state := filepath.Join(t.TempDir(), "state")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r1")
	name := handOneIn(t, def, state, "r1")
	mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--run-id", "r2")
	unlock, err := wf.FileJournal(state).Lock("r2")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	mustSingle(t, def, "wf", "dev", "started", name, "--session", "s1", "--agent", "a1", "--state-dir", state)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("started took %v with another run's lock held, want no wait", d)
	}
	unlock()
	if got := sessionOf(t, def, state, "r1"); got != "s1" {
		t.Errorf("r1's unit session %q, want s1", got)
	}
}
