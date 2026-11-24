package wf

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedPathSetup struct{}
type recordedPathCheck struct{}
type recordedPathDone struct{}
type recordedPathOutput struct {
	Ref      Ref[string]
	Template string
}

func TestRecordedComputedOwnedPathsKeepProducerAndCalibrate(t *testing.T) {
	workspace, runDir := t.TempDir(), t.TempDir()
	d := New("recorded-owned-path")
	setup := NewCompute[recordedPathSetup, recordedPathOutput](d, func(Ctx) (recordedPathOutput, error) {
		return recordedPathOutput{Ref: Ref[string]{Path: filepath.Join(runDir, "records/spec.md"), Hash: "sha"}, Template: PathTemplate("docs/input.json#section")}, nil
	})
	check := NewAgent[recordedPathCheck, bool](d)
	done := NewTerminal[recordedPathDone](d, Success)
	ready := setup.To(check, "check")
	setup.Route(func(recordedPathOutput, Ctx) Edge[recordedPathSetup] { return ready }, ready)
	check.Spec(PromptText("Check the paths."), Retry(Const(1), check.To(done, "exhausted")), Checkf("owned paths", func(_ bool, c Ctx) error {
		value := ComputedOf(c, setup)
		if value.Ref.Path != filepath.Join(runDir, "records/spec.md") || value.Template != filepath.Join(workspace, "docs/input.json")+"#section" {
			return fmt.Errorf("incorrect computed view: %+v", value)
		}
		return nil
	}))
	finish := check.To(done, "done")
	check.Route(func(bool, Ctx) Edge[recordedPathCheck] { return finish }, finish)
	journal := MemJournal()
	e := NewEngine(journal, NewAdapters(), EngineOptions{Workspace: workspace, RunDir: func(RunID) string { return runDir }})
	run, err := e.Run(d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	instructions, err := e.Next(run, "")
	if err != nil || len(instructions) != 1 {
		t.Fatalf("next: %+v %v", instructions, err)
	}
	result, err := e.Submit(run, Submission{Token: instructions[0].Token, Output: json.RawMessage(`true`)})
	if err != nil || result.Class != Accepted {
		t.Fatalf("submit: %+v %v", result, err)
	}
	unlock, err := journal.Lock(run)
	if err != nil {
		t.Fatal(err)
	}
	records, err := journal.Records(run)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	var producer, accept JournalRecord
	var accepted AcceptedData
	for _, record := range records {
		if record.Kind == JournalAccepted {
			if record.State == "recordedPathSetup" {
				producer = record
			}
			if record.State == "recordedPathCheck" {
				accept = record
				if err := json.Unmarshal(record.Data, &accepted); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	original := string(producer.Data)
	if !strings.Contains(original, ownedPathMarker) {
		t.Fatalf("producer lacks durable markers: %s", original)
	}
	if accepted.Context == nil || len(accepted.Context.Checks) != 1 {
		t.Fatalf("context missing: %+v", accepted.Context)
	}
	recorded := accepted.Context.Checks[0]
	if len(recorded.Incomplete) != 0 || len(recorded.Reads) != 1 || recorded.Reads[0].Seq != producer.Seq {
		t.Fatalf("producer provenance lost: %+v", recorded)
	}
	if recorded.Reads[0].PathView == nil || recorded.Reads[0].PathView.Workspace != workspace || recorded.Reads[0].PathView.RunDir != runDir || len(recorded.Reads[0].Value) != 0 {
		t.Fatalf("path view replaced producer reference: %+v", recorded.Reads[0])
	}
	// Historical fixture reconstruction must not inspect the original files.
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(runDir); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCheckContexts(accepted.Context, accept, records)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range Inspect(d).States {
		if state.Name == "recordedPathCheck" {
			results, err := state.CheckRecordedOutput(json.RawMessage(`true`), resolved)
			if err != nil || len(results) != 0 {
				t.Fatalf("calibration: %v %v", results, err)
			}
		}
	}
	if string(producer.Data) != original {
		t.Fatal("harvesting changed historical producer")
	}
}

func TestRecordedOwnedPathViewValidatesRootsWithoutFilesystem(t *testing.T) {
	raw := json.RawMessage(`{"Path":{"$wf.path":{"base":"run","path":"records/spec.md"}}}`)
	for _, roots := range []*RecordedPathView{nil, {}, {RunDir: "relative"}} {
		if _, _, err := recordedOwnedPathView(raw, roots); err == nil {
			t.Errorf("invalid roots accepted: %+v", roots)
		}
	}
	external := json.RawMessage(`{"Path":{"$wf.path":{"base":"external","path":"/external/spec.md","fragment":"#section"}}}`)
	got, marked, err := recordedOwnedPathView(external, nil)
	if err != nil || !marked || string(got) != `{"Path":"/external/spec.md#section"}` {
		t.Fatalf("pinned external needs no root: %s %v %v", got, marked, err)
	}
}
