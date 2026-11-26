package wf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestSnapshotForkRefusesUntypedSourcePathsBeforeWriting(t *testing.T) {
	for _, root := range []string{"state", "workspace", "state-embedded", "workspace-embedded", "state-dot", "workspace-dot", "state-parent", "workspace-parent", "state-double", "workspace-double", "state-dot-embedded", "state-root", "workspace-root"} {
		t.Run(root, func(t *testing.T) {
			embedded := strings.HasSuffix(root, "-embedded")
			root = strings.TrimSuffix(root, "-embedded")
			variant := ""
			if i := strings.IndexByte(root, '-'); i >= 0 {
				variant, root = root[i+1:], root[:i]
			}
			workspace, state := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("source"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "worktrees", "legacy")
			if root == "workspace" {
				path = filepath.Join(workspace, "worktrees", "legacy")
			}
			switch variant {
			case "root":
				path = strings.Replace(filepath.Dir(filepath.Dir(path)), "/", "/./", 1)
			case "dot":
				path = strings.Replace(path, "/", "/./", 1)
			case "parent":
				path = strings.Replace(path, "/", "/unused/../", 1)
			case "double":
				path = strings.ReplaceAll(path, "/", "//")
			}
			if embedded {
				path = "cat \"" + path + "\" | head"
			}
			d := wf.New("legacy-snapshot")
			a := wf.NewAgent[snapshotStart, string](d)
			done := wf.NewTerminal[snapshotDone](d, wf.Success)
			a.Spec(wf.PromptText("work"), wf.Retry[snapshotStart](wf.Const(1), a.To(done, "stop")))
			finish := a.To(done, "finished")
			a.Route(func(string, wf.Ctx) wf.Edge[snapshotStart] { return finish }, finish)
			reg := wf.NewAdapters()
			_ = reg.Register(&snapshotRuntime{})
			e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{
				Defs: []*wf.Def{d}, Workspace: workspace, StateRoot: state, Snapshots: true,
				RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) },
			}).(*wf.Engine)
			// The run's options are engine-owned operational state a fork
			// carries as recorded: an untyped source path there refuses it.
			run, err := e.Run(d, nil, map[string]string{"tool": path})
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
			var seq int64
			for _, rec := range records {
				if rec.Kind == wf.JournalWorldSnapshot {
					seq = rec.Seq
				}
			}
			out := filepath.Join(t.TempDir(), "fork")
			_, err = e.ForkSnapshot(run, seq, out)
			if err == nil || !strings.Contains(err.Error(), "untyped paths under the source "+root+" root") {
				t.Fatalf("legacy fork: %v", err)
			}
			if _, err = os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote destination: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(workspace, "data")); err != nil || string(got) != "source" {
				t.Fatalf("source changed: %q, %v", got, err)
			}
		})
	}
}

// unrelocatableOutput is a command runtime whose result names path as its
// full output: an engine-owned path the fork would read, outside the
// engine's own effects directory, so the fork-time check refuses it (a
// plan's effect args stop being read once the transition consumed it).
type unrelocatableOutput struct{ path string }

func (*unrelocatableOutput) Ops() []string { return []string{"shell.run"} }
func (*unrelocatableOutput) Read(string, wf.ObservationDesc) (json.RawMessage, error) {
	return nil, nil
}
func (u *unrelocatableOutput) Apply(string, wf.ExternalEffect) (json.RawMessage, error) {
	return json.Marshal(wf.RunResult{Output: u.path})
}
func (*unrelocatableOutput) Check(string, wf.ExternalEffect) (wf.EffectStatus, error) {
	return wf.EffectNotDone, nil
}
