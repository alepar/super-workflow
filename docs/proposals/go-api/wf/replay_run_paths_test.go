package wf_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/alepar/super-workflow/docs/proposals/go-api/wf"
)

func TestReplayWithLegacyRunPaths(t *testing.T) {
	for _, rootKind := range []string{"workspace", "state"} {
		for _, action := range []string{"submit", "validate"} {
			t.Run(rootKind+"/"+action, func(t *testing.T) {
				workspace := t.TempDir()
				state := filepath.Join(workspace, "state")
				d := resultPathsDefinition()
				opts := wf.EngineOptions{Workspace: workspace, StateRoot: state}
				h := newHarnessOpts(t, d, nil, opts)
				run := h.run(d, nil)
				in, _ := startedUnit(t, h, run, "agent")
				if _, err := h.e.Done(run, in.Name, wf.Submission{Output: json.RawMessage(`{"Text":"content"}`)}); err != nil {
					t.Fatal(err)
				}
				root := workspace
				if rootKind == "state" {
					root = state
				}
				legacy, _ := json.Marshal(resultPathOutput{Text: filepath.Join(root, "spec.md")})
				// Model a legacy journal: these bytes were accepted before the live guard.
				var records []wf.JournalRecord
				for _, rec := range h.records(run) {
					switch rec.Kind {
					case wf.JournalRunStarted:
						var data wf.RunStartedData
						if err := json.Unmarshal(rec.Data, &data); err != nil {
							t.Fatal(err)
						}
						data.Workspace = workspace
						rec.Data, _ = json.Marshal(data)
					case wf.JournalSubmitted:
						var data wf.SubmittedData
						if err := json.Unmarshal(rec.Data, &data); err != nil {
							t.Fatal(err)
						}
						data.Output = legacy
						rec.Data, _ = json.Marshal(data)
					case wf.JournalAccepted:
						var data wf.AcceptedData
						if err := json.Unmarshal(rec.Data, &data); err != nil {
							t.Fatal(err)
						}
						data.Output = legacy
						rec.Data, _ = json.Marshal(data)
					}
					records = append(records, rec)
					if rec.Kind == wf.JournalSubmitted && action == "validate" {
						refused := rec
						refused.Kind = wf.JournalSubmissionRefused
						refused.Data, _ = json.Marshal(wf.SubmissionRefusedData{Reasons: []string{"old check"}})
						records = append(records, refused)
					}
				}
				for i := range records {
					records[i].Seq = int64(i + 1)
				}
				before, _ := json.Marshal(records)
				report, err := wf.ReplayWith(records, d, "", wf.ReplayOptions{})
				if err != nil || report.Stop.Kind != "ended" {
					t.Fatalf("legacy replay: %v\n%s", err, report)
				}
				after, _ := json.Marshal(records)
				if string(before) != string(after) {
					t.Fatal("replay changed source journal")
				}
				// The exemption belongs to the private replay engine, never live engines.
				live := newHarnessOpts(t, d, nil, opts)
				liveRun := live.run(d, nil)
				unit, _ := startedUnit(t, live, liveRun, "agent")
				if _, err := live.e.Validate(liveRun, unit.Name, wf.Submission{Output: legacy}); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("live validation: %v", err)
				}
				if _, err := live.e.Done(liveRun, unit.Name, wf.Submission{Output: legacy}); !errors.Is(err, wf.ErrRejected) {
					t.Fatalf("live done: %v", err)
				}
			})
		}
	}
}
