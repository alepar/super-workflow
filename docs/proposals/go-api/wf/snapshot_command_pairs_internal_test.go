package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// commandJournal is a command visit of instance "" state X: its intent,
// its result, then a Computed acceptance with out.
func commandJournal(t *testing.T, ok bool, result, out RunResult) []JournalRecord {
	t.Helper()
	enc := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return []JournalRecord{
		{Seq: 1, Kind: JournalEffectIntent, State: "X", Data: enc(EffectIntentData{Command: true, Effect: EffectData{Kind: "external", Adapter: "shell", Op: "run", Args: map[string]any{"argv": []any{"suite"}}}})},
		{Seq: 2, Kind: JournalEffectResult, State: "X", Data: enc(EffectResultData{Intent: 1, OK: ok, Result: enc(result)})},
		{Seq: 3, Kind: JournalAccepted, State: "X", Data: enc(AcceptedData{Output: enc(out), Computed: true})},
	}
}

// Only the acceptance that routes this very intent's OK result is a
// command's printed history (an earlier revision review round 1): a failed visit, or an
// acceptance whose output is not that result (a migrated or reused state
// name), is checked as any output is.
func TestCommandAcceptanceIsExact(t *testing.T) {
	src := PathRoots{Workspace: "/src/ws", State: "/src/state"}
	printed := RunResult{Stdout: "root: /src/ws\n"}
	if err := CheckJournalRelocation(commandJournal(t, true, printed, printed), src); err != nil {
		t.Fatalf("a command's own printed output: %v", err)
	}
	if err := CheckJournalRelocation(commandJournal(t, false, printed, printed), src); err == nil {
		t.Fatal("an acceptance after a failed command visit was taken for the command's")
	}
	if err := CheckJournalRelocation(commandJournal(t, true, RunResult{Stdout: "other"}, printed), src); err == nil {
		t.Fatal("an acceptance that is not the result was taken for the command's")
	}
}

func TestCopyCommandOutputsRefusesLinksAndFIFOs(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	effects := filepath.Join(state, "effects")
	if err := os.Mkdir(effects, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(outside, "1.out")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(effects, "1.out")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(state, "linked")
	if err := os.Symlink(outside, linkedParent); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(effects, "2.out")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, from string }{
		{"leaf link", link}, {"parent link", filepath.Join(linkedParent, "1.out")}, {"fifo", pipe},
	} {
		t.Run(c.name, func(t *testing.T) {
			to := filepath.Join(t.TempDir(), "copy")
			done := make(chan error, 1)
			go func() { done <- copyCommandOutputs(map[string]string{c.from: to}) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("copied from an unsafe source")
				}
			case <-time.After(200 * time.Millisecond):
				// Release a reader blocked in open on the old implementation.
				w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0)
				if err == nil {
					w.Close()
					<-done
				}
				t.Fatal("copy blocked on its source")
			}
			if _, err := os.Lstat(to); !os.IsNotExist(err) {
				t.Fatalf("destination exists: %v", err)
			}
		})
	}
}

// A command result's Output is the engine's full-output file and nothing
// else: under the source run it relocates with the fork; any other path
// there is refused.
func TestCommandOutputFileRelocates(t *testing.T) {
	src := PathRoots{Workspace: "/src/ws", State: "/src/state"}
	own := RunResult{Stdout: "tail", Output: "/src/state/runs/r/effects/1.out"}
	if err := CheckJournalRelocation(commandJournal(t, true, own, own), src); err != nil {
		t.Fatalf("the engine's own output file: %v", err)
	}
	other := RunResult{Stdout: "tail", Output: "/src/state/runs/other/notes.txt"}
	if err := CheckJournalRelocation(commandJournal(t, true, other, other), src); err == nil || !strings.Contains(err.Error(), "Output.Output") {
		t.Fatalf("another path as Output: %v", err)
	}
	recs := commandJournal(t, true, own, own)
	to := PathRoots{Workspace: "/fork/ws", State: "/fork/state"}
	files, err := relocateCommandOutputs(recs, src, to)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/fork/state", "runs", "r", "effects", "1.out")
	if files[own.Output] != want || len(files) != 1 {
		t.Fatalf("files to carry %v", files)
	}
	var res EffectResultData
	var acc AcceptedData
	var rr, ra RunResult
	_ = json.Unmarshal(recs[1].Data, &res)
	_ = json.Unmarshal(res.Result, &rr)
	_ = json.Unmarshal(recs[2].Data, &acc)
	_ = json.Unmarshal(acc.Output, &ra)
	if rr.Output != want || ra.Output != want || ra.Stdout != "tail" {
		t.Fatalf("relocated %q and %q, want %q", rr.Output, ra.Output, want)
	}
}
