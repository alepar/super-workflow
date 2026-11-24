package wf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPathParamsNormalizeAndResolve(t *testing.T) {
	workspace, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs/r")
	d := New("paths")
	for _, name := range []string{"workspace", "run", "external", "relative", "root"} {
		d.Param(name).Path(WorkspaceRelative)
	}
	d.Param("runRelative").Path(RunRelative)
	original := map[string]string{"workspace": filepath.Join(workspace, "config"), "run": filepath.Join(runDir, "input"), "external": filepath.Join(outside, "driver"), "relative": "cfg/input", "root": workspace, "runRelative": "records/input", "ordinary": filepath.Join(state, "opaque")}
	normalized, bases, err := normalizePathParams(d, original, workspace, runDir, state)
	if err != nil {
		t.Fatal(err)
	}
	if normalized["workspace"] != "config" || normalized["run"] != "input" || normalized["root"] != "." || bases["run"] != RunRelative || bases["external"] != externalPathBase {
		t.Fatalf("normalized: %v %v", normalized, bases)
	}
	if normalized["ordinary"] != original["ordinary"] || original["workspace"] != filepath.Join(workspace, "config") {
		t.Fatal("ordinary value or caller map changed")
	}
	forkWorkspace, forkRun := t.TempDir(), t.TempDir()
	c := &cycle{run: "r", e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return forkRun }}}, st: &runState{started: &RunStartedData{Workspace: forkWorkspace, Params: normalized, ParamPaths: bases}}}
	for name, want := range map[string]string{"workspace": filepath.Join(forkWorkspace, "config"), "run": filepath.Join(forkRun, "input"), "root": forkWorkspace, "external": original["external"], "ordinary": original["ordinary"], "runRelative": filepath.Join(forkRun, "records/input")} {
		if got, err := c.paramValue(name); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", name, got, err, want)
		}
	}
}

