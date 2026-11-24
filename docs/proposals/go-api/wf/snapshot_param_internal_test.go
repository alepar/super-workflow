package wf

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSnapshotPathParamMarkersRequireRecordedOwnership(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     any
		paths     map[string]PathBase
		wantError bool
	}{
		{"owned", map[string]any{pathParamMarker: "spec"}, map[string]PathBase{"spec": WorkspaceRelative}, false},
		{"unknown", map[string]any{pathParamMarker: "missing"}, map[string]PathBase{"spec": WorkspaceRelative}, true},
		{"extra-key", map[string]any{pathParamMarker: "spec", "extra": true}, map[string]PathBase{"spec": WorkspaceRelative}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			started, _ := json.Marshal(RunStartedData{Params: map[string]string{"spec": "spec.md"}, ParamPaths: test.paths})
			effect, _ := json.Marshal(map[string]any{"Effect": map[string]any{"Args": map[string]any{"argv": []any{test.value}}}, "Plan": test.value})
			err := snapshotRelocatable([]JournalRecord{{Kind: JournalRunStarted, Data: started}, {Kind: JournalEffectIntent, Data: effect}}, PathRoots{Workspace: t.TempDir()})
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSnapshotLegacyRunPathParamNormalizationOnlyChangesHeader(t *testing.T) {
	workspace, state := t.TempDir(), t.TempDir()
	runDir := filepath.Join(state, "runs", "r")
	d := New("legacy-path")
	d.Param("spec").Path(RunRelative)
	path := filepath.Join(runDir, "records", "spec.md")
	header, _ := json.Marshal(RunStartedData{Params: map[string]string{"spec": path, "ordinary": "literal"}})
	output, _ := json.Marshal(map[string]any{"Output": path})
	records := []JournalRecord{{Kind: JournalRunStarted, Data: header}, {Kind: JournalAccepted, Data: output}}
	if err := normalizeSnapshotParams(records, d, workspace, runDir, state); err != nil {
		t.Fatal(err)
	}
	var started RunStartedData
	if err := json.Unmarshal(records[0].Data, &started); err != nil {
		t.Fatal(err)
	}
	if started.Params["spec"] != "records/spec.md" || started.ParamPaths["spec"] != RunRelative {
		t.Fatalf("header: %+v", started)
	}
	if string(records[1].Data) != string(output) {
		t.Fatal("ordinary output rewritten")
	}
	if err := snapshotRelocatable(records, PathRoots{Workspace: workspace, State: state}); err == nil {
		t.Fatal("opaque source output accepted")
	}
}
