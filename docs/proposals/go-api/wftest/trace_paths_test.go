package wftest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

type tracePathCompute struct{}
type tracePathDone struct{}
type tracePathResult struct {
	Ref          wf.Ref[string]
	WorkspaceRef wf.Ref[string]
	Template     string
	Plain        string
}

func TestTraceDecodeValueResolvesComputedPathsWithoutChangingJournal(t *testing.T) {
	for _, withTB := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("withTB=%t/workspace=%t", withTB, override), func(t *testing.T) {
				d := wf.New("trace-paths")
				compute := wf.NewCompute[tracePathCompute, tracePathResult](d, func(wf.Ctx) (tracePathResult, error) {
					return tracePathResult{
						Ref:          wf.Ref[string]{Path: wf.PathTemplate("{records}/spec.json#/items/0"), Hash: "hash"},
						WorkspaceRef: wf.Ref[string]{Path: "spec.md", Hash: "workspace-hash"},
						Template:     wf.PathTemplate("{records}/report.md"), Plain: "{records}/ordinary.md",
					}, nil
				})
				done := wf.NewTerminal[tracePathDone](d, wf.Success)
				compute.Then(compute.To(done, "done"))
				workspace, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				var opts []Option
				if withTB {
					opts = append(opts, WithTB(t))
				}
				if override {
					workspace = t.TempDir()
					opts = append(opts, WithWorkspace(workspace))
				}
				trace := New(d, opts...).Run(Resume())
				records := trace.Journal()
				before, err := json.Marshal(records)
				if err != nil {
					t.Fatal(err)
				}
				var raw json.RawMessage
				for _, record := range records {
					if record.Kind != wf.JournalAccepted {
						continue
					}
					var accepted wf.AcceptedData
					if err := json.Unmarshal(record.Data, &accepted); err != nil {
						t.Fatal(err)
					}
					if accepted.Computed {
						raw = accepted.Output
						break
					}
				}
				if len(raw) == 0 || strings.Count(string(raw), `"$wf.path"`) != 3 {
					t.Fatalf("computed output lacks typed paths: %s", raw)
				}
				originalOutput := string(raw)
				var result tracePathResult
				if err := trace.DecodeValue(raw, &result); err != nil {
					t.Fatal(err)
				}
				if result.Ref.Path != filepath.Join(trace.runDir, "records", "spec.json")+"#/items/0" || result.Ref.Hash != "hash" {
					t.Fatalf("run ref: %+v", result.Ref)
				}
				if result.WorkspaceRef.Path != filepath.Join(workspace, "spec.md") || result.WorkspaceRef.Hash != "workspace-hash" {
					t.Fatalf("workspace ref: %+v", result.WorkspaceRef)
				}
				if result.Template != filepath.Join(trace.runDir, "records", "report.md") || result.Plain != "{records}/ordinary.md" {
					t.Fatalf("strings: %+v", result)
				}
				if _, err := os.Stat(trace.runDir); withTB && err != nil || !withTB && !os.IsNotExist(err) {
					t.Fatalf("run directory lifecycle withTB=%t: %v", withTB, err)
				}
				after, err := json.Marshal(trace.Journal())
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) || string(raw) != originalOutput {
					t.Fatal("DecodeValue mutated durable journal")
				}
				var accepted wf.AcceptedData
				for _, record := range records {
					if record.Kind != wf.JournalAccepted {
						continue
					}
					if err := trace.DecodeValue(record.Data, &accepted); err != nil {
						t.Fatal(err)
					}
					if accepted.Computed {
						break
					}
				}
				var nested tracePathResult
				if err := json.Unmarshal(accepted.Output, &nested); err != nil || nested.Ref.Path != result.Ref.Path {
					t.Fatalf("whole payload decode: %+v %v", nested, err)
				}
			})
		}
	}
}
