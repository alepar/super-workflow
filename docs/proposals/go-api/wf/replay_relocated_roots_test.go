package wf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/shell"
	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type replayRootDiscovery struct{}
type replayRootDone struct{}

func TestReplayRelocatedAbsoluteObservationRoots(t *testing.T) {
	workspace := t.TempDir()
	state := filepath.Join(workspace, "state")
	d := wf.New("replay-root-discovery")
	d.Param("record").Path(wf.RunRelative)
	d.Param("local").Path(wf.WorkspaceRelative)
	observation := shell.Observe(shell.Run("printf", "%s", "{record}", "{local}"))
	discovery := wf.NewCompute[replayRootDiscovery, bool](d, func(c wf.Ctx) (bool, error) {
		_, err := wf.Observed(c, observation)
		return true, err
	}).Observes(observation)
	done := wf.NewTerminal[replayRootDone](d, wf.Success)
	edge := discovery.To(done, "discovered").With(shell.Effect(wf.KeyOf("discovery", "reset"), shell.Run("printf", "%s", wf.PathTemplate("{record}"), wf.PathTemplate("{local}"))))
	discovery.Route(func(bool, wf.Ctx) wf.Edge[replayRootDiscovery] { return edge }, edge)
	h := newHarnessOpts(t, d, shell.Runtime(shell.Config{}), wf.EngineOptions{Workspace: workspace, StateRoot: state, RunDir: func(run wf.RunID) string { return filepath.Join(state, "runs", string(run)) }})
	run := h.run(d, map[string]string{"record": "records/run.md", "local": "docs/spec.md"})
	h.next(run)
	records := h.records(run)
	before, _ := json.Marshal(records)
	for _, source := range []string{filepath.Join(state, "runs", string(run)), filepath.Join(t.TempDir(), "runs", string(run))} {
		report, err := wf.Replay(records, d, source)
		if err != nil || report.Stop.Kind != "ended" || len(report.Differences) != 0 {
			t.Fatalf("relocated discovery: %v\n%s", err, report)
		}
	}
	for _, target := range []struct {
		kind wf.JournalKind
		stop string
	}{{wf.JournalObserved, "read"}, {wf.JournalEffectIntent, "effect"}} {
		for _, change := range []struct{ name, from, to string }{
			{"different-path", "records/run.md", "records/other.md"},
			{"different-command", `"printf"`, `"different-command"`},
		} {
			t.Run(target.stop+"/"+change.name, func(t *testing.T) {
				changed := append([]wf.JournalRecord(nil), records...)
				found := false
				for i := range changed {
					if changed[i].Kind == target.kind && strings.Contains(string(changed[i].Data), change.from) {
						changed[i].Data = json.RawMessage(strings.ReplaceAll(string(changed[i].Data), change.from, change.to))
						found = true
					}
				}
				if !found {
					t.Fatal("regression did not change a recorded argument")
				}
				report, err := wf.Replay(changed, d, filepath.Join(t.TempDir(), "runs", string(run)))
				if err != nil || report.Stop.Kind != target.stop {
					t.Fatalf("normalization hid changed argument: %v\n%s", err, report)
				}
			})
		}
	}
	after, _ := json.Marshal(records)
	if string(before) != string(after) {
		t.Fatal("replay mutated source journal")
	}
}
