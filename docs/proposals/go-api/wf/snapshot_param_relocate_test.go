package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

// an earlier revision: every combined-pin run carries an untyped param naming its
// workspace inside a shell command (the finish hand-off: `cat >
// <workspace>/.superpowers/finish-handoff.md`). A fork rewrites the source
// root in such a param to its own, at path boundaries only, and names each
// param it rewrote in its rewound record, instead of refusing the fork.
func TestSnapshotForkRelocatesAnUntypedParamNamingTheSourceRoot(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("world"), 0600); err != nil {
		t.Fatal(err)
	}
	d := snapshotDef()
	d.Param("handoff")
	d.Param("sibling")
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: state, Snapshots: true, RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }}
	e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
	params := map[string]string{
		"handoff":  "cat > " + filepath.Join(workspace, ".superpowers", "finish-handoff.md"),
		"sibling":  "ls " + workspace + "-other/x", // shares the root's spelling, not its path
		"ordinary": "leave me alone",
	}
	run, err := e.Run(d, params, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	records, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(records)
	var seq int64
	for _, r := range records {
		if r.Kind == wf.JournalWorldSnapshot {
			seq = r.Seq
		}
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatalf("fork of a run whose untyped param names its workspace: %v", err)
	}
	copied, err := wf.ReadJournal(fork.StateDir, run)
	if err != nil {
		t.Fatal(err)
	}
	var started wf.RunStartedData
	if err = json.Unmarshal(copied[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if want := "cat > " + filepath.Join(fork.Workspace, ".superpowers", "finish-handoff.md"); started.Params["handoff"] != want {
		t.Errorf("Params.handoff = %q, want %q: the hand-off writes into the fork's copy", started.Params["handoff"], want)
	}
	if started.Params["sibling"] != params["sibling"] || started.Params["ordinary"] != params["ordinary"] {
		t.Errorf("params = %#v: only a path under the source root is rewritten", started.Params)
	}
	var rd wf.RewoundData
	for _, r := range copied {
		if r.Kind == wf.JournalRewound {
			if err := json.Unmarshal(r.Data, &rd); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slices.Equal(rd.Relocated, []string{"Params.handoff"}) {
		t.Errorf("rewound Relocated = %q, want [Params.handoff]: the fork says what it rewrote", rd.Relocated)
	}
	after, err := wf.ReadJournal(state, run)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged, _ := json.Marshal(after); string(original) != string(unchanged) {
		t.Fatal("source journal changed")
	}
}
