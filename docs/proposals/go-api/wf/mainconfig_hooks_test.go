package wf_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// hookCLI runs the named-binary CLI over def with cfg's hooks set by with.
func hookCLI(def *wf.Def, with func(*wf.MainConfig), args ...string) (int, string, string) {
	cfg := wf.MainConfig{Adapters: clConfig().Adapters, Defs: []wf.Registration{{Def: def}}, Binary: "super"}
	with(&cfg)
	var o, er bytes.Buffer
	code := fixtureCLI(cfg, args, &o, &er)
	return code, o.String(), er.String()
}

// F2: a client names where a successor enters through SuccessorNote, given
// the successor's definition and its records directory (wf.RecordsDir), never
// by matching the engine's stderr.
func TestSuccessorNoteEndsTheContinuingLine(t *testing.T) {
	def := keyedAgentDef("succ-note")
	state := filepath.Join(t.TempDir(), "state")
	ws := t.TempDir()
	first := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x"))
	if err := os.MkdirAll(wf.RecordsDir(state, wf.RunID(first)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf.RecordsDir(state, wf.RunID(first)), "run.md"), []byte("phase: code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	finishOne(t, def, state)

	var gotDef, gotRecords string
	code, out, er := hookCLI(def, func(c *wf.MainConfig) {
		c.SuccessorNote = func(d, records string) string {
			gotDef, gotRecords = d, records
			raw, _ := os.ReadFile(filepath.Join(records, "run.md"))
			return "from " + strings.TrimSpace(strings.TrimPrefix(string(raw), "phase: "))
		}
	}, "run", "--state-dir", state, "--workspace", ws, "--param", "topic=x")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	second := strings.TrimSpace(out)
	if !strings.Contains(er, "continuing as "+second+" from code\n") {
		t.Errorf("stderr lacks the noted successor line: %q", er)
	}
	want, _ := filepath.Abs(wf.RecordsDir(state, wf.RunID(second)))
	if gotDef != "succ-note" || gotRecords != want {
		t.Errorf("SuccessorNote got (%q, %q), want (succ-note, %q)", gotDef, gotRecords, want)
	}
}

// F2: an ended run's text next gains the client's EndedNote, given the
// ended status and the run's records directory. A JSON next does not.
func TestEndedNoteFollowsAnEndedTextNext(t *testing.T) {
	def := keyedAgentDef("ended-note")
	state := filepath.Join(t.TempDir(), "state")
	first := strings.TrimSpace(mustSingle(t, def, "run", "--state-dir", state, "--workspace", t.TempDir(), "--param", "topic=x"))
	finishOne(t, def, state)

	var gotRecords string
	note := func(c *wf.MainConfig) {
		c.EndedNote = func(st wf.RunStatus, records string) string {
			gotRecords = records
			return "handback: here (" + string(st.Run) + ")\n"
		}
	}
	code, out, er := hookCLI(def, note, "wf", "next", "--state-dir", state)
	if code != 7 {
		t.Fatalf("wf next on the ended run exited %d: %s", code, er)
	}
	if !strings.HasSuffix(out, "handback: here ("+first+")\n") {
		t.Errorf("text next lacks the ended note at its end:\n%s", out)
	}
	if want, _ := filepath.Abs(wf.RecordsDir(state, wf.RunID(first))); gotRecords != want {
		t.Errorf("EndedNote records %q, want %q", gotRecords, want)
	}
	if _, out, _ := hookCLI(def, note, "wf", "next", "--json", "--state-dir", state); strings.Contains(out, "handback: here") {
		t.Errorf("a JSON next carries the ended note:\n%s", out)
	}
}

// F2: StateDirFor picks the default state dir of `run <def>` by the entry
// it starts; other verbs keep DefaultStateDir.
func TestStateDirForPicksRunsDefault(t *testing.T) {
	def := keyedAgentDef("dir-for")
	root := t.TempDir()
	t.Chdir(root)
	code, out, er := hookCLI(def, func(c *wf.MainConfig) {
		c.DefaultStateDir = "state/default"
		c.StateDirFor = func(d string) string { return "state/" + d }
	}, "run", "dir-for", "--workspace", t.TempDir(), "--param", "topic=x")
	if code != 0 {
		t.Fatalf("run exited %d: %s", code, er)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "dir-for", "runs", strings.TrimSpace(out))); err != nil {
		t.Errorf("run did not use StateDirFor's directory: %v", err)
	}
}
