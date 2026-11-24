package wf

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lkDefChanged is lkDef with another prompt: the same name, another hash.
func lkDefChanged() *Def {
	d := New("lock-cli")
	a := NewAgent[lkAsk, lkAnswer](d)
	done := NewTerminal[lkDone](d, Success)
	gaveUp := NewTerminal[lkGaveUp](d, Stopped)
	d.OnFailure(Blocked, gaveUp)
	a.Spec(PromptText("Say hello, changed."), Retry[lkAsk](Const(2), a.To(gaveUp, "gave up")))
	next := a.To(done, "ok")
	a.Route(func(lkAnswer, Ctx) Edge[lkAsk] { return next }, next)
	return d
}

// claudeSession is a CLI caller in Claude session id, whose home is root.
func claudeSession(id, root string) func(cfg MainConfig, args ...string) (int, string, string) {
	return func(cfg MainConfig, args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := CLIWithEnv(cfg, args, &o, &e, func(k string) string {
			switch k {
			case "CLAUDECODE":
				return "1"
			case "CLAUDE_CODE_SESSION_ID":
				return id
			case "CLAUDE_CONFIG_DIR":
				return root
			}
			return os.Getenv(k)
		})
		return code, o.String(), e.String()
	}
}

// A coordinator verb that ends on an error is an error and nothing more:
// its text says why and that next may run again, nothing is journaled for
// the session, and the same session's next verb simply runs.
func TestCoordinatorErrorStopsNoSession(t *testing.T) {
	noHarnessEnv(t)
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	changed := MainConfig{Defs: []Registration{{Def: lkDefChanged()}}}
	dir := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	coord := claudeSession("coord-1", t.TempDir())
	var o, e bytes.Buffer
	if code := CLIWithEnv(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", ws, "--run-id", "err"}, &o, &e, os.Getenv); code != 0 {
		t.Fatalf("run: %d %s", code, e.String())
	}
	before, err := FileJournal(dir).(committedReader).ReadCommitted("err")
	if err != nil {
		t.Fatal(err)
	}

	// Its next meets a definition this binary does not run: an infra error.
	code, _, er := coord(changed, "next", "err", "1", "--state-dir", dir, "--json")
	if code != exitInfra || !strings.Contains(er, "definition hash differs") {
		t.Fatalf("the failing next: exit %d: %s", code, er)
	}
	if !strings.HasSuffix(strings.TrimSpace(er), "This command failed, and the error above says why: "+coordinatorGoOn+".") ||
		strings.Contains(er, coordinatorStopReport) || strings.Contains(er, "clear-stop") || strings.Contains(er, "told to stop") {
		t.Errorf("the error's tail: %s", er)
	}
	after, err := FileJournal(dir).(committedReader).ReadCommitted("err")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the failed verb journaled %d record(s): %v", len(after)-len(before), after[len(before):])
	}

	// The same session goes on: status sends it to next, and next hands out
	// the unit.
	code, out, er := coord(cfg, "status", "err", "--state-dir", dir)
	if code != 0 || strings.Contains(out, "stopped") {
		t.Errorf("status after the error: exit %d: %s %s", code, out, er)
	}
	var ins []Instruction
	code, out, er = coord(cfg, "next", "err", "1", "--state-dir", dir, "--json")
	if code != 0 || json.Unmarshal([]byte(out), &ins) != nil || len(ins) != 1 || ins[0].Kind != InstructionAgent {
		t.Fatalf("next after the error: exit %d: %s %s", code, out, er)
	}
	if code, _, er := coord(cfg, "retry", "err", "--state-dir", dir); strings.Contains(er, "told to stop") || strings.Contains(er, coordinatorStopReport) {
		t.Errorf("retry after the error: exit %d: %s", code, er)
	}

	// The verb that let a person clear a stopped session is gone with it.
	if code, _, _ := coord(cfg, "dev", "clear-stop", "err", "--state-dir", dir); code != exitUsage {
		t.Errorf("dev clear-stop: exit %d, want a usage error for an unknown verb", code)
	}
}

// A journal written before the session stop was removed holds its records
// (session_stopped, session_cleared, the refusal's degraded record). They
// still decode and fold, to no state: the session they named runs on.
func TestJournalWithSessionStopRecordsFolds(t *testing.T) {
	noHarnessEnv(t)
	cfg := MainConfig{Defs: []Registration{{Def: lkDef()}}}
	dir := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	coord := claudeSession("coord-1", t.TempDir())
	var o, e bytes.Buffer
	if code := CLIWithEnv(cfg, []string{"run", "lock-cli", "--state-dir", dir, "--workspace", ws, "--run-id", "old"}, &o, &e, os.Getenv); code != 0 {
		t.Fatalf("run: %d %s", code, e.String())
	}
	j := FileJournal(dir)
	recs, err := j.(committedReader).ReadCommitted("old")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := j.Lock("old")
	if err != nil {
		t.Fatal(err)
	}
	for _, old := range []struct {
		kind JournalKind
		data any
	}{
		{JournalSessionStopped, SessionStoppedData{Harness: "claude", Session: "coord-1", Verb: "next", Text: "next: boom"}},
		{JournalDegraded, DegradedData{Kind: DegradedStoppedSessionRefused, Cause: "next (claude)", Action: "refused: the session was told to stop", Session: "coord-1"}},
		{JournalSessionStopped, SessionStoppedData{Harness: "codex", Session: "other", Verb: "retry", Text: "retry: boom"}},
		{JournalSessionCleared, SessionClearedData{Session: "other"}},
	} {
		raw, err := json.Marshal(old.data)
		if err != nil {
			t.Fatal(err)
		}
		if err := j.Append(JournalRecord{Kind: old.kind, Run: "old", DefHash: recs[0].DefHash, At: time.Now(), Data: raw}); err != nil {
			t.Fatalf("appending %s: %v", old.kind, err)
		}
	}
	unlock()

	// coord-1 was stopped and never cleared in that journal: it runs on.
	var ins []Instruction
	code, out, er := coord(cfg, "next", "old", "1", "--state-dir", dir, "--json")
	if code != 0 || json.Unmarshal([]byte(out), &ins) != nil || len(ins) != 1 || ins[0].Kind != InstructionAgent {
		t.Fatalf("next over the old journal: exit %d: %s %s", code, out, er)
	}
	code, out, er = coord(cfg, "status", "old", "--state-dir", dir, "--full")
	if code != 0 || strings.Contains(out, "stopped session coord-1") || strings.Contains(out, "clear-stop") {
		t.Errorf("status over the old journal: exit %d: %s %s", code, out, er)
	}
	code, out, er = coord(cfg, "journal", "old", "--state-dir", dir)
	if code != 0 || strings.Count(out, "session_stopped") != 2 || strings.Count(out, "session_cleared") != 1 {
		t.Errorf("journal over the old journal: exit %d: %s %s", code, out, er)
	}
}
