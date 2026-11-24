package wf

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDecodeRunValueResolvesExplicitPathsOnly(t *testing.T) {
	workspace, runDir := t.TempDir(), t.TempDir()
	header, _ := json.Marshal(RunStartedData{Workspace: workspace, Params: map[string]string{"exe": "bin/tool"}, ParamPaths: map[string]PathBase{"exe": WorkspaceRelative}})
	records := []JournalRecord{{Run: "r", Kind: JournalRunStarted, Data: header}}
	raw := json.RawMessage(`[{"$wf.path":{"base":"run","path":"records/spec.json","fragment":"#/part"}},{"$wf.pathParam":"exe"},{"$wf.runPath":"records/old"},"plain"]`)
	var got []string
	if err := DecodeRunValue(records, runDir, raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(runDir, "records/spec.json") + "#/part", filepath.Join(workspace, "bin/tool"), filepath.Join(runDir, "records/old"), "plain"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value=%v want=%v", got, want)
		}
	}
	if string(records[0].Data) != string(header) {
		t.Fatal("header mutated")
	}
	if !json.Valid(raw) {
		t.Fatal("raw value changed")
	}
	if err := DecodeRunValue(records, runDir, json.RawMessage(`{"$wf.pathParam":"missing"}`), new(string)); err == nil {
		t.Fatal("unknown path parameter accepted")
	}
}
