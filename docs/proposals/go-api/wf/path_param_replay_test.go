package wf

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReplayPreservesRecordedPathParamOwnership(t *testing.T) {
	for _, declaration := range []PathBase{"", WorkspaceRelative, RunRelative} {
		t.Run(string(declaration), func(t *testing.T) {
			workspace, oldDir, newDir := t.TempDir(), t.TempDir(), t.TempDir()
			d := New("replay-path")
			if declaration != "" {
				d.Param("input").Path(declaration)
			}
			NewTerminal[runPathDone](d, Success)
			started := &RunStartedData{Def: d.name, Workspace: workspace, Params: map[string]string{"input": "records/input"}, ParamPaths: map[string]PathBase{"input": RunRelative}}
			raw, err := json.Marshal(started)
			if err != nil {
				t.Fatal(err)
			}
			r := &replay{rec: []JournalRecord{{Kind: JournalRunStarted, Run: "r", Data: raw}}, def: d, run: "r", started: started, oldDir: oldDir, newDir: newDir}
			r.index()
			if _, err := r.drive(); err != nil {
				t.Fatal(err)
			}
			unlock, err := r.j.Lock("r")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			records, err := r.j.Records("r")
			if err != nil {
				t.Fatal(err)
			}
			var got RunStartedData
			if err := json.Unmarshal(records[0].Data, &got); err != nil {
				t.Fatal(err)
			}
			if got.ParamPaths["input"] != RunRelative || got.Params["input"] != "records/input" {
				t.Fatalf("ownership changed: %+v", got)
			}
			c := &cycle{e: r.engine, run: "r", st: &runState{started: &got}}
			if value, err := c.paramValue("input"); err != nil || value != filepath.Join(newDir, "records/input") {
				t.Fatalf("value=%q err=%v", value, err)
			}
			if started.ParamPaths["input"] != RunRelative || started.Params["input"] != "records/input" {
				t.Fatal("source mutated")
			}
		})
	}
}

func TestReplayPreservesHistoricallyPlainParams(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			d := New("replay-plain")
			d.Param("input").Path(WorkspaceRelative)
			d.Param("typed").Path(WorkspaceRelative)
			NewTerminal[runPathDone](d, Success)
			started := &RunStartedData{Def: d.name, Workspace: t.TempDir(), Params: map[string]string{"input": "records/input", "typed": "records/typed"}}
			if partial {
				started.ParamPaths = map[string]PathBase{"typed": RunRelative}
			}
			raw, err := json.Marshal(started)
			if err != nil {
				t.Fatal(err)
			}
			r := &replay{rec: []JournalRecord{{Kind: JournalRunStarted, Run: "r", Data: raw}}, def: d, run: "r", started: started, oldDir: t.TempDir(), newDir: t.TempDir()}
			r.index()
			if _, err := r.drive(); err != nil {
				t.Fatal(err)
			}
			unlock, err := r.j.Lock("r")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			records, err := r.j.Records("r")
			if err != nil {
				t.Fatal(err)
			}
			var got RunStartedData
			if err := json.Unmarshal(records[0].Data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Params, started.Params) || !reflect.DeepEqual(got.ParamPaths, started.ParamPaths) {
				t.Fatalf("historical parameters changed: %+v", got)
			}
			c := &cycle{e: r.engine, run: "r", st: &runState{started: &got}}
			if value, err := c.paramValue("input"); err != nil || value != "records/input" {
				t.Fatalf("plain value=%q err=%v", value, err)
			}
			if after, _ := json.Marshal(started); string(after) != string(raw) {
				t.Fatal("source mutated")
			}
		})
	}
}
