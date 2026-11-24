package wf

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPathParameterBindingKeepsDurableReferences(t *testing.T) {
	workspace, runDir := t.TempDir(), t.TempDir()
	d := New("path-params")
	exe := d.Param("exe").Path(WorkspaceRelative)
	c := &cycle{run: "run", e: &Engine{opts: EngineOptions{RunDir: func(RunID) string { return runDir }}}, st: &runState{started: &RunStartedData{Workspace: workspace, Params: map[string]string{"exe": "bin/super", "plain": "literal"}, ParamPaths: map[string]PathBase{"exe": WorkspaceRelative}}}}
	scope := c.scope(&instance{def: d})
	want := filepath.Join(workspace, "bin/super")
	if got, ok := scope.lookup("exe"); !ok || got != want {
		t.Fatalf("lookup %q %v", got, ok)
	}
	inputs, err := scope.inputValues([]Input{exe})
	if err != nil || len(inputs) != 1 || inputs[0].value != want {
		t.Fatalf("prompt input %+v %v", inputs, err)
	}
	argv, err := scope.bindArgv([]string{"{exe}", "{plain}"})
	marker := map[string]any{pathParamMarker: "exe"}
	if err != nil || !reflect.DeepEqual(argv, []any{marker, "literal"}) {
		t.Fatalf("durable argv %#v %v", argv, err)
	}
	effect, err := scope.bindEffect(EffectData{Kind: "external", Adapter: "shell", Op: "run", Args: map[string]any{"argv": []string{"{exe}", "{plain}"}}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(effect.Args)
	fork := t.TempDir()
	c.st.started.Workspace = fork
	resolved, err := c.resolvePathValues(effect.Args)
	if err != nil {
		t.Fatal(err)
	}
	got := resolved.(map[string]any)["argv"].([]any)
	if got[0] != filepath.Join(fork, "bin/super") || got[1] != "literal" {
		t.Fatalf("operational argv %#v", got)
	}
	after, _ := json.Marshal(effect.Args)
	if string(before) != string(after) {
		t.Fatal("resolution mutated durable effect")
	}
	if _, err := c.resolvePathValues(map[string]any{pathParamMarker: "plain"}); err == nil {
		t.Fatal("untyped parameter accepted as path marker")
	}
}
