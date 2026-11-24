package wf

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

type pathPlanSeed struct{}
type pathPlanEnd struct{}
type pathPlanOutput struct{ Name string }

func TestPathPlanFreezesExplicitPathsWithAcceptedOutput(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs", "r")
	d := New("path-plan")
	seed := NewCompute[pathPlanSeed, pathPlanOutput](d, func(Ctx) (pathPlanOutput, error) { return pathPlanOutput{Name: "spec.md"}, nil })
	end := NewTerminal[pathPlanEnd](d, Success)
	edge := seed.To(end, "ready").With(ExternalEffect{Adapter: "path", Op: "write", Key: KeyOf("write"), Args: map[string]any{
		"path":     PathTemplate(filepath.Join(runDir, "records", "{Name}")),
		"ref":      Ref[string]{Path: filepath.Join(workspace, "input"), Hash: "hash"},
		"ordinary": "{Name}",
	}})
	seed.Route(func(pathPlanOutput, Ctx) Edge[pathPlanSeed] { return edge }, edge)
	runtime := &runPathRuntime{}
	reg := NewAdapters()
	if err := reg.Register(runtime); err != nil {
		t.Fatal(err)
	}
	j := MemJournal()
	e := NewEngine(j, reg, EngineOptions{RunID: "r", Workspace: workspace, StateRoot: state, RunDir: func(RunID) string { return runDir }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Next(run, ""); err != nil {
		t.Fatal(err)
	}
	if runtime.applied["path"] != filepath.Join(runDir, "records/spec.md") || runtime.applied["ordinary"] != "spec.md" {
		t.Fatalf("runtime args=%#v", runtime.applied)
	}
	unlock, err := j.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := j.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshotRelocatable(records, PathRoots{Workspace: workspace, State: state}); err != nil {
		t.Fatal(err)
	}
	var plan EdgePlan
	for _, rec := range records {
		if rec.Kind == JournalAccepted && rec.State == "pathPlanSeed" {
			var accepted AcceptedData
			if err := json.Unmarshal(rec.Data, &accepted); err != nil {
				t.Fatal(err)
			}
			plan = accepted.Plan
			if strings.Contains(string(rec.Data), workspace) || strings.Contains(string(rec.Data), runDir) {
				t.Fatalf("source paths in plan: %s", rec.Data)
			}
		}
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Effects[0].Args["ordinary"] != "{Name}" {
		t.Fatalf("ordinary plan binding changed: %+v", plan)
	}
	forkWorkspace, forkRun := t.TempDir(), t.TempDir()
	c := &cycle{run: "r", e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return forkRun }}}, st: &runState{started: &RunStartedData{Workspace: forkWorkspace}}}
	args, err := c.resolvePathValues(plan.Steps[0].Effects[0].Args)
	if err != nil {
		t.Fatal(err)
	}
	values := args.(map[string]any)
	if values["path"] != filepath.Join(forkRun, "records/spec.md") || values["ref"].(map[string]any)["Path"] != filepath.Join(forkWorkspace, "input") {
		t.Fatalf("fork plan args=%#v", values)
	}
}
