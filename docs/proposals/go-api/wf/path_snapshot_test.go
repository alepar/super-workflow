package wf_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type pathSnapshotSeed struct{}
type pathSnapshotAgent struct{}
type pathSnapshotEnd struct{}
type pathSnapshotValue struct {
	File string
	Ref  wf.Ref[string]
}

func TestSnapshotForkResolvesPortableReferencesWithoutSource(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "data"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(state, "runs", "r")
	d := wf.New("portable-snapshot")
	seed := wf.NewCompute[pathSnapshotSeed, pathSnapshotValue](d, func(wf.Ctx) (pathSnapshotValue, error) {
		return pathSnapshotValue{File: wf.PathTemplate(filepath.Join(workspace, "data")), Ref: wf.Ref[string]{Path: filepath.Join(runDir, "records/spec.md"), Hash: "hash"}}, nil
	})
	agent := wf.NewAgent[pathSnapshotAgent, struct{}](d)
	end := wf.NewTerminal[pathSnapshotEnd](d, wf.Success)
	ready := seed.To(agent, "ready")
	seed.Route(func(pathSnapshotValue, wf.Ctx) wf.Edge[pathSnapshotSeed] { return ready }, ready)
	agent.Spec(wf.PromptText("Read {{layout}}", wf.As("layout", wf.Out(seed))), wf.Retry(wf.Const(1), agent.To(end, "stop")))
	done := agent.To(end, "done")
	agent.Route(func(struct{}, wf.Ctx) wf.Edge[pathSnapshotAgent] { return done }, done)
	reg := wf.NewAdapters()
	if err := reg.Register(&snapshotRuntime{}); err != nil {
		t.Fatal(err)
	}
	e := wf.NewEngine(wf.FileJournal(state), reg, wf.EngineOptions{Defs: []*wf.Def{d}, RunID: "r", Workspace: workspace, StateRoot: state, Snapshots: true, RunDir: func(wf.RunID) string { return runDir }}).(*wf.Engine)
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
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
	if seq == 0 {
		t.Fatal("no checkpoint")
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	fork, err := e.ForkSnapshot(run, seq, filepath.Join(t.TempDir(), "fork"))
	if err != nil {
		t.Fatal(err)
	}
	forkEngine := wf.NewEngine(wf.FileJournal(fork.StateDir), reg, wf.EngineOptions{Defs: []*wf.Def{d}, Workspace: fork.Workspace, StateRoot: fork.StateDir, RunDir: func(id wf.RunID) string { return filepath.Join(fork.StateDir, "runs", string(id)) }})
	ins, err := forkEngine.Next(run, "")
	if err != nil || len(ins) != 1 {
		t.Fatalf("fork next=%+v err=%v", ins, err)
	}
	prompt := ins[0].Prompt
	if !strings.Contains(prompt, filepath.Join(fork.Workspace, "data")) || !strings.Contains(prompt, filepath.Join(fork.StateDir, "runs", "r", "records/spec.md")) || strings.Contains(prompt, workspace) || strings.Contains(prompt, runDir) || strings.Contains(prompt, "$wf.path") {
		t.Fatalf("fork prompt=%s", prompt)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("source recreated")
	}
}