func TestPathParamsRejectInvalidAndSourceState(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	run := filepath.Join(state, "runs/current")
	d := New("paths")
	d.Param("path").Path(WorkspaceRelative)
	for _, value := range []string{"", "../escape", "a/../b", "a//b", filepath.Join(state, "runs/other/file"), state} {
		if _, _, err := normalizePathParams(d, map[string]string{"path": value}, workspace, run, state); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	if _, _, err := normalizePathParams(d, nil, workspace, run, state); err != nil {
		t.Fatalf("missing optional parameter: %v", err)
	}
}

func TestPathParamDeclarationHashAndLint(t *testing.T) {
	makeDef := func(base PathBase) *Def {
		d := New("path-hash")
		p := d.Param("path")
		if base != "" {
			p.Path(base)
		}
		NewTerminal[runPathDone](d, Success)
		return d
	}
	ordinary := makeDef("")
	raw, _ := json.Marshal(Inspect(ordinary))
	if strings.Contains(string(raw), "ParamPaths") {
		t.Fatal("ordinary definition gained path metadata")
	}
	a, b := makeDef(WorkspaceRelative), makeDef(RunRelative)
	if a.Hash() == b.Hash() || a.Hash() == ordinary.Hash() {
		t.Fatal("path declaration missing from hash")
	}
	if !reflect.DeepEqual(Inspect(a).ParamPaths, map[string]PathBase{"path": WorkspaceRelative}) {
		t.Fatal("inspect omitted declarations")
	}
	for _, d := range []*Def{makeDef(PathBase("bad")), func() *Def { d := makeDef(WorkspaceRelative); d.Param("path").Path(RunRelative); return d }()} {
		if len(d.Lint()) == 0 {
			t.Fatal("invalid/conflicting declaration not linted")
		}
	}
}

func TestPathParamSymlinkRefusedByContext(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	if err := os.Symlink(source, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	d := New("path-context")
	param := d.Param("input").Path(WorkspaceRelative)
	c := &cycle{st: &runState{started: &RunStartedData{Workspace: root, Params: map[string]string{"input": "escape/file"}, ParamPaths: map[string]PathBase{"input": WorkspaceRelative}}}}
	ctx := &runCtx{c: c, in: &instance{def: d}}
	if got := ctx.Param(param); got != "" || !ctx.blocked {
		t.Fatalf("context followed escape: %q blocked=%v", got, ctx.blocked)
	}
}

func TestPathParamsRunStartPersistsNormalizedMetadata(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	d := New("path-start")
	d.Param("input").Path(WorkspaceRelative)
	NewTerminal[runPathDone](d, Success)
	journal := MemJournal()
	engine := NewEngine(journal, NewAdapters(), EngineOptions{Workspace: workspace, StateRoot: state, RunID: "r", RunDir: func(id RunID) string { return filepath.Join(state, "runs", string(id)) }})
	run, err := engine.Run(d, map[string]string{"input": filepath.Join(workspace, "input"), "plain": "unchanged"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := journal.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	records, err := journal.Records(run)
	if err != nil {
		t.Fatal(err)
	}
	var started RunStartedData
	if err := json.Unmarshal(records[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if started.Params["input"] != "input" || started.ParamPaths["input"] != WorkspaceRelative || started.Params["plain"] != "unchanged" {
		t.Fatalf("started: %+v", started)
	}
}

func TestPathParamsSuccessorPreservesRecordedBase(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	oldRun, newRun := filepath.Join(state, "runs/old"), filepath.Join(state, "runs/new")
	d := New("successor")
	d.Param("input").Path(WorkspaceRelative)
	previous := &RunStartedData{Workspace: workspace, Params: map[string]string{"input": "records/input"}, ParamPaths: map[string]PathBase{"input": RunRelative}}
	for _, params := range []map[string]string{nil, {"input": "records/input"}, {"input": filepath.Join(oldRun, "records/input")}} {
		values, bases, err := normalizeSuccessorPathParams(d, params, workspace, newRun, state, oldRun, previous)
		if err != nil || values["input"] != "records/input" || bases["input"] != RunRelative {
			t.Fatalf("successor = %v %v %v", values, bases, err)
		}
	}
}

type pathParamSub struct{}
type pathParamMap struct{}

func TestPathParamsCollectChildDeclarations(t *testing.T) {
	for _, kind := range []string{"sub", "map"} {
		t.Run(kind, func(t *testing.T) {
			child := New("path-child")
			child.Param("config").Path(WorkspaceRelative)
			terminal := NewTerminal[runPathDone](child, Success)
			Yield(terminal, func(Ctx) bool { return true })
			parent := New("path-parent")
			typed := Typed[string, bool](child)
			if kind == "sub" {
				NewSub[pathParamSub](parent, typed, func(Ctx) string { return "item" })
			} else {
				NewMap[pathParamMap](parent, FromObservation(Observe[[]string]("test", "items", nil)), func(s string) string { return s }, typed)
			}
			root := t.TempDir()
			values, bases, err := normalizePathParams(parent, map[string]string{"config": filepath.Join(root, "config")}, root, "", "")
			if err != nil || values["config"] != "config" || bases["config"] != WorkspaceRelative {
				t.Fatalf("child path declaration ignored: %v %v %v", values, bases, err)
			}
			parent.Param("config").Path(RunRelative)
			if _, _, err := normalizePathParams(parent, nil, root, root, ""); err == nil {
				t.Fatal("conflicting parent/child bases accepted")
			}
			conflict := false
			for _, problem := range parent.Lint() {
				if strings.Contains(problem.Msg, "conflicting path bases") {
					conflict = true
				}
			}
			if !conflict {
				t.Fatal("lint missed conflicting parent/child bases")
			}
		})
	}
}

func TestPathParamsRelativeStateCandidatesMatchAbsolute(t *testing.T) {
	workspace := t.TempDir()
	state := filepath.Join(workspace, ".state")
	run := filepath.Join(state, "runs/current")
	d := New("equivalent-paths")
	d.Param("input").Path(WorkspaceRelative)
	for _, relative := range []string{".state", ".state/runs/other/input", ".state/runs/current/input", "config"} {
		relValues, relBases, relErr := normalizePathParams(d, map[string]string{"input": relative}, workspace, run, state)
		absValues, absBases, absErr := normalizePathParams(d, map[string]string{"input": filepath.Join(workspace, relative)}, workspace, run, state)
		if (relErr == nil) != (absErr == nil) || !reflect.DeepEqual(relValues, absValues) || !reflect.DeepEqual(relBases, absBases) {
			t.Errorf("%s differs: relative %v %v %v; absolute %v %v %v", relative, relValues, relBases, relErr, absValues, absBases, absErr)
		}
		if relative == ".state/runs/current/input" && (relBases["input"] != RunRelative || relValues["input"] != "input") {
			t.Errorf("own run path not normalized: %v %v", relValues, relBases)
		}
	}
}
