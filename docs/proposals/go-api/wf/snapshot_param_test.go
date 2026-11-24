package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestSnapshotPathParamsPreserveOwnershipAndLegacyOptIn(t *testing.T) {
	for _, mode := range []string{"typed", "legacy", "untyped", "external"} {
		t.Run(mode, func(t *testing.T) {
			workspace, state := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("world"), 0600); err != nil {
				t.Fatal(err)
			}
			d := snapshotDef()
			param := d.Param("spec")
			value := filepath.Join(workspace, "spec.md")
			if mode == "external" {
				value = filepath.Join(t.TempDir(), "driver")
			}
			if mode == "typed" || mode == "external" {
				param.Path(wf.WorkspaceRelative)
			}
			reg := wf.NewAdapters()
			if err := reg.Register(&snapshotRuntime{}); err != nil {
				t.Fatal(err)
			}
			opts := wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: state, Snapshots: true, RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }}
			e := wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
			run, err := e.Run(d, map[string]string{"spec": value, "ordinary": "leave me alone"}, nil)
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
			if mode == "legacy" || mode == "typed" {
				d = snapshotDef()
				base := wf.WorkspaceRelative
				if mode == "typed" {
					base = wf.RunRelative
				}
				d.Param("spec").Path(base)
				opts.Defs = []*wf.Def{d}
				e = wf.NewEngine(wf.FileJournal(state), reg, opts).(*wf.Engine)
			}
			if err = os.RemoveAll(workspace); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "fork")
			fork, err := e.ForkSnapshot(run, seq, destination)
			if mode == "untyped" {
				// an earlier revision: an untyped param naming the source workspace is
				// rewritten to the fork's, not refused.
				if err != nil {
					t.Fatalf("untyped fork: %v", err)
				}
				copied, err := wf.ReadJournal(fork.StateDir, run)
				if err != nil {
					t.Fatal(err)
				}
				var started wf.RunStartedData
				if err = json.Unmarshal(copied[0].Data, &started); err != nil {
					t.Fatal(err)
				}
				if want := filepath.Join(fork.Workspace, "spec.md"); started.Params["spec"] != want || started.Params["ordinary"] != "leave me alone" {
					t.Fatalf("params: %#v, want spec under the fork's workspace", started.Params)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				copied, err := wf.ReadJournal(fork.StateDir, run)
				if err != nil {
					t.Fatal(err)
				}
				var started wf.RunStartedData
				if err = json.Unmarshal(copied[0].Data, &started); err != nil {
					t.Fatal(err)
				}
				want, base := "spec.md", wf.WorkspaceRelative
				if mode == "external" {
					want, base = value, wf.PathBase("external")
				}
				if started.Params["spec"] != want || started.ParamPaths["spec"] != base || started.Params["ordinary"] != "leave me alone" {
					t.Fatalf("params: %#v ownership: %#v", started.Params, started.ParamPaths)
				}
			}
			after, err := wf.ReadJournal(state, run)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, _ := json.Marshal(after)
			if string(original) != string(unchanged) {
				t.Fatal("source journal changed")
			}
			if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("source recreated: %v", err)
			}
		})
	}
}
