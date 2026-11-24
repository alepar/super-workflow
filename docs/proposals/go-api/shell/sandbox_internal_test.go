package shell

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// A detached command of a sandboxed run runs with the run's bd shim first on
// PATH, a bare bd included, as an inline one does; a sandboxed launch with
// no shim is refused before anything runs (bd config review round 1).
func TestADetachedCommandOfASandboxedRunCallsBdSandboxed(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\necho \"$* db=${BEADS_DB:-}\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEADS_DB", "/elsewhere/beads.db")
	bin, err := wf.WriteSandboxShim(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"bd", "close", "x-1"}, {"sh", "-c", "bd close x-1"}} {
		os.WriteFile(log, nil, 0o644)
		if res := runArgv(argv, dir, filepath.Join(dir, "full"), bin); res.Exit != 0 {
			t.Fatalf("%v: %+v", argv, res)
		}
		if calls, _ := os.ReadFile(log); string(calls) != "--sandbox close x-1 db=\n" {
			t.Errorf("%v: bd saw %q", argv, calls)
		}
		os.WriteFile(log, nil, 0o644)
		runArgv(argv, dir, filepath.Join(dir, "full"), "")
		if calls, _ := os.ReadFile(log); string(calls) != "close x-1 db=/elsewhere/beads.db\n" {
			t.Errorf("%v unsandboxed: bd saw %q", argv, calls)
		}
	}
	if !detachedSupported {
		return
	}
	req := wf.DetachedRequest{Context: wf.EffectContext{Run: "r", Sandboxed: true}, Argv: []string{"bd", "close", "x-1"}, Dir: dir,
		ResultPath: filepath.Join(dir, "e", "1.json"), ProcPath: filepath.Join(dir, "e", "1.proc"), LogPath: filepath.Join(dir, "e", "1.log"), Mode: wf.DetachProcess}
	if _, err := (detached{bin: os.Args[0]}).Start(req); !errors.Is(err, wf.ErrNotLaunched) || !strings.Contains(err.Error(), "no bd shim") {
		t.Errorf("a sandboxed launch with no shim: %v, want refused before it launched", err)
	}
}
